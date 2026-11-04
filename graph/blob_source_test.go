package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestMemoryStore_SetFileBlob_RestampsTheFile: after an accepted write every
// origin in the file names the new blob; nil names none (after a merge).
// Origins in other files are left alone.
func TestMemoryStore_SetFileBlob_RestampsTheFile(t *testing.T) {
	s := NewMemoryStore()
	s.AddNode(&Node{ID: "a/F", Origin: &SourceOrigin{FilePath: "/a.go", EndByte: 5, Blob: []byte("old")}})
	s.AddNode(&Node{ID: "a/G", Origin: &SourceOrigin{FilePath: "/a.go", StartByte: 6, EndByte: 9, Blob: []byte("old")}})
	s.AddNode(&Node{ID: "b/H", Origin: &SourceOrigin{FilePath: "/b.go", EndByte: 3, Blob: []byte("other")}})

	s.SetFileBlob("/a.go", []byte("new"))
	for _, id := range []string{"a/F", "a/G"} {
		n, _ := s.GetNode(id)
		assert.Equal(t, []byte("new"), n.Origin.Blob, id)
	}
	s.SetFileBlob("/a.go", nil)
	n, _ := s.GetNode("a/F")
	assert.Nil(t, n.Origin.Blob)
	h, _ := s.GetNode("b/H")
	assert.Equal(t, []byte("other"), h.Origin.Blob)
}
