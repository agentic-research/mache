package vfs

import (
	"sync"

	"github.com/agentic-research/mache/internal/graph"
)

// DiagnosticsHandler serves the /_diagnostics/ virtual directory.
// Requires Writable=true and a DiagStatus sync.Map (shared with MemoryStore.WriteStatus).
type DiagnosticsHandler struct {
	Writable   bool
	DiagStatus *sync.Map // parentDir → status string
}

func (h *DiagnosticsHandler) Match(path string) bool {
	return h.Writable && graph.IsDiagPath(path)
}

func (h *DiagnosticsHandler) Stat(path string) *VEntry {
	parentDir, fileName := graph.ParseDiagPath(path)
	if fileName == "" {
		return &VEntry{Kind: KindDir, Perm: 0o555}
	}
	content, ok := h.diagContent(parentDir, fileName)
	if !ok {
		return nil
	}
	return &VEntry{
		Kind:    KindFile,
		Size:    int64(len(content)),
		Perm:    0o444,
		Content: content,
	}
}

func (h *DiagnosticsHandler) ReadContent(path string) ([]byte, bool) {
	parentDir, fileName := graph.ParseDiagPath(path)
	if fileName == "" {
		return nil, false
	}
	return h.diagContent(parentDir, fileName)
}

func (h *DiagnosticsHandler) ListDir(path string) ([]DirExtra, bool) {
	_, fileName := graph.ParseDiagPath(path)
	if fileName != "" {
		return nil, false // not a directory
	}
	return []DirExtra{
		{Name: graph.DiagLastWrite, Kind: KindFile, Perm: 0o444},
		{Name: graph.DiagASTErrors, Kind: KindFile, Perm: 0o444},
		{Name: graph.DiagLint, Kind: KindFile, Perm: 0o444},
	}, true
}

func (h *DiagnosticsHandler) DirExtras(parentPath string, _ *graph.Node) []DirExtra {
	if h.Writable && parentPath != "/" {
		return []DirExtra{{
			Name: graph.DiagnosticsDir,
			Kind: KindDir,
			Perm: 0o555,
		}}
	}
	return nil
}

// diagContent returns the content of a diagnostics virtual file.
// Unifies the FUSE and NFS implementations, including DiagLint.
func (h *DiagnosticsHandler) diagContent(parentDir, fileName string) ([]byte, bool) {
	switch fileName {
	case graph.DiagLastWrite:
		val, ok := h.DiagStatus.Load(parentDir)
		if !ok {
			return []byte("no writes yet\n"), true
		}
		return []byte(val.(string) + "\n"), true
	case graph.DiagASTErrors:
		val, ok := h.DiagStatus.Load(parentDir)
		if !ok {
			return []byte("no errors\n"), true
		}
		msg := val.(string)
		if msg == "ok" {
			return []byte("no errors\n"), true
		}
		return []byte(msg + "\n"), true
	case graph.DiagLint:
		val, ok := h.DiagStatus.Load(parentDir + "/" + graph.DiagLint)
		if !ok {
			return []byte("clean\n"), true
		}
		return []byte(val.(string)), true
	default:
		return nil, false
	}
}
