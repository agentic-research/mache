package vfs

import (
	"fmt"
	"sync"
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

// TestNewDefaultResolver pins the chain shape that mounts depend on:
// the standard handler set, in registration order, with typed
// references for post-construction configuration. The order matters
// because Resolve uses first-match-wins — _schema.json before paths
// that could shadow it, callers/callees last so they fall through to
// the graph lookup when no virtual entry matches.
func TestNewDefaultResolver_ChainShape(t *testing.T) {
	g := graph.NewMemoryStore()
	r := NewDefaultResolver(g, []byte(`{"version":"v1"}`))
	require.NotNil(t, r)

	// Seven handlers in this order: schema, prompt, diag, context,
	// location, callers, callees (one for each * Handler type below
	// the SchemaHandler).
	require.Len(t, r.handlers, 7, "default chain length is fixed at 7")
	wantTypes := []string{
		"*vfs.SchemaHandler",
		"*vfs.PromptHandler",
		"*vfs.DiagnosticsHandler",
		"*vfs.ContextHandler",
		"*vfs.LocationHandler",
		"*vfs.CallersHandler",
		"*vfs.CalleesHandler",
	}
	for i, h := range r.handlers {
		// fmt.Sprintf("%T", ...) renders the concrete type; checking
		// it pins the chain shape without committing the test to the
		// internal layout of each handler struct.
		assert.Equal(t, wantTypes[i], typeName(h),
			"handler[%d] should be %s", i, wantTypes[i])
	}

	// Typed references are wired so SetPromptContent/SetWritable
	// reach the right handler instance.
	require.NotNil(t, r.promptH, "promptH typed ref must be wired")
	require.NotNil(t, r.diagH, "diagH typed ref must be wired")
}

func TestNewDefaultResolver_SetPromptContent(t *testing.T) {
	r := NewDefaultResolver(graph.NewMemoryStore(), nil)
	r.SetPromptContent([]byte("hello"))
	assert.Equal(t, []byte("hello"), r.promptH.Content,
		"SetPromptContent must mutate the typed-ref handler")
}

func TestNewDefaultResolver_SetWritable(t *testing.T) {
	r := NewDefaultResolver(graph.NewMemoryStore(), nil)

	// Default: not writable.
	assert.False(t, r.diagH.Writable)

	// Enable writable + supply a fresh diagStatus map.
	var diagStatus sync.Map
	r.SetWritable(true, &diagStatus)
	assert.True(t, r.diagH.Writable)
	assert.Same(t, &diagStatus, r.diagH.DiagStatus,
		"diag handler should hold the caller-provided status map")

	// SetWritable(_, nil) preserves the existing status map.
	r.SetWritable(false, nil)
	assert.False(t, r.diagH.Writable)
	assert.Same(t, &diagStatus, r.diagH.DiagStatus,
		"nil diagStatus must not stomp the previously-set map")
}

// typeName returns "*vfs.FooHandler" for h. Used to pin the chain
// shape without depending on each handler's internal layout.
func typeName(h any) string {
	return fmt.Sprintf("%T", h)
}
