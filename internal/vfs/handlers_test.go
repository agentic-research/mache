package vfs

import (
	"sync"
	"testing"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSchemaHandler(t *testing.T) {
	h := &SchemaHandler{Content: []byte(`{"name":"test"}`)}

	assert.True(t, h.Match("/_schema.json"))
	assert.False(t, h.Match("/other"))

	e := h.Stat("/_schema.json")
	require.NotNil(t, e)
	assert.Equal(t, KindFile, e.Kind)
	assert.Equal(t, int64(15), e.Size)
	assert.Equal(t, uint32(0o444), e.Perm)

	data, ok := h.ReadContent("/_schema.json")
	assert.True(t, ok)
	assert.Equal(t, []byte(`{"name":"test"}`), data)

	extras := h.DirExtras("/", nil)
	require.Len(t, extras, 1)
	assert.Equal(t, "_schema.json", extras[0].Name)

	assert.Nil(t, h.DirExtras("/sub", nil))
}

func TestPromptHandler_Empty(t *testing.T) {
	h := &PromptHandler{}
	assert.False(t, h.Match("/PROMPT.txt"))
	assert.Nil(t, h.Stat("/PROMPT.txt"))
	assert.Nil(t, h.DirExtras("/", nil))
}

func TestPromptHandler_WithContent(t *testing.T) {
	h := &PromptHandler{Content: []byte("hello agent")}

	assert.True(t, h.Match("/PROMPT.txt"))
	assert.False(t, h.Match("/other"))

	e := h.Stat("/PROMPT.txt")
	require.NotNil(t, e)
	assert.Equal(t, KindFile, e.Kind)
	assert.Equal(t, int64(11), e.Size)

	data, ok := h.ReadContent("/PROMPT.txt")
	assert.True(t, ok)
	assert.Equal(t, []byte("hello agent"), data)

	extras := h.DirExtras("/", nil)
	require.Len(t, extras, 1)
	assert.Equal(t, "PROMPT.txt", extras[0].Name)
}

func TestDiagnosticsHandler_Disabled(t *testing.T) {
	h := &DiagnosticsHandler{Writable: false, DiagStatus: &sync.Map{}}
	assert.False(t, h.Match("/foo/_diagnostics"))
}

func TestDiagnosticsHandler_DirStat(t *testing.T) {
	h := &DiagnosticsHandler{Writable: true, DiagStatus: &sync.Map{}}

	assert.True(t, h.Match("/foo/_diagnostics"))

	e := h.Stat("/foo/_diagnostics")
	require.NotNil(t, e)
	assert.Equal(t, KindDir, e.Kind)
}

func TestDiagnosticsHandler_FileStat(t *testing.T) {
	ds := &sync.Map{}
	ds.Store("/foo", "ok")
	h := &DiagnosticsHandler{Writable: true, DiagStatus: ds}

	e := h.Stat("/foo/_diagnostics/last-write-status")
	require.NotNil(t, e)
	assert.Equal(t, KindFile, e.Kind)
	assert.Equal(t, []byte("ok\n"), e.Content)

	e = h.Stat("/foo/_diagnostics/ast-errors")
	require.NotNil(t, e)
	assert.Equal(t, []byte("no errors\n"), e.Content)
}

func TestDiagnosticsHandler_Lint(t *testing.T) {
	ds := &sync.Map{}
	ds.Store("/foo/lint", "warning: unused var\n")
	h := &DiagnosticsHandler{Writable: true, DiagStatus: ds}

	e := h.Stat("/foo/_diagnostics/lint")
	require.NotNil(t, e)
	assert.Equal(t, []byte("warning: unused var\n"), e.Content)
}

func TestDiagnosticsHandler_ListDir(t *testing.T) {
	h := &DiagnosticsHandler{Writable: true, DiagStatus: &sync.Map{}}

	entries, ok := h.ListDir("/foo/_diagnostics")
	assert.True(t, ok)
	require.Len(t, entries, 3)
	assert.Equal(t, "last-write-status", entries[0].Name)
	assert.Equal(t, "ast-errors", entries[1].Name)
	assert.Equal(t, "lint", entries[2].Name)
}

func TestDiagnosticsHandler_DirExtras(t *testing.T) {
	h := &DiagnosticsHandler{Writable: true, DiagStatus: &sync.Map{}}

	extras := h.DirExtras("/foo", nil)
	require.Len(t, extras, 1)
	assert.Equal(t, "_diagnostics", extras[0].Name)
	assert.Equal(t, KindDir, extras[0].Kind)

	// Root should not get _diagnostics
	assert.Nil(t, h.DirExtras("/", nil))
}

func TestContextHandler(t *testing.T) {
	store := graph.NewMemoryStore()
	store.AddNode(&graph.Node{ID: "pkg/Foo", Mode: 0o40000, Context: []byte("import context")})

	h := &ContextHandler{Graph: store}

	assert.True(t, h.Match("/pkg/Foo/context"))
	assert.False(t, h.Match("/pkg/Foo/source"))

	e := h.Stat("/pkg/Foo/context")
	require.NotNil(t, e)
	assert.Equal(t, KindFile, e.Kind)
	assert.Equal(t, int64(14), e.Size)

	data, ok := h.ReadContent("/pkg/Foo/context")
	assert.True(t, ok)
	assert.Equal(t, []byte("import context"), data)

	// No context → nil
	store.AddNode(&graph.Node{ID: "pkg/Bar", Mode: 0o40000})
	assert.Nil(t, h.Stat("/pkg/Bar/context"))
}

func TestContextHandler_DirExtras(t *testing.T) {
	h := &ContextHandler{Graph: graph.NewMemoryStore()}

	node := &graph.Node{Context: []byte("ctx")}
	extras := h.DirExtras("/pkg/Foo", node)
	require.Len(t, extras, 1)
	assert.Equal(t, "context", extras[0].Name)

	assert.Nil(t, h.DirExtras("/pkg/Foo", &graph.Node{}))
	assert.Nil(t, h.DirExtras("/pkg/Foo", nil))
}

func TestQueryHandler(t *testing.T) {
	h := &QueryHandler{Enabled: false}
	assert.False(t, h.Match("/.query"))

	h.Enabled = true
	assert.True(t, h.Match("/.query"))
	assert.True(t, h.Match("/.query/foo"))
	assert.False(t, h.Match("/other"))

	// Stat always returns nil (backend handles it)
	assert.Nil(t, h.Stat("/.query"))

	extras := h.DirExtras("/", nil)
	require.Len(t, extras, 1)
	assert.Equal(t, ".query", extras[0].Name)
	assert.Equal(t, KindDir, extras[0].Kind)

	assert.Nil(t, h.DirExtras("/sub", nil))
}

func TestCallersHandler(t *testing.T) {
	store := graph.NewMemoryStore()
	store.AddNode(&graph.Node{ID: "funcs/Foo", Mode: 0o40000})
	store.AddNode(&graph.Node{ID: "funcs/Bar", Mode: 0o40000})
	store.AddNode(&graph.Node{ID: "funcs/Bar/source", Mode: 0, Data: []byte("bar code")})
	require.NoError(t, store.AddRef("Foo", "funcs/Bar"))

	h := &CallersHandler{Graph: store}

	assert.True(t, h.Match("/funcs/Foo/callers"))
	assert.True(t, h.Match("/funcs/Foo/callers/funcs_Bar"))
	assert.False(t, h.Match("/funcs/Foo/source"))

	// Dir stat
	e := h.Stat("/funcs/Foo/callers")
	require.NotNil(t, e)
	assert.Equal(t, KindDir, e.Kind)

	// Entry stat → symlink
	e = h.Stat("/funcs/Foo/callers/funcs_Bar")
	require.NotNil(t, e)
	assert.Equal(t, KindSymlink, e.Kind)
	assert.Equal(t, "funcs/Bar", e.NodeID)
	assert.Contains(t, string(e.Content), "../")

	// Non-existent entry
	assert.Nil(t, h.Stat("/funcs/Foo/callers/nope"))

	// ListDir
	entries, ok := h.ListDir("/funcs/Foo/callers")
	assert.True(t, ok)
	require.Len(t, entries, 1)
	assert.Equal(t, "funcs_Bar", entries[0].Name)

	// DirExtras
	extras := h.DirExtras("/funcs/Foo", nil)
	require.Len(t, extras, 1)
	assert.Equal(t, "callers", extras[0].Name)

	// No callers → no extras
	assert.Nil(t, h.DirExtras("/funcs/Bar", nil))
	assert.Nil(t, h.DirExtras("/", nil))
}

func TestCalleesHandler(t *testing.T) {
	store := graph.NewMemoryStore()
	store.AddNode(&graph.Node{ID: "funcs/Foo", Mode: 0o40000})
	store.AddNode(&graph.Node{ID: "funcs/Bar", Mode: 0o40000})
	store.AddNode(&graph.Node{ID: "funcs/Bar/source", Mode: 0, Data: []byte("bar code")})

	// Foo calls Bar — set up via GetCallees mock
	// GetCallees uses the extractor, so we need a simpler approach.
	// Instead, test the handler with a graph that returns callees.
	// MemoryStore.GetCallees requires an extractor — skip for unit test.
	// Just test Match, DirExtras with no callees.
	h := &CalleesHandler{Graph: store}

	assert.True(t, h.Match("/funcs/Foo/callees"))
	assert.True(t, h.Match("/funcs/Foo/callees/funcs_Bar_source"))
	assert.False(t, h.Match("/funcs/Foo/source"))

	// No callees → nil stat
	assert.Nil(t, h.Stat("/funcs/Foo/callees"))

	// DirExtras with no callees → nil
	assert.Nil(t, h.DirExtras("/funcs/Foo", nil))
	assert.Nil(t, h.DirExtras("/", nil))
}
