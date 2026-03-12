package vfs

import "github.com/agentic-research/mache/internal/graph"

// SchemaHandler serves the /_schema.json virtual file.
type SchemaHandler struct {
	Content []byte // Serialized schema JSON
}

func (h *SchemaHandler) Match(path string) bool {
	return path == "/"+graph.SchemaDotJSON
}

func (h *SchemaHandler) Stat(path string) *VEntry {
	return &VEntry{
		Kind:    KindFile,
		Size:    int64(len(h.Content)),
		Perm:    0o444,
		Content: h.Content,
	}
}

func (h *SchemaHandler) ReadContent(path string) ([]byte, bool) {
	return h.Content, true
}

func (h *SchemaHandler) ListDir(_ string) ([]DirExtra, bool) {
	return nil, false
}

func (h *SchemaHandler) DirExtras(parentPath string, _ *graph.Node) []DirExtra {
	if parentPath == "/" {
		return []DirExtra{{
			Name: graph.SchemaDotJSON,
			Kind: KindFile,
			Size: int64(len(h.Content)),
			Perm: 0o444,
		}}
	}
	return nil
}
