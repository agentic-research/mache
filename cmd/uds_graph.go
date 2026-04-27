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

func (g *udsGraph) ListChildren(id string) ([]string, error) {
	id = graph.NormalizeID(id)

	resp, err := g.sock.SendOp(map[string]any{
		"op": "list_children",
		"id": id,
	})
	if err != nil {
		return nil, err
	}

	rawChildren, _ := resp["children"].([]any)
	children := make([]string, 0, len(rawChildren))
	for _, c := range rawChildren {
		if s, ok := c.(string); ok {
			children = append(children, s)
		}
	}
	return children, nil
}

func (g *udsGraph) ListChildStats(id string) ([]graph.NodeStat, error) {
	id = graph.NormalizeID(id)

	resp, err := g.sock.SendOp(map[string]any{
		"op": "list_children",
		"id": id,
	})
	if err != nil {
		return nil, err
	}

	// list_children returns IDs. We need stats. Use get_node for each.
	// TODO: add a list_child_stats op to the daemon for efficiency.
	rawChildren, _ := resp["children"].([]any)
	stats := make([]graph.NodeStat, 0, len(rawChildren))
	for _, c := range rawChildren {
		childID, ok := c.(string)
		if !ok {
			continue
		}
		n, err := g.GetNode(childID)
		if err != nil {
			continue
		}
		stats = append(stats, graph.NodeStat{
			ID:          n.ID,
			IsDir:       n.Mode.IsDir(),
			ContentSize: n.ContentSize(),
			ModTime:     n.ModTime,
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
