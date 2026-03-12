package vfs

import "github.com/agentic-research/mache/internal/graph"

// PromptHandler serves the /PROMPT.txt virtual file (agent mode).
// If Content is nil/empty, the handler reports no match.
type PromptHandler struct {
	Content []byte
}

func (h *PromptHandler) Match(path string) bool {
	return len(h.Content) > 0 && path == "/"+graph.PromptFile
}

func (h *PromptHandler) Stat(path string) *VEntry {
	if len(h.Content) == 0 {
		return nil
	}
	return &VEntry{
		Kind:    KindFile,
		Size:    int64(len(h.Content)),
		Perm:    0o444,
		Content: h.Content,
	}
}

func (h *PromptHandler) ReadContent(path string) ([]byte, bool) {
	if len(h.Content) == 0 {
		return nil, false
	}
	return h.Content, true
}

func (h *PromptHandler) ListDir(_ string) ([]DirExtra, bool) {
	return nil, false
}

func (h *PromptHandler) DirExtras(parentPath string, _ *graph.Node) []DirExtra {
	if parentPath == "/" && len(h.Content) > 0 {
		return []DirExtra{{
			Name: graph.PromptFile,
			Kind: KindFile,
			Size: int64(len(h.Content)),
			Perm: 0o444,
		}}
	}
	return nil
}
