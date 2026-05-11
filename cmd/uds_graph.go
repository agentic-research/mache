package cmd

import (
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

	resp, err := g.sock.SendOp(map[string]any{
		"op": "get_node",
		"id": id,
	})
	if err != nil {
		return nil, err
	}
	if ok, _ := resp["ok"].(bool); !ok {
		return nil, graph.ErrNotFound
	}

	nodeData, _ := resp["node"].(map[string]any)
	if nodeData == nil {
		return nil, graph.ErrNotFound
	}

	kind := toInt(nodeData["kind"])
	size := toInt64(nodeData["size"])

	mode := os.FileMode(0o444)
	if kind == graph.NodeKindDir {
		mode = os.ModeDir | 0o555
	}

	node := &graph.Node{
		ID:   id,
		Mode: mode,
	}
	if kind == graph.NodeKindFile {
		node.Ref = &graph.ContentRef{ContentLen: size}
	}

	// Try to get record content for ReadContent
	if record, ok := nodeData["record"].(string); ok && record != "" {
		node.Data = []byte(record)
	}

	return node, nil
}

// listChildrenResponse fetches the daemon's `list_children` response. The
// daemon returns `[{id, name, kind, size}, ...]` per child (single-shot
// stats, see rs/ll-open/cli-lib/src/daemon/ops.rs::op_list_children) —
// not bare ID strings. ListChildren and ListChildStats both consume the
// same payload; this helper centralizes the JSON parse + error envelope
// so the two stay in lock-step and don't misread a daemon error as an
// empty directory.
func (g *udsGraph) listChildrenResponse(id string) ([]map[string]any, error) {
	resp, err := g.sock.SendOp(map[string]any{
		"op": "list_children",
		"id": id,
	})
	if err != nil {
		return nil, err
	}
	// Daemon signals failure via {"ok": false, "error": "..."}. Match the
	// shape SQLiteGraph uses — missing-node returns graph.ErrNotFound so
	// readdir on a stale ID surfaces cleanly instead of looking like a
	// successful read of an empty dir.
	if ok, _ := resp["ok"].(bool); !ok {
		if msg, _ := resp["error"].(string); msg != "" {
			if strings.Contains(strings.ToLower(msg), "not found") {
				return nil, graph.ErrNotFound
			}
			return nil, fmt.Errorf("list_children %q: %s", id, msg)
		}
		return nil, fmt.Errorf("list_children %q: daemon returned ok=false", id)
	}
	raw, _ := resp["children"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, c := range raw {
		if m, ok := c.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

func (g *udsGraph) ListChildren(id string) ([]string, error) {
	id = graph.NormalizeID(id)
	children, err := g.listChildrenResponse(id)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(children))
	for _, c := range children {
		if s, ok := c["id"].(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

func (g *udsGraph) ListChildStats(id string) ([]graph.NodeStat, error) {
	id = graph.NormalizeID(id)
	children, err := g.listChildrenResponse(id)
	if err != nil {
		return nil, err
	}
	stats := make([]graph.NodeStat, 0, len(children))
	for _, c := range children {
		cid, _ := c["id"].(string)
		if cid == "" {
			continue
		}
		stats = append(stats, graph.NodeStat{
			ID:          cid,
			IsDir:       toInt(c["kind"]) == graph.NodeKindDir,
			ContentSize: toInt64(c["size"]),
		})
	}
	return stats, nil
}

func (g *udsGraph) ReadContent(id string, buf []byte, offset int64) (int, error) {
	id = graph.NormalizeID(id)

	resp, err := g.sock.SendOp(map[string]any{
		"op": "read_content",
		"id": id,
	})
	if err != nil {
		return 0, err
	}
	if ok, _ := resp["ok"].(bool); !ok {
		return 0, graph.ErrNotFound
	}

	content, _ := resp["content"].(string)
	return graph.SliceContent([]byte(content), buf, offset), nil
}

func (g *udsGraph) GetCallers(token string) ([]*graph.Node, error) {
	resp, err := g.sock.SendOp(map[string]any{
		"op":    "find_callers",
		"token": token,
	})
	if err != nil {
		return nil, err
	}

	rawCallers, _ := resp["callers"].([]any)
	nodes := make([]*graph.Node, 0, len(rawCallers))
	for _, c := range rawCallers {
		row, ok := c.(map[string]any)
		if !ok {
			continue
		}
		nodeID, _ := row["node_id"].(string)
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

	resp, err := g.sock.SendOp(map[string]any{
		"op": "find_callees",
		"id": id,
	})
	if err != nil {
		return nil, err
	}
	if ok, _ := resp["ok"].(bool); !ok {
		// Match the "no callees found" semantics other backends use —
		// missing-node or empty-result is not an error condition for
		// this query, just an empty edge set.
		return nil, nil
	}

	rawCallees, _ := resp["callees"].([]any)
	nodes := make([]*graph.Node, 0, len(rawCallees))
	// Compare in normalized form so daemon-returned "/pkg/X" matches our
	// already-normalized input id "pkg/X" — otherwise the self-edge slip
	// past and `find_callees(F)` lists F itself.
	seen := map[string]bool{id: true}
	for _, c := range rawCallees {
		row, ok := c.(map[string]any)
		if !ok {
			continue
		}
		nodeID, _ := row["node_id"].(string)
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

// --- helpers ---

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	default:
		return 0
	}
}
