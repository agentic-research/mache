package vfs

import (
	"strings"

	"github.com/agentic-research/mache/internal/graph"
)

// CalleesHandler serves the virtual callees/ directory and its symlink entries.
type CalleesHandler struct {
	Graph graph.Graph
}

func (h *CalleesHandler) Match(path string) bool {
	return graph.IsCalleesPath(path)
}

func (h *CalleesHandler) Stat(path string) *VEntry {
	parentDir, entryName := graph.ParseCalleesPath(path)
	if parentDir == "/" {
		return nil
	}
	if _, err := h.Graph.GetNode(parentDir); err != nil {
		return nil
	}
	callees, err := h.Graph.GetCallees(parentDir)
	if err != nil || len(callees) == 0 {
		return nil
	}
	if entryName == "" {
		return &VEntry{Kind: KindDir, Perm: 0o555}
	}
	for _, callee := range callees {
		sourceID := graph.FindSourceChild(h.Graph, callee.ID)
		if sourceID == "" {
			continue
		}
		flatName := strings.ReplaceAll(sourceID, "/", "_")
		if flatName == entryName {
			target := graph.VDirSymlinkTarget(parentDir, sourceID)
			return &VEntry{
				Kind:    KindSymlink,
				Size:    int64(len(target)),
				Perm:    0o777,
				Content: []byte(target),
				NodeID:  sourceID,
			}
		}
	}
	return nil
}

func (h *CalleesHandler) ReadContent(path string) ([]byte, bool) {
	entry := h.Stat(path)
	if entry == nil || entry.Kind != KindSymlink {
		return nil, false
	}
	return entry.Content, true
}

func (h *CalleesHandler) ListDir(path string) ([]DirExtra, bool) {
	parentDir, entryName := graph.ParseCalleesPath(path)
	if entryName != "" || parentDir == "/" {
		return nil, false
	}
	if _, err := h.Graph.GetNode(parentDir); err != nil {
		return nil, false
	}
	callees, err := h.Graph.GetCallees(parentDir)
	if err != nil || len(callees) == 0 {
		return nil, false
	}
	var entries []DirExtra
	for _, c := range callees {
		sourceID := graph.FindSourceChild(h.Graph, c.ID)
		if sourceID != "" {
			flatName := strings.ReplaceAll(sourceID, "/", "_")
			entries = append(entries, DirExtra{
				Name: flatName,
				Kind: KindSymlink,
				Perm: 0o777,
			})
		}
	}
	return entries, true
}

func (h *CalleesHandler) DirExtras(parentPath string, _ *graph.Node) []DirExtra {
	if parentPath == "/" {
		return nil
	}
	callees, err := h.Graph.GetCallees(parentPath)
	if err != nil || len(callees) == 0 {
		return nil
	}
	return []DirExtra{{
		Name: graph.CalleesDir,
		Kind: KindDir,
		Perm: 0o555,
	}}
}
