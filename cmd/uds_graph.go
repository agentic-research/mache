package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/leyline"
)

// udsGraph implements graph.Graph by sending structured ops over a UDS socket
// to the ley-line daemon. The daemon runs queries against the arena buffer
// (zero-copy via sqlite3_deserialize). Mache never opens SQLite directly.
type udsGraph struct {
	sock *leyline.SocketClient
}

func newUDSGraph(sockPath string) (*udsGraph, error) {
	sock, err := leyline.DialSocket(sockPath)
	if err != nil {
		return nil, fmt.Errorf("connect to daemon: %w", err)
	}
	return &udsGraph{sock: sock}, nil
}

func (g *udsGraph) GetNode(id string) (*graph.Node, error) {
	id = graph.NormalizeID(id)
	if id == "" {
		return &graph.Node{ID: "", Mode: os.ModeDir | 0o555}, nil
	}

	var resp leyline.GetNodeResponse
	if err := g.sock.SendOpInto(map[string]any{
		"op": "get_node",
		"id": id,
	}, &resp); err != nil {
		if daemonNotFound(err) {
			return nil, graph.ErrNotFound
		}
		return nil, err
	}
	if !boolVal(resp.OK) || resp.Node == nil {
		return nil, graph.ErrNotFound
	}

	kind := int32Val(resp.Node.Kind)
	size := int64Val(resp.Node.Size)

	mode := os.FileMode(0o444)
	if int(kind) == graph.NodeKindDir {
		mode = os.ModeDir | 0o555
	}

	node := &graph.Node{
		ID:   id,
		Mode: mode,
	}
	if int(kind) == graph.NodeKindFile {
		node.Ref = &graph.ContentRef{ContentLen: size}
	}
	if resp.Node.Record != nil && *resp.Node.Record != "" {
		node.Data = []byte(*resp.Node.Record)
	}

	return node, nil
}

// listChildren issues the daemon's `list_children` op. The daemon returns
// `[{id, name, kind, size}, ...]` per child (single-shot stats, see
// rs/ll-open/cli-lib/src/daemon/ops.rs::op_list_children) — not bare ID
// strings. ListChildren and ListChildStats both consume the same payload;
// this helper centralizes the JSON decode + error envelope so the two stay
// in lock-step and don't misread a daemon error as an empty directory.
func (g *udsGraph) listChildren(id string) ([]leyline.Node, error) {
	var resp leyline.ListChildrenResponse
	if err := g.sock.SendOpInto(map[string]any{
		"op": "list_children",
		"id": id,
	}, &resp); err != nil {
		if daemonNotFound(err) {
			return nil, graph.ErrNotFound
		}
		return nil, err
	}
	// Daemon signals failure via {"ok": false, "error": "..."}. Match the
	// shape SQLiteGraph uses — missing-node returns graph.ErrNotFound so
	// readdir on a stale ID surfaces cleanly instead of looking like a
	// successful read of an empty dir.
	if !boolVal(resp.OK) {
		return nil, fmt.Errorf("list_children %q: daemon returned ok=false", id)
	}
	return resp.Children, nil
}

func (g *udsGraph) ListChildren(id string) ([]string, error) {
	id = graph.NormalizeID(id)
	children, err := g.listChildren(id)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(children))
	for i := range children {
		if s := strVal(children[i].ID); s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

func (g *udsGraph) ListChildStats(id string) ([]graph.NodeStat, error) {
	id = graph.NormalizeID(id)
	children, err := g.listChildren(id)
	if err != nil {
		return nil, err
	}
	stats := make([]graph.NodeStat, 0, len(children))
	for i := range children {
		cid := strVal(children[i].ID)
		if cid == "" {
			continue
		}
		stats = append(stats, graph.NodeStat{
			ID:          cid,
			IsDir:       int(int32Val(children[i].Kind)) == graph.NodeKindDir,
			ContentSize: int64Val(children[i].Size),
		})
	}
	return stats, nil
}

func (g *udsGraph) ReadContent(id string, buf []byte, offset int64) (int, error) {
	id = graph.NormalizeID(id)

	var resp leyline.ReadContentResponse
	if err := g.sock.SendOpInto(map[string]any{
		"op": "read_content",
		"id": id,
	}, &resp); err != nil {
		if daemonNotFound(err) {
			return 0, graph.ErrNotFound
		}
		return 0, err
	}
	if !boolVal(resp.OK) {
		return 0, graph.ErrNotFound
	}

	return graph.SliceContent([]byte(strVal(resp.Content)), buf, offset), nil
}

func (g *udsGraph) GetCallers(token string) ([]*graph.Node, error) {
	var resp leyline.FindCallersResponse
	if err := g.sock.SendOpInto(map[string]any{
		"op":    "find_callers",
		"token": token,
	}, &resp); err != nil {
		return nil, err
	}

	nodes := make([]*graph.Node, 0, len(resp.Callers))
	for i := range resp.Callers {
		nodeID := strVal(resp.Callers[i].NodeID)
		if nodeID != "" {
			nodes = append(nodes, &graph.Node{
				ID:   nodeID,
				Mode: 0o444,
			})
		}
	}
	return nodes, nil
}

// GetCallees resolves a construct's forward references to their
// definitions via the daemon's `find_callees` op (added in LLO 0.2.2 /
// daemon op_find_callees). The daemon executes
//
//	SELECT DISTINCT d.node_id, d.source_id
//	FROM node_refs r JOIN node_defs d ON r.token = d.token
//	WHERE r.node_id = ?
//
// and returns `{callees: [{node_id, source_id}, ...]}`. Unlike
// SQLiteGraph's client-side extractor (tree-sitter on source bytes),
// this path is purely SQL — no CGO grammar walk required.
func (g *udsGraph) GetCallees(id string) ([]*graph.Node, error) {
	id = graph.NormalizeID(id)
	if id == "" {
		return nil, nil
	}

	var resp leyline.FindCalleesResponse
	if err := g.sock.SendOpInto(map[string]any{
		"op": "find_callees",
		"id": id,
	}, &resp); err != nil {
		if daemonNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if !boolVal(resp.OK) {
		// Match the "no callees found" semantics other backends use —
		// missing-node or empty-result is not an error condition for
		// this query, just an empty edge set.
		return nil, nil
	}

	nodes := make([]*graph.Node, 0, len(resp.Callees))
	// Compare in normalized form so daemon-returned "/pkg/X" matches our
	// already-normalized input id "pkg/X" — otherwise the self-edge slips
	// past and `find_callees(F)` lists F itself.
	seen := map[string]bool{id: true}
	for i := range resp.Callees {
		nodeID := strVal(resp.Callees[i].NodeID)
		if nodeID == "" {
			continue
		}
		key := graph.NormalizeID(nodeID)
		if seen[key] {
			continue
		}
		seen[key] = true
		nodes = append(nodes, &graph.Node{
			ID:   nodeID,
			Mode: os.ModeDir | 0o555,
		})
	}
	return nodes, nil
}

func (g *udsGraph) Invalidate(id string) {}

func (g *udsGraph) Act(id, action, payload string) (*graph.ActionResult, error) {
	return nil, graph.ErrActNotSupported
}

func (g *udsGraph) Close() error {
	return g.sock.Close()
}

var _ graph.Graph = (*udsGraph)(nil)

// --- pointer-deref helpers ---
//
// The typed wire structs use `*T` for every field to model capnp's
// "field is set" vs "field is unset" distinction. These helpers collapse
// that into Go zero-values at the consumer boundary where we don't need
// to distinguish nil from zero.

func boolVal(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

func strVal(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func int32Val(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}

func int64Val(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// daemonNotFound recognizes an explicit ley-line error envelope without
// conflating transport or typed-decode failures with a missing graph node.
func daemonNotFound(err error) bool {
	var daemonErr *leyline.DaemonResponseError
	return errors.As(err, &daemonErr) && strings.Contains(strings.ToLower(daemonErr.Message), "not found")
}
