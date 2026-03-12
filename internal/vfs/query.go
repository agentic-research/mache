package vfs

import (
	"strings"

	"github.com/agentic-research/mache/internal/graph"
)

// QueryHandler is a thin wrapper for the /.query/ magic directory.
// It only handles Match and DirExtras — the stateful lifecycle
// (Mkdir, Write, Release, queryExecute) stays in the backend.
type QueryHandler struct {
	Enabled bool // true when queryFn is set
}

func (h *QueryHandler) Match(path string) bool {
	return h.Enabled && (path == "/.query" || strings.HasPrefix(path, "/.query/"))
}

func (h *QueryHandler) Stat(_ string) *VEntry {
	// Stat is handled by the backend's queryGetattr.
	return nil
}

func (h *QueryHandler) ReadContent(_ string) ([]byte, bool) {
	return nil, false
}

func (h *QueryHandler) ListDir(_ string) ([]DirExtra, bool) {
	return nil, false
}

func (h *QueryHandler) DirExtras(parentPath string, _ *graph.Node) []DirExtra {
	if h.Enabled && parentPath == "/" {
		return []DirExtra{{
			Name: ".query",
			Kind: KindDir,
			Perm: 0o777,
		}}
	}
	return nil
}
