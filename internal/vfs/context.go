package vfs

import (
	"path/filepath"
	"strings"

	"github.com/agentic-research/mache/internal/graph"
)

// ContextHandler serves the virtual "context" file inside directory nodes.
type ContextHandler struct {
	Graph graph.Graph
}

func (h *ContextHandler) Match(path string) bool {
	return strings.HasSuffix(path, "/"+graph.ContextFile)
}

func (h *ContextHandler) Stat(path string) *VEntry {
	parentDir := filepath.Dir(path)
	node, err := h.Graph.GetNode(parentDir)
	if err != nil || len(node.Context) == 0 {
		return nil
	}
	return &VEntry{
		Kind:    KindFile,
		Size:    int64(len(node.Context)),
		Perm:    0o444,
		Content: node.Context,
	}
}

func (h *ContextHandler) ReadContent(path string) ([]byte, bool) {
	parentDir := filepath.Dir(path)
	node, err := h.Graph.GetNode(parentDir)
	if err != nil || len(node.Context) == 0 {
		return nil, false
	}
	return node.Context, true
}

func (h *ContextHandler) ListDir(_ string) ([]DirExtra, bool) {
	return nil, false
}

func (h *ContextHandler) DirExtras(parentPath string, node *graph.Node) []DirExtra {
	if node != nil && len(node.Context) > 0 {
		return []DirExtra{{
			Name: graph.ContextFile,
			Kind: KindFile,
			Size: int64(len(node.Context)),
			Perm: 0o444,
		}}
	}
	return nil
}
