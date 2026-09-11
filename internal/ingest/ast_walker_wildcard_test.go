package ingest

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rustWalker parses one Rust source with the pinned leyline and returns an
// ASTWalker over it plus the root that keys the file (leyline keys a file
// parsed from a directory by its path relative to that directory).
func rustWalker(t *testing.T, src string) (*ASTWalker, ASTRoot) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lib.rs"), []byte(src), 0o644))
	db, err := sql.Open("sqlite", parseWithLeyline(t, dir))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return NewASTWalker(db), ASTRoot{DB: db, SourceID: "lib.rs"}
}

// captured is the string value of capture name across matches, in match order.
func captured(t *testing.T, matches []Match, name string) []string {
	t.Helper()
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		v, ok := m.Values()[name].(string)
		require.True(t, ok, "capture %q missing from match %v", name, m.Values())
		out = append(out, v)
	}
	return out
}

func TestParseSelector_RejectsWildcardOuter(t *testing.T) {
	_, err := parseSelector("(_ name: (identifier) @name) @scope")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outer node of selector cannot be the wildcard")
}

// The receiver of a generic impl (`impl<T> Cell<T>`) is a type_identifier one
// level below the impl_item's `type:` child — which is a generic_type, not a
// type_identifier, so `(impl_item type: (type_identifier) @receiver ...)`
// cannot reach it. A wildcard step `(_ type: ...)` matches any kind at that
// level while still honouring the field (mache-c777ef).
func TestASTWalker_Query_WildcardStep_ReachesGenericReceiver(t *testing.T) {
	w, root := rustWalker(t, `pub struct Cell<T> { v: T }

impl<T: Clone> Cell<T> {
    pub fn new(v: T) -> Self { Cell { v } }
    pub fn get(&self) -> &T { &self.v }
}
`)
	const body = " body: (declaration_list (function_item name: (identifier) @name) @scope))"

	plain, err := w.Query(root, "(impl_item type: (type_identifier) @receiver"+body)
	require.NoError(t, err)
	assert.Empty(t, plain, "a generic impl's type: child is a generic_type, not a type_identifier")

	viaWildcard, err := w.Query(root, "(impl_item type: (_ type: (type_identifier) @receiver)"+body)
	require.NoError(t, err)
	require.Len(t, viaWildcard, 2, "one match per method under the impl")
	assert.Equal(t, []string{"Cell", "Cell"}, captured(t, viaWildcard, "receiver"))
	assert.Equal(t, []string{"new", "get"}, captured(t, viaWildcard, "name"))
	for _, scope := range captured(t, viaWildcard, "scope") {
		assert.True(t, strings.HasPrefix(scope, "pub fn "), "@scope is the method itself, got %q", scope)
		assert.NotContains(t, scope, "impl", "@scope must not widen to the impl block")
	}

	// A wildcard step still honours its field: `name:` is not `type:`.
	wrongField, err := w.Query(root, "(impl_item type: (_ name: (type_identifier) @receiver)"+body)
	require.NoError(t, err)
	assert.Empty(t, wrongField, "generic_type has a type: child, not a name: child")

	// A bare wildcard capture yields the node's own source text.
	raw, err := w.Query(root, "(impl_item type: (_) @receiver"+body)
	require.NoError(t, err)
	require.Len(t, raw, 2)
	assert.Equal(t, []string{"Cell<T>", "Cell<T>"}, captured(t, raw, "receiver"))
}

// An inner-@scope selector whose scope path reaches nothing is NOT a match.
// It used to fall back to the outer node, so a function-less module matched
// `(mod_item body: (declaration_list (function_item ...) @scope))` with @name
// resolving to the module's own name — projecting `functions/empty` for a
// module that defines no function (mache-c777ef).
func TestASTWalker_Query_InnerScope_NoOuterFallback(t *testing.T) {
	w, root := rustWalker(t, `pub mod empty { pub struct Nothing; }

pub mod full { pub fn origin() -> u32 { 0 } }

fn plain() -> u32 { let v = 1; v }
`)
	inMod, err := w.Query(root, "(mod_item body: (declaration_list (function_item name: (identifier) @name) @scope))")
	require.NoError(t, err)
	assert.Equal(t, []string{"origin"}, captured(t, inMod, "name"),
		"only the module that defines a function matches; `empty` must not fall back to the mod_item")

	inBlock, err := w.Query(root, "(block (function_item name: (identifier) @name) @scope)")
	require.NoError(t, err)
	assert.Empty(t, inBlock, "a block with no nested fn is no match, whatever identifiers it contains")
}

// Sibling schema nodes are an ordered choice per scope node: when two
// selectors under the same parent resolve the same @scope, the first in
// schema order claims it and the later one is skipped — the rust preset's
// receiver-shape alternatives rely on this so a method is projected once.
// Siblings under a different parent are unaffected (mache-c777ef).
func TestEngine_SiblingSelectors_FirstClaimsScope(t *testing.T) {
	src := []api.Leaf{{Name: "source", ContentTemplate: "{{.scope}}"}}
	byFn := "(function_item name: (identifier) @name) @scope"
	schema := &api.Topology{Version: "1", Nodes: []api.Node{
		{Name: "fns", Selector: "$", Children: []api.Node{
			{Name: "first_{{.name}}", Selector: "(source_file " + byFn + ")", Files: src},
			{Name: "second_{{.name}}", Selector: byFn, Files: src},
		}},
		{Name: "other", Selector: "$", Children: []api.Node{
			{Name: "third_{{.name}}", Selector: byFn, Files: src},
		}},
	}}

	dir := t.TempDir()
	rsFile := filepath.Join(dir, "lib.rs")
	require.NoError(t, os.WriteFile(rsFile, []byte("pub fn alpha() {}\n"), 0o644))

	store := graph.NewMemoryStore()
	engine := NewEngine(schema, store)
	attachLeylineAST(t, engine, rsFile)
	require.NoError(t, engine.Ingest(rsFile))

	fns, err := store.ListChildren("fns")
	require.NoError(t, err)
	assert.Equal(t, []string{"fns/first_alpha"}, fns, "the first sibling claims the function; the second must not project it again")

	other, err := store.ListChildren("other")
	require.NoError(t, err)
	assert.Equal(t, []string{"other/third_alpha"}, other, "a sibling under another parent is a separate choice")
}
