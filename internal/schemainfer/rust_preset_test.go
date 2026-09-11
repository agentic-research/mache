package schemainfer

import (
	"database/sql"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/lltest"
	"github.com/agentic-research/mache/internal/testfixtures"
	"github.com/agentic-research/mache/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// projectRust ingests parseDir, ley-line-parsed into astDB, with the rust
// preset into a MemoryStore.
func projectRust(t *testing.T, astDB *sql.DB, parseDir string) *graph.MemoryStore {
	t.Helper()
	schema, err := LoadPresetSchema("rust")
	require.NoError(t, err)
	store := graph.NewMemoryStore()
	engine := ingest.NewEngine(schema, store)
	engine.SetASTWalker(ingest.NewASTWalker(astDB))
	require.NoError(t, engine.Ingest(parseDir))
	return store
}

// projectedNames lists the base names of dir's children, sorted. A missing
// dir is an empty list, so an assertion on its contents still fails visibly.
func projectedNames(t *testing.T, store *graph.MemoryStore, dir string) []string {
	t.Helper()
	ids, err := store.ListChildren(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		names = append(names, strings.TrimPrefix(id, dir+"/"))
	}
	sort.Strings(names)
	return names
}

// projectedSource is the rendered content of node id.
func projectedSource(t *testing.T, store *graph.MemoryStore, id string) string {
	t.Helper()
	n, err := store.GetNode(id)
	require.NoError(t, err, "get %s", id)
	buf := make([]byte, n.ContentSize())
	read, err := store.ReadContent(id, buf, 0)
	require.NoError(t, err, "read %s", id)
	return string(buf[:read])
}

// TestRustPreset_MethodsAreReceiverQualified pins the bead's acceptance on
// the rust fixture (mache-c777ef defect 1):
//
//	(a) every impl method is methods/<Receiver>.<name>, whose source is that
//	    one fn — never the impl block;
//	(b) functions/ holds free functions only — no impl method, and nothing
//	    for a module or block that defines no function;
//	(c) implementations/ is an index of impl blocks by receiver, with no
//	    source and no generic arguments leaking into the name.
//
// The receiver shapes come from impls.rs (inherent impls, free functions) and
// traits.rs (trait impls), one impl per shape of the impl_item's `type:`
// child; lib.rs adds the plain-receiver impls.
func TestRustPreset_MethodsAreReceiverQualified(t *testing.T) {
	astDB, parseDir := lltest.ParseSourceViaLeyline(t, filepath.Join(testutil.PresetFixturesDir(t), "rust"))
	store := projectRust(t, astDB, parseDir)

	wantMethods := []string{
		"Catalog.count", "Catalog.insert", "Catalog.lookup", "Catalog.new", // lib.rs
		"Cell.fmt", "Cell.get", "Cell.new",
		"Grid.into_iter", "Grid.new",
		"Hash32.describe", // a trait's default method
		"Point.new",
		"Vec2.default",
		"[u8].hash32", "u32.hash32", "(u8, u8).hash32",
	}
	sort.Strings(wantMethods)
	assert.Equal(t, wantMethods, projectedNames(t, store, "methods"))

	for _, m := range wantMethods {
		src := projectedSource(t, store, "methods/"+m+"/source")
		name := m[strings.LastIndex(m, ".")+1:]
		assert.Contains(t, src, "fn "+name+"(", "methods/%s/source is the method itself", m)
		assert.NotContains(t, src, "impl ", "methods/%s/source must not widen to the impl block", m)
		assert.NotContains(t, src, "trait ", "methods/%s/source must not widen to the trait", m)
		assert.Equal(t, 1, strings.Count(src, "fn "), "methods/%s/source holds exactly one fn", m)
	}
	assert.Equal(t, "pub fn get(&self) -> &T {\n        &self.value\n    }",
		projectedSource(t, store, "methods/Cell.get/source"))

	assert.Equal(t, []string{"free", "nested", "open", "origin", "plain"}, projectedNames(t, store, "functions"),
		"functions/ is free functions only: no impl method, no `empty` module, no block tail `v`")

	impls := projectedNames(t, store, "implementations")
	assert.Equal(t, []string{"(u8, u8)", "Catalog", "Cell", "Grid", "Point", "Vec2", "[u8]", "u32"}, impls)
	for _, name := range impls {
		id := "implementations/" + name
		assert.NotContains(t, name, "<", "%s: generic arguments leaked into the receiver name", id)
		children, err := store.ListChildren(id)
		require.NoError(t, err)
		assert.NotContains(t, children, id+"/source", "%s is an index entry, not a copy of the impl block", id)
	}
}

// TestRustPreset_EveryFunctionItemProjectedOnce is the corpus-scale form of
// the same invariant on a real crate: every `function_item` ley-line parsed
// is projected exactly once, as a free function or as a method — no method
// swallowed by its impl block, no function projected twice by two receiver
// shapes matching the same impl (mache-c777ef).
func TestRustPreset_EveryFunctionItemProjectedOnce(t *testing.T) {
	testfixtures.RequireTier(t, "medium")
	srcPath, err := testfixtures.ResolvePath("medium-rust-rosary")
	require.NoError(t, err)
	astDB, parseDir := lltest.ParseSourceViaLeyline(t, srcPath)
	store := projectRust(t, astDB, parseDir)

	var functionItems int
	require.NoError(t, astDB.QueryRow(`SELECT count(*) FROM _ast WHERE node_kind = 'function_item'`).Scan(&functionItems))
	require.Positive(t, functionItems)

	functions := projectedNames(t, store, "functions")
	methods := projectedNames(t, store, "methods")
	t.Logf("%d function_item nodes; functions/=%d methods/=%d", functionItems, len(functions), len(methods))
	assert.Equal(t, functionItems, len(functions)+len(methods),
		"%d function_item nodes parsed; %d projected under functions/ and %d under methods/",
		functionItems, len(functions), len(methods))
	assert.NotEmpty(t, methods)
}
