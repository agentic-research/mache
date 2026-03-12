package vfs

import (
	"path/filepath"
	"strings"

	"github.com/agentic-research/mache/internal/graph"
)

// CallersHandler serves the virtual callers/ directory and its symlink entries.
type CallersHandler struct {
	Graph graph.Graph
}

func (h *CallersHandler) Match(path string) bool {
	return graph.IsCallersPath(path)
}

func (h *CallersHandler) Stat(path string) *VEntry {
	parentDir, entryName := graph.ParseCallersPath(path)
	if parentDir == "/" {
		return nil
	}
	if _, err := h.Graph.GetNode(parentDir); err != nil {
		return nil
	}
	token := filepath.Base(parentDir)
	callers, err := h.Graph.GetCallers(token)
	if err != nil || len(callers) == 0 {
		return nil
	}
	if entryName == "" {
		return &VEntry{Kind: KindDir, Perm: 0o555}
	}
	for _, caller := range callers {
		flatName := strings.ReplaceAll(caller.ID, "/", "_")
		if flatName == entryName {
			target := graph.VDirSymlinkTarget(parentDir, caller.ID)
			return &VEntry{
				Kind:    KindSymlink,
				Size:    int64(len(target)),
				Perm:    0o777,
				Content: []byte(target),
				NodeID:  caller.ID,
			}
		}
	}
	return nil
}

func (h *CallersHandler) ReadContent(path string) ([]byte, bool) {
	// Callers entries are symlinks in FUSE, graphFiles in NFS.
	// Content is the symlink target; NFS uses NodeID instead.
	entry := h.Stat(path)
	if entry == nil || entry.Kind != KindSymlink {
		return nil, false
	}
	return entry.Content, true
}

func (h *CallersHandler) ListDir(path string) ([]DirExtra, bool) {
	parentDir, entryName := graph.ParseCallersPath(path)
	if entryName != "" || parentDir == "/" {
		return nil, false
	}
	if _, err := h.Graph.GetNode(parentDir); err != nil {
		return nil, false
	}
	token := filepath.Base(parentDir)
	callers, err := h.Graph.GetCallers(token)
	if err != nil || len(callers) == 0 {
		return nil, false
	}
	entries := make([]DirExtra, 0, len(callers))
	for _, c := range callers {
		flatName := strings.ReplaceAll(c.ID, "/", "_")
		entries = append(entries, DirExtra{
			Name: flatName,
			Kind: KindSymlink,
			Perm: 0o777,
		})
	}
	return entries, true
}

func (h *CallersHandler) DirExtras(parentPath string, _ *graph.Node) []DirExtra {
	if parentPath == "/" {
		return nil
	}
	token := filepath.Base(parentPath)
	callers, err := h.Graph.GetCallers(token)
	if err != nil || len(callers) == 0 {
		return nil
	}
	return []DirExtra{{
		Name: graph.CallersDir,
		Kind: KindDir,
		Perm: 0o555,
	}}
}
