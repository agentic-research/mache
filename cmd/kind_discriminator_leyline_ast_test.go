package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// buildLeylineRustASTFixture seeds the SAME failure the real-world
// layer-4 smoke caught (bead mache-5bb181): a leyline-parsed Rust .db
// where the symbol `parse` exists as BOTH a free function and an impl
// method. Unlike buildAmbiguousNameFixture (construct-dir projection,
// `pkg/functions/parse/source`), leyline emits tree-sitter node-kind
// paths with NO `/functions/` segment:
//
//	lib.rs/function_item_0                                  — free fn `parse`
//	lib.rs/impl_item_0/declaration_list/function_item_1     — method `parse`
//
// Both nodes are node_kind=function_item — the function-vs-method
// distinction lives ONLY in ancestry (the method's parent chain passes
// through an impl_item). Kind resolution therefore cannot come from the
// path segment; it must join _ast.node_kind and walk nodes.parent_id.
//
// The fixture wires:
//   - MemoryStore defs: `parse` → both leyline node_ids (drives LookupDef)
//   - nodes table: parent_id chain (file → impl_item → declaration_list → fn)
//   - _ast table: node_kind per node (the projection-independent kind source)
func buildLeylineRustASTFixture(t *testing.T) *smellTestGraph {
	t.Helper()

	const (
		fileID   = "lib.rs"
		freeFn   = "lib.rs/function_item_0"
		implID   = "lib.rs/impl_item_0"
		declList = "lib.rs/impl_item_0/declaration_list"
		methodFn = "lib.rs/impl_item_0/declaration_list/function_item_1"
	)

	dbPath := filepath.Join(t.TempDir(), "leyline_rust.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.Exec(`
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY, parent_id TEXT, name TEXT NOT NULL,
			kind INTEGER NOT NULL, size INTEGER DEFAULT 0,
			mtime INTEGER NOT NULL DEFAULT 0, record_id TEXT, record TEXT,
			source_file TEXT
		);
		CREATE TABLE _ast (
			node_id TEXT PRIMARY KEY, source_id TEXT NOT NULL,
			node_kind TEXT NOT NULL,
			start_byte INTEGER NOT NULL, end_byte INTEGER NOT NULL,
			start_row INTEGER, start_col INTEGER,
			end_row INTEGER, end_col INTEGER
		);
		CREATE TABLE node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
	`)
	require.NoError(t, err)

	// nodes: parent_id chain. File is root; free fn hangs off the file;
	// the method hangs off impl_item via declaration_list.
	nodeRows := []struct{ id, parent string }{
		{fileID, ""},
		{freeFn, fileID},
		{implID, fileID},
		{declList, implID},
		{methodFn, declList},
	}
	for _, r := range nodeRows {
		_, err = db.Exec(
			"INSERT INTO nodes (id, parent_id, name, kind) VALUES (?, ?, ?, 0)",
			r.id, r.parent, filepath.Base(r.id))
		require.NoError(t, err)
	}

	// _ast: tree-sitter node_kind per node. Both `parse` defs are
	// function_item; ancestry is what separates them.
	astRows := []struct{ id, kind string }{
		{fileID, "source_file"},
		{freeFn, "function_item"},
		{implID, "impl_item"},
		{declList, "declaration_list"},
		{methodFn, "function_item"},
	}
	for _, r := range astRows {
		_, err = db.Exec(
			"INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte) VALUES (?, 'lib.rs', ?, 0, 0)",
			r.id, r.kind)
		require.NoError(t, err)
	}

	// node_defs mirrors the in-memory defs so the fixture is faithful to
	// a real SQLiteGraph (which resolves defs from this table). Both
	// `parse` defs ALSO call `helper`, so the same two leyline-shaped
	// node_ids exercise the find_callers kind filter (function-caller
	// vs method-caller).
	store := graph.NewMemoryStore()
	for _, id := range []string{freeFn, methodFn} {
		// AddRoot registers the node so GetCallers can return it by ID.
		store.AddRoot(&graph.Node{ID: id, Mode: fs.ModeDir, Children: []string{}})
		require.NoError(t, store.AddDef("parse", id))
		require.NoError(t, store.AddRef("helper", id))
		_, err = db.Exec("INSERT INTO node_defs (token, node_id) VALUES ('parse', ?)", id)
		require.NoError(t, err)
	}

	return &smellTestGraph{MemoryStore: store, db: db, path: dbPath}
}

// TestKindDiscriminator_LeylineAST_FindDefinition is the regression
// proof for mache-5bb181: kind filtering MUST work against leyline-
// parsed paths (node_kind in _ast), not just mache's own construct-dir
// projection. PR #441's integration test only built construct-dir
// fixtures, so this gap shipped silently until a real-world smoke.
//
// Falsifies: any kind filter that pattern-matches `/functions/` path
// segments instead of resolving kind from _ast.node_kind + ancestry.
func TestKindDiscriminator_LeylineAST_FindDefinition(t *testing.T) {
	const (
		freeFn   = "lib.rs/function_item_0"
		methodFn = "lib.rs/impl_item_0/declaration_list/function_item_1"
	)

	g := buildLeylineRustASTFixture(t)
	handler := makeFindDefinitionHandler(g)

	t.Run("no_kind_returns_both", func(t *testing.T) {
		req := makeRequest(map[string]any{"symbol": "parse"})
		result, err := handler(context.Background(), req)
		require.NoError(t, err)
		dirs := extractDirIDs(t, resultText(t, result))
		require.Len(t, dirs, 2, "without kind, both `parse` defs should return; got %v", dirs)
		require.Contains(t, dirs, freeFn)
		require.Contains(t, dirs, methodFn)
	})

	t.Run("kind_function_narrows_to_free_function", func(t *testing.T) {
		req := makeRequest(map[string]any{"symbol": "parse", "kind": "function"})
		result, err := handler(context.Background(), req)
		require.NoError(t, err)
		dirs := extractDirIDs(t, resultText(t, result))
		require.Equal(t, []string{freeFn}, dirs,
			"kind=function must keep the free function_item (no impl_item ancestor), drop the method")
	})

	t.Run("kind_method_narrows_to_impl_method", func(t *testing.T) {
		req := makeRequest(map[string]any{"symbol": "parse", "kind": "method"})
		result, err := handler(context.Background(), req)
		require.NoError(t, err)
		dirs := extractDirIDs(t, resultText(t, result))
		require.Equal(t, []string{methodFn}, dirs,
			"kind=method must keep the function_item under impl_item, drop the free function")
	})
}

// TestResolveKindFromAST_EdgeCases hardens the _ast resolver against the
// two cases the PR #448 review surfaced:
//   - a cyclic nodes.parent_id must NOT hang the recursive CTE (the depth
//     cap bounds it); the node still resolves by its own node_kind.
//   - function_signature_item (Rust trait method signature, no body)
//     resolves to "method" directly, without needing ancestry.
func TestResolveKindFromAST_EdgeCases(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "edge.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.Exec(`
		CREATE TABLE nodes (id TEXT PRIMARY KEY, parent_id TEXT, name TEXT NOT NULL, kind INTEGER NOT NULL DEFAULT 0);
		CREATE TABLE _ast (node_id TEXT PRIMARY KEY, source_id TEXT NOT NULL, node_kind TEXT NOT NULL, start_byte INTEGER NOT NULL DEFAULT 0, end_byte INTEGER NOT NULL DEFAULT 0);
	`)
	require.NoError(t, err)

	// A <-> B parent_id cycle, with A a function_item. Without the depth
	// cap the ancestry CTE spins forever; with it, resolution terminates.
	_, err = db.Exec(`INSERT INTO nodes (id, parent_id, name) VALUES ('A','B','A'), ('B','A','B')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO _ast (node_id, source_id, node_kind) VALUES ('A','f.rs','function_item'), ('B','f.rs','block')`)
	require.NoError(t, err)

	// A trait method signature — resolves to method by node_kind alone.
	_, err = db.Exec(`INSERT INTO nodes (id, parent_id, name) VALUES ('sig','','sig')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO _ast (node_id, source_id, node_kind) VALUES ('sig','f.rs','function_signature_item')`)
	require.NoError(t, err)

	g := &smellTestGraph{MemoryStore: graph.NewMemoryStore(), db: db, path: dbPath}

	require.Equal(t, "function", resolveKindFromAST(g, "A"),
		"cyclic parent_id must terminate (depth cap) and resolve by own node_kind")
	require.Equal(t, "method", resolveKindFromAST(g, "sig"),
		"function_signature_item resolves to method without ancestry")
}

// TestKindDiscriminator_LeylineAST_FindCallers is the caller-side proof
// for the same bug: find_callers narrows by the KIND of caller, and the
// caller node_ids are leyline-shaped too. `helper` is called by both the
// free function `parse` and the impl method `parse`.
func TestKindDiscriminator_LeylineAST_FindCallers(t *testing.T) {
	const (
		freeFn   = "lib.rs/function_item_0"
		methodFn = "lib.rs/impl_item_0/declaration_list/function_item_1"
	)

	g := buildLeylineRustASTFixture(t)
	handler := makeFindCallersHandler(g)

	extractCallers := func(t *testing.T, raw string) []string {
		t.Helper()
		var bare []string
		if err := json.Unmarshal([]byte(raw), &bare); err == nil {
			return bare
		}
		var wrapped struct {
			Callers []string `json:"callers"`
		}
		require.NoError(t, json.Unmarshal([]byte(raw), &wrapped))
		return wrapped.Callers
	}

	t.Run("kind_function_narrows_to_free_function_caller", func(t *testing.T) {
		req := makeRequest(map[string]any{"token": "helper", "kind": "function"})
		result, err := handler(context.Background(), req)
		require.NoError(t, err)
		callers := extractCallers(t, resultText(t, result))
		require.Equal(t, []string{freeFn}, callers,
			"kind=function must keep ONLY the free-function caller")
	})

	t.Run("kind_method_narrows_to_method_caller", func(t *testing.T) {
		req := makeRequest(map[string]any{"token": "helper", "kind": "method"})
		result, err := handler(context.Background(), req)
		require.NoError(t, err)
		callers := extractCallers(t, resultText(t, result))
		require.Equal(t, []string{methodFn}, callers,
			"kind=method must keep ONLY the method caller (function_item under impl_item)")
	})
}
