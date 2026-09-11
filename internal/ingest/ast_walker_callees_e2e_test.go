package ingest

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildScopedCalleesASTFixture builds a minimal `_ast` database (the schema
// ley-line produces) with TWO function scopes in the same source file, so a
// scoped query can be proven to actually scope — not just to work when the
// whole file happens to contain a single call:
//
//	pkg.go
//	├── function_declaration    (scope A) -> calls helper.Do()  (qualified)
//	├── function_declaration_1  (scope B) -> calls Other()      (bare)
//	└── functionXdeclaration    (scope C) -> calls Leak()       (bare)
//
// Node ids follow the LLO convention (parent-path + "/" + kind[_N]), so the
// scope-prefix filter in callRows behaves exactly as it would against a real
// ley-line-produced .db. Scope C differs from A only where A has a '_': the
// SQL LIKE that once did this filtering read '_' as a one-character wildcard
// and leaked C's call into A until the prefix was escaped (mache-702f9b);
// the prefix must match literally whatever does the matching.
//
// Each scope owns a disjoint byte range so the file index (start-byte order)
// lists the scopes and their calls in document order, as a real parse would.
func buildScopedCalleesASTFixture(t *testing.T) *sql.DB {
	t.Helper()
	b := fixturedb.New(t, fixturedb.Leyline)
	b.ASTNode("pkg.go", "source_file", "pkg.go", fixturedb.Bytes(0, 300))

	// Scope A: helper.Do() — qualified call (selector_expression shape).
	fn := "pkg.go/function_declaration"
	b.ASTNode(fn, "function_declaration", "pkg.go", fixturedb.Bytes(0, 100))
	b.ASTNode(fn+"/call_expression", "call_expression", "pkg.go", fixturedb.Bytes(10, 30))
	b.ASTNode(fn+"/call_expression/selector_expression", "selector_expression", "pkg.go", fixturedb.Bytes(10, 19), fixturedb.Detail{Field: "function"})
	b.ASTNode(fn+"/call_expression/selector_expression/identifier", "identifier", "pkg.go", fixturedb.Bytes(10, 16), fixturedb.Detail{Token: "helper", Field: "operand"})
	b.ASTNode(fn+"/call_expression/selector_expression/field_identifier", "field_identifier", "pkg.go", fixturedb.Bytes(17, 19), fixturedb.Detail{Token: "Do", Field: "field"})
	b.ASTNode(fn+"/call_expression/argument_list", "argument_list", "pkg.go", fixturedb.Bytes(19, 21), fixturedb.Detail{Field: "arguments"})

	// Scope B: Other() — bare call. Must NOT leak into scope A's results.
	fn = "pkg.go/function_declaration_1"
	b.ASTNode(fn, "function_declaration", "pkg.go", fixturedb.Bytes(100, 200))
	b.ASTNode(fn+"/call_expression", "call_expression", "pkg.go", fixturedb.Bytes(110, 120))
	b.ASTNode(fn+"/call_expression/identifier", "identifier", "pkg.go", fixturedb.Bytes(110, 115), fixturedb.Detail{Token: "Other", Field: "function"})
	b.ASTNode(fn+"/call_expression/argument_list", "argument_list", "pkg.go", fixturedb.Bytes(115, 117), fixturedb.Detail{Field: "arguments"})

	// Scope C: Leak() — id differs from scope A only at A's '_'.
	fn = "pkg.go/functionXdeclaration"
	b.ASTNode(fn, "function_declaration", "pkg.go", fixturedb.Bytes(200, 300))
	b.ASTNode(fn+"/call_expression", "call_expression", "pkg.go", fixturedb.Bytes(210, 220))
	b.ASTNode(fn+"/call_expression/identifier", "identifier", "pkg.go", fixturedb.Bytes(210, 214), fixturedb.Detail{Token: "Leak", Field: "function"})
	b.ASTNode(fn+"/call_expression/argument_list", "argument_list", "pkg.go", fixturedb.Bytes(214, 216), fixturedb.Detail{Field: "arguments"})

	_, f := b.Build()
	return f.DB()
}

// TestASTWalker_ExtractQualifiedCallsScoped_ScopesCorrectly pins the
// building block GetCallees relies on: querying one scope must not see
// another scope's calls — not scope B's, and not scope C's, whose id is one
// wildcard away from A's — and the qualifier must survive.
func TestASTWalker_ExtractQualifiedCallsScoped_ScopesCorrectly(t *testing.T) {
	db := buildScopedCalleesASTFixture(t)
	w := NewASTWalker(db)

	callsA, err := w.ExtractQualifiedCallsScoped("pkg.go", "pkg.go/function_declaration", "go")
	require.NoError(t, err)
	require.Len(t, callsA, 1, "scope A must surface exactly its own call")
	assert.Equal(t, "Do", callsA[0].Token)
	assert.Equal(t, "helper", callsA[0].Qualifier)

	callsB, err := w.ExtractQualifiedCallsScoped("pkg.go", "pkg.go/function_declaration_1", "go")
	require.NoError(t, err)
	require.Len(t, callsB, 1, "scope B must surface exactly its own call")
	assert.Equal(t, "Other", callsB[0].Token)
	assert.Empty(t, callsB[0].Qualifier, "bare call has no qualifier")

	bare, err := w.ExtractCallsScoped("pkg.go", "pkg.go/function_declaration", "go")
	require.NoError(t, err)
	assert.Equal(t, []string{"Do"}, bare, "scope A must not see Leak() from scope C (mache-702f9b)")
}

// TestMemoryStore_GetCallees_ASTScoped is the REAL end-to-end regression test
// for bead mache-fd9982: find_callees was silently broken on the serve/mount
// path because the old wiring fed the GRAPH node id (e.g.
// "cmd/functions/evalOrAbs") into the `_ast` query as if it were the real
// source_id, which matched zero rows every time. This test exercises the
// fixed path with NO regex fallback: a construct's graph node carries the
// ast_source_id/ast_scope_id Properties persisted at projection time
// (engine_walk.go via the ASTScope interface), MemoryStore.GetCallees reads
// them, and the scoped extractor queries the real `_ast` table — the same
// mechanism newASTScopedCallExtractor wires in production.
func TestMemoryStore_GetCallees_ASTScoped(t *testing.T) {
	astDB := buildScopedCalleesASTFixture(t)
	walker := NewASTWalker(astDB)

	store := graph.NewMemoryStore()
	store.SetScopedCallExtractor(walker.ExtractQualifiedCallsScoped)

	store.AddRoot(&graph.Node{ID: "pkg", Mode: os.ModeDir | 0o555})

	// Construct A carries the AST scope mapping for scope A.
	store.AddNode(&graph.Node{
		ID: "pkg/A", Mode: os.ModeDir | 0o555,
		Properties: map[string]json.RawMessage{
			"lang":          []byte(`"go"`),
			"ast_source_id": []byte(`"pkg.go"`),
			"ast_scope_id":  []byte(`"pkg.go/function_declaration"`),
		},
	})
	// Construct B carries the mapping for scope B — a different construct in
	// the same file. Its call must not leak into A's callees.
	store.AddNode(&graph.Node{
		ID: "pkg/B", Mode: os.ModeDir | 0o555,
		Properties: map[string]json.RawMessage{
			"lang":          []byte(`"go"`),
			"ast_source_id": []byte(`"pkg.go"`),
			"ast_scope_id":  []byte(`"pkg.go/function_declaration_1"`),
		},
	})

	// helper.Do resolves via the qualified key "helper.Do" (package.Name).
	store.AddNode(&graph.Node{ID: "helper/Do", Mode: os.ModeDir | 0o555})
	require.NoError(t, store.AddDef("helper.Do", "helper/Do"))
	// Other resolves via the bare key "Other" — present so a scoping leak
	// would be detectable (it would show up in A's results below).
	store.AddNode(&graph.Node{ID: "pkg/Other", Mode: os.ModeDir | 0o555})
	require.NoError(t, store.AddDef("Other", "pkg/Other"))

	t.Run("scope A resolves the qualified call and nothing else", func(t *testing.T) {
		callees, err := store.GetCallees("pkg/A")
		require.NoError(t, err)
		require.NotEmpty(t, callees, "find_callees must resolve helper.Do() — bead mache-fd9982")
		var ids []string
		for _, n := range callees {
			ids = append(ids, n.ID)
		}
		assert.Contains(t, ids, "helper/Do", "qualified call helper.Do() must resolve to its def")
		assert.NotContains(t, ids, "pkg/Other", "scope B's call must not leak into A's callees")
	})

	t.Run("scope B resolves its own bare call and nothing else", func(t *testing.T) {
		callees, err := store.GetCallees("pkg/B")
		require.NoError(t, err)
		require.NotEmpty(t, callees, "find_callees must resolve Other()")
		var ids []string
		for _, n := range callees {
			ids = append(ids, n.ID)
		}
		assert.Contains(t, ids, "pkg/Other")
		assert.NotContains(t, ids, "helper/Do", "scope A's call must not leak into B's callees")
	})
}

// TestSQLiteGraph_GetCallees_ASTScoped is the SQLiteGraph counterpart of
// TestMemoryStore_GetCallees_ASTScoped — same bead, same fix, the other
// backend. The graph .db (nodes/node_defs, built via SQLiteWriter exactly as
// `mache build` does) and the `_ast` db are deliberately SEPARATE files here,
// mirroring the "read-only source" mount.go wiring where the projected index
// carries no `_ast` table and callees resolve through a sibling `_ast` db
// kept open for the mount's lifetime.
func TestSQLiteGraph_GetCallees_ASTScoped(t *testing.T) {
	astDB := buildScopedCalleesASTFixture(t)
	walker := NewASTWalker(astDB)

	graphDBPath := filepath.Join(t.TempDir(), "graph.db")
	w, err := NewSQLiteWriter(graphDBPath)
	require.NoError(t, err)

	w.AddRoot(&graph.Node{ID: "pkg", Mode: os.ModeDir | 0o555})
	w.AddNode(&graph.Node{
		ID: "pkg/A", Mode: os.ModeDir | 0o555,
		Properties: map[string]json.RawMessage{
			"lang":          []byte(`"go"`),
			"ast_source_id": []byte(`"pkg.go"`),
			"ast_scope_id":  []byte(`"pkg.go/function_declaration"`),
		},
	})
	w.AddNode(&graph.Node{
		ID: "pkg/B", Mode: os.ModeDir | 0o555,
		Properties: map[string]json.RawMessage{
			"lang":          []byte(`"go"`),
			"ast_source_id": []byte(`"pkg.go"`),
			"ast_scope_id":  []byte(`"pkg.go/function_declaration_1"`),
		},
	})
	w.AddNode(&graph.Node{ID: "helper/Do", Mode: os.ModeDir | 0o555})
	require.NoError(t, w.AddDef("helper.Do", "helper/Do"))
	w.AddNode(&graph.Node{ID: "pkg/Other", Mode: os.ModeDir | 0o555})
	require.NoError(t, w.AddDef("Other", "pkg/Other"))
	require.NoError(t, w.Close())

	sg, err := graph.OpenSQLiteGraph(graphDBPath, &api.Topology{}, nil)
	require.NoError(t, err)
	defer func() { _ = sg.Close() }()

	sg.SetScopedCallExtractor(walker.ExtractQualifiedCallsScoped)

	t.Run("scope A resolves the qualified call and nothing else", func(t *testing.T) {
		callees, err := sg.GetCallees("pkg/A")
		require.NoError(t, err)
		require.NotEmpty(t, callees, "find_callees must resolve helper.Do() — bead mache-fd9982")
		var ids []string
		for _, n := range callees {
			ids = append(ids, n.ID)
		}
		assert.Contains(t, ids, "helper/Do", "qualified call helper.Do() must resolve to its def")
		assert.NotContains(t, ids, "pkg/Other", "scope B's call must not leak into A's callees")
	})

	t.Run("scope B resolves its own bare call and nothing else", func(t *testing.T) {
		callees, err := sg.GetCallees("pkg/B")
		require.NoError(t, err)
		require.NotEmpty(t, callees, "find_callees must resolve Other()")
		var ids []string
		for _, n := range callees {
			ids = append(ids, n.ID)
		}
		assert.Contains(t, ids, "pkg/Other")
		assert.NotContains(t, ids, "helper/Do", "scope A's call must not leak into B's callees")
	})
}
