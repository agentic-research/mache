package vfs

import (
	"testing"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubHandler matches a single path and returns fixed VEntry/content.
type stubHandler struct {
	path    string
	entry   *VEntry
	content []byte
	dirList []DirExtra
	extras  []DirExtra
}

func (s *stubHandler) Match(path string) bool                       { return path == s.path }
func (s *stubHandler) Stat(_ string) *VEntry                        { return s.entry }
func (s *stubHandler) ReadContent(_ string) ([]byte, bool)          { return s.content, s.content != nil }
func (s *stubHandler) ListDir(_ string) ([]DirExtra, bool)          { return s.dirList, s.dirList != nil }
func (s *stubHandler) DirExtras(_ string, _ *graph.Node) []DirExtra { return s.extras }

func TestResolver_Resolve(t *testing.T) {
	h1 := &stubHandler{path: "/a", entry: &VEntry{Kind: KindFile, Size: 10}}
	h2 := &stubHandler{path: "/b", entry: &VEntry{Kind: KindDir}}
	r := NewResolver(h1, h2)

	e := r.Resolve("/a")
	require.NotNil(t, e)
	assert.Equal(t, KindFile, e.Kind)
	assert.Equal(t, int64(10), e.Size)

	e = r.Resolve("/b")
	require.NotNil(t, e)
	assert.Equal(t, KindDir, e.Kind)

	assert.Nil(t, r.Resolve("/c"))
}

func TestResolver_Match(t *testing.T) {
	h := &stubHandler{path: "/x"}
	r := NewResolver(h)

	assert.True(t, r.Match("/x"))
	assert.False(t, r.Match("/y"))
}

func TestResolver_ReadContent(t *testing.T) {
	h := &stubHandler{path: "/f", content: []byte("hello")}
	r := NewResolver(h)

	data, ok := r.ReadContent("/f")
	assert.True(t, ok)
	assert.Equal(t, []byte("hello"), data)

	_, ok = r.ReadContent("/nope")
	assert.False(t, ok)
}

func TestResolver_DirExtras_CollectsAll(t *testing.T) {
	h1 := &stubHandler{path: "/a", extras: []DirExtra{{Name: "x"}}}
	h2 := &stubHandler{path: "/b", extras: []DirExtra{{Name: "y"}, {Name: "z"}}}
	r := NewResolver(h1, h2)

	extras := r.DirExtras("/parent", nil)
	assert.Len(t, extras, 3)
	assert.Equal(t, "x", extras[0].Name)
	assert.Equal(t, "y", extras[1].Name)
	assert.Equal(t, "z", extras[2].Name)
}

func TestResolver_FirstMatchWins(t *testing.T) {
	h1 := &stubHandler{path: "/dup", entry: &VEntry{Kind: KindFile, Size: 1}}
	h2 := &stubHandler{path: "/dup", entry: &VEntry{Kind: KindFile, Size: 2}}
	r := NewResolver(h1, h2)

	e := r.Resolve("/dup")
	require.NotNil(t, e)
	assert.Equal(t, int64(1), e.Size) // h1 wins
}
