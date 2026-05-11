package cmd

import (
	"fmt"
	"os"

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
// same payload; this helper centralizes the JSON parse so the two stay
// in lock-step.
func (g *udsGraph) listChildrenResponse(id string) ([]map[string]any, error) {
	resp, err := g.sock.SendOp(map[string]any{
		"op": "list_children",
		"id": id,
	})
	if err != nil {
		return nil, err
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

// GetCallees is not yet implemented on the UDS backend — the daemon's
// `base_op_names` exposes `find_callers` and `find_defs` but no
// `find_callees` op. Consumers of `serve --control` see empty callees
// results today; `mount --control` is gated on this and stays on the
// SQLiteGraph path. Tracked: mache-a5e4ea (mache-side consumer), which
// is gated on LLO Bead B ley-line-open-a47d7d (daemon-side
// `find_callees`). Returning `(nil, nil)` matches the "no callees
// found" semantics of other backends so consumers degrade silently
// rather than erroring out.
func (g *udsGraph) GetCallees(id string) ([]*graph.Node, error) {
	return nil, nil
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
