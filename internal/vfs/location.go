package vfs

import (
	"path/filepath"
	"strings"

	"github.com/agentic-research/mache/internal/graph"
)

// LocationHandler serves the virtual "location" file inside directory nodes
// that have a Properties["location"] value (e.g., "internal/ingest/engine.go:142:298").
// This bridges the orientation gap between mache's construct-based paths and
// the original source file coordinates.
type LocationHandler struct {
	Graph graph.Graph
}

func (h *LocationHandler) Match(path string) bool {
	return strings.HasSuffix(path, "/"+graph.LocationFile)
}

func (h *LocationHandler) Stat(path string) *VEntry {
	parentDir := filepath.Dir(path)
	node, err := h.Graph.GetNode(parentDir)
	if err != nil {
		return nil
	}
	loc, ok := node.Properties["location"]
	if !ok || len(loc) == 0 {
		return nil
	}
	return &VEntry{
		Kind:    KindFile,
		Size:    int64(len(loc)),
		Perm:    0o444,
		Content: loc,
	}
}

func (h *LocationHandler) ReadContent(path string) ([]byte, bool) {
	parentDir := filepath.Dir(path)
	node, err := h.Graph.GetNode(parentDir)
	if err != nil {
		return nil, false
	}
	loc, ok := node.Properties["location"]
	if !ok || len(loc) == 0 {
		return nil, false
	}
	return loc, true
}

func (h *LocationHandler) ListDir(_ string) ([]DirExtra, bool) {
	return nil, false
}

func (h *LocationHandler) DirExtras(parentPath string, node *graph.Node) []DirExtra {
	if node != nil && node.Properties != nil {
		if loc, ok := node.Properties["location"]; ok && len(loc) > 0 {
			return []DirExtra{{
				Name: graph.LocationFile,
				Kind: KindFile,
				Size: int64(len(loc)),
				Perm: 0o444,
			}}
		}
	}
	return nil
}
