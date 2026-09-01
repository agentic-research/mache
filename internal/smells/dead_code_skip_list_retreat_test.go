package smells

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// dead_code skip-list precise retreat — ADR-0013 follow-up after
// Falsifiability A passed empirically. The skip-list now has two
// halves:
//
//   - Runtime-invoked names (main, init, Test*, Benchmark*, ...)
//     stay unconditionally skipped. Lattice ceiling — neither
//     tree-sitter nor LSP synthesizes references for the Go runtime
//     loader or test harness's reflective dispatch.
//
//   - Interface-method names (Read, Write, ServeHTTP, ...) skip
//     ONLY when LSP didn't see a binding-fidelity reference to
//     that specific def. With LSP coverage, the alive-check's
//     binding arm decides; without LSP coverage, the skip-list
//     compensates as before.
//
// Tests pin both halves of that contract.

func TestDeadCode_InterfaceMethodSkippedWithoutLSP(t *testing.T) {
	// No _lsp_* tables. An interface-named method (`Read`) with no
	// node_refs entries falls into the legacy compensation path —
	// skip-listed, NOT flagged dead.
	dbPath := filepath.Join(t.TempDir(), "no_lsp.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)

	_, err = db.Exec(`
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY, parent_id TEXT, name TEXT NOT NULL,
			kind INTEGER NOT NULL, size INTEGER, mtime INTEGER NOT NULL,
			record_id TEXT, record TEXT, source_file TEXT
		);
		CREATE TABLE node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

		INSERT INTO nodes VALUES
			('pkg/methods/MyType.Read', NULL, 'Read', 1, 0, 0, NULL, NULL, NULL),
			('pkg/methods/MyType.Read/source', 'pkg/methods/MyType.Read', 'source', 0, 0, 0, NULL, NULL, 'pkg/mytype.go');
		INSERT INTO node_defs VALUES ('Read', 'pkg/methods/MyType.Read');
	`)
	require.NoError(t, err)

	tg := &testutil.SmellTestGraph{DB: db}
	handler := MakeFindSmellsHandler(tg)

	res, err := handler(context.Background(), testutil.MakeRequest(map[string]any{"rule": "dead_code"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(testutil.ResultText(t, res)), &resp))

	for _, f := range resp.Findings {
		assert.NotEqual(t, "pkg/methods/MyType.Read", f.NodeID,
			"without LSP coverage, interface-named def 'Read' must remain skip-listed (legacy compensation)")
	}
}

func TestDeadCode_InterfaceMethodFlaggedWhenLSPSeesNoRefs(t *testing.T) {
	// LSP indexed the file (binding-fidelity rows exist for OTHER
	// defs in the package) but found NO references to MyType.Read.
	// The precise-retreat: skip-list no longer hides Read, so the
	// alive-check makes the call. With no node_refs and no
	// binding refs to MyType.Read, the def is flagged dead.
	//
	// This is the case the precise retreat unlocks — a method
	// whose name happens to match the interface skip-list but is
	// genuinely dead. Pre-retreat: skip-list masked it; post-
	// retreat: it surfaces as a real finding.
	dbPath := filepath.Join(t.TempDir(), "lsp_no_refs.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)

	_, err = db.Exec(`
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY, parent_id TEXT, name TEXT NOT NULL,
			kind INTEGER NOT NULL, size INTEGER, mtime INTEGER NOT NULL,
			record_id TEXT, record TEXT, source_file TEXT
		);
		CREATE TABLE node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE _lsp_defs (
			node_id TEXT NOT NULL, def_token TEXT NOT NULL DEFAULT '',
			def_uri TEXT NOT NULL,
			def_start_line INTEGER NOT NULL, def_start_col INTEGER NOT NULL,
			def_end_line INTEGER NOT NULL, def_end_col INTEGER NOT NULL
		);
		CREATE TABLE _lsp_refs (
			node_id TEXT NOT NULL, referrer_node_id TEXT,
			ref_token TEXT NOT NULL DEFAULT '',
			ref_uri TEXT NOT NULL,
			ref_start_line INTEGER NOT NULL, ref_start_col INTEGER NOT NULL,
			ref_end_line INTEGER NOT NULL, ref_end_col INTEGER NOT NULL
		);

		INSERT INTO nodes VALUES
			('pkg/methods/MyType.Read', NULL, 'Read', 1, 0, 0, NULL, NULL, NULL),
			('pkg/methods/MyType.Read/source', 'pkg/methods/MyType.Read', 'source', 0, 0, 0, NULL, NULL, 'pkg/mytype.go'),
			('pkg/functions/Other', NULL, 'Other', 1, 0, 0, NULL, NULL, NULL),
			('pkg/functions/Other/source', 'pkg/functions/Other', 'source', 0, 0, 0, NULL, NULL, 'pkg/other.go');

		INSERT INTO node_defs VALUES
			('Read', 'pkg/methods/MyType.Read'),
			('Other', 'pkg/functions/Other');

		-- LSP saw the file (def_token populated, def_uri set) but
		-- emitted NO _lsp_refs targeting MyType.Read — the method
		-- has no callers as far as gopls is concerned.
		INSERT INTO _lsp_defs VALUES
			('pkg/methods/MyType.Read', 'Read', 'file:///pkg/mytype.go', 10, 5, 10, 9),
			('pkg/functions/Other', 'Other', 'file:///pkg/other.go', 5, 5, 5, 10);

		-- _lsp_refs intentionally empty for MyType.Read. (We could
		-- add a binding row for Other to prove other refs in the
		-- file don't accidentally rescue Read, but the empty case
		-- is clearer.)
	`)
	require.NoError(t, err)

	tg := &testutil.SmellTestGraph{DB: db}
	handler := MakeFindSmellsHandler(tg)

	res, err := handler(context.Background(), testutil.MakeRequest(map[string]any{"rule": "dead_code"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(testutil.ResultText(t, res)), &resp))

	flagged := map[string]bool{}
	for _, f := range resp.Findings {
		flagged[f.NodeID] = true
	}
	assert.True(t, flagged["pkg/methods/MyType.Read"],
		"LSP-aware skip-list retreat: 'Read' is genuinely dead in this fixture (no node_refs, no _lsp_refs targeting it) — flag it instead of masking via the interface-name skip-list")
}

func TestDeadCode_InterfaceMethodAliveWhenLSPSeesRefs(t *testing.T) {
	// LSP saw a binding-fidelity reference to MyType.Read. The
	// alive-check's binding arm matches target_node_id, so the def
	// is alive. The skip-list retreat doesn't change this case —
	// it was never going to be flagged in production either (the
	// skip-list would hide it pre-retreat, the alive-check covers
	// it post-retreat). Pinning the post-retreat path explicitly.
	dbPath := filepath.Join(t.TempDir(), "lsp_with_refs.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)

	_, err = db.Exec(`
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY, parent_id TEXT, name TEXT NOT NULL,
			kind INTEGER NOT NULL, size INTEGER, mtime INTEGER NOT NULL,
			record_id TEXT, record TEXT, source_file TEXT
		);
		CREATE TABLE node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE _lsp_defs (
			node_id TEXT NOT NULL, def_token TEXT NOT NULL DEFAULT '',
			def_uri TEXT NOT NULL,
			def_start_line INTEGER NOT NULL, def_start_col INTEGER NOT NULL,
			def_end_line INTEGER NOT NULL, def_end_col INTEGER NOT NULL
		);
		CREATE TABLE _lsp_refs (
			node_id TEXT NOT NULL, referrer_node_id TEXT,
			ref_token TEXT NOT NULL DEFAULT '',
			ref_uri TEXT NOT NULL,
			ref_start_line INTEGER NOT NULL, ref_start_col INTEGER NOT NULL,
			ref_end_line INTEGER NOT NULL, ref_end_col INTEGER NOT NULL
		);

		INSERT INTO nodes VALUES
			('pkg/methods/MyType.Read', NULL, 'Read', 1, 0, 0, NULL, NULL, NULL),
			('pkg/methods/MyType.Read/source', 'pkg/methods/MyType.Read', 'source', 0, 0, 0, NULL, NULL, 'pkg/mytype.go');
		INSERT INTO node_defs VALUES ('Read', 'pkg/methods/MyType.Read');
		INSERT INTO _lsp_defs VALUES
			('pkg/methods/MyType.Read', 'Read', 'file:///pkg/mytype.go', 10, 5, 10, 9);
	`)
	require.NoError(t, err)

	// Post-mache-6bd4d8: binding-fidelity refs come from the sibling
	// .bindings.capnp event log, not the legacy _lsp_refs SQL table.
	// Write the equivalent record there.
	testutil.WriteBindingLogForTest(t, dbPath,
		"pkg/methods/MyType.Read", // targetNodeId
		"Read",                    // refToken
		"pkg/functions/Caller",    // constructNodeId (referrer)
		"file:///pkg/caller.go")   // refUri

	tg := &testutil.SmellTestGraph{DB: db, Path: dbPath}
	handler := MakeFindSmellsHandler(tg)

	res, err := handler(context.Background(), testutil.MakeRequest(map[string]any{"rule": "dead_code"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(testutil.ResultText(t, res)), &resp))

	for _, f := range resp.Findings {
		assert.NotEqual(t, "pkg/methods/MyType.Read", f.NodeID,
			"alive-check binding arm matches the capnp ref → 'Read' is alive, not dead")
	}
}

func TestDeadCode_TestPrefixAlwaysSkipped(t *testing.T) {
	// Test*/Benchmark*/Example*/Fuzz* are lattice-ceiling: invoked
	// reflectively by `go test`; nothing static can see the
	// dispatch. The skip-list keeps these unconditionally —
	// LSP coverage doesn't change anything.
	dbPath := filepath.Join(t.TempDir(), "test_prefix.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)

	_, err = db.Exec(`
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY, parent_id TEXT, name TEXT NOT NULL,
			kind INTEGER NOT NULL, size INTEGER, mtime INTEGER NOT NULL,
			record_id TEXT, record TEXT, source_file TEXT
		);
		CREATE TABLE node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		-- _lsp_* present, so binding_covered considered, but Test*
		-- defs aren't in interface_method category — they're in the
		-- always-skipped category — so coverage is irrelevant.
		CREATE TABLE _lsp_defs (
			node_id TEXT NOT NULL, def_token TEXT NOT NULL DEFAULT '',
			def_uri TEXT NOT NULL,
			def_start_line INTEGER NOT NULL, def_start_col INTEGER NOT NULL,
			def_end_line INTEGER NOT NULL, def_end_col INTEGER NOT NULL
		);
		CREATE TABLE _lsp_refs (
			node_id TEXT NOT NULL, referrer_node_id TEXT,
			ref_token TEXT NOT NULL DEFAULT '',
			ref_uri TEXT NOT NULL,
			ref_start_line INTEGER NOT NULL, ref_start_col INTEGER NOT NULL,
			ref_end_line INTEGER NOT NULL, ref_end_col INTEGER NOT NULL
		);

		INSERT INTO nodes VALUES
			('pkg/functions/TestFoo', NULL, 'TestFoo', 1, 0, 0, NULL, NULL, NULL),
			('pkg/functions/TestFoo/source', 'pkg/functions/TestFoo', 'source', 0, 0, 0, NULL, NULL, 'pkg/foo_test.go');
		INSERT INTO node_defs VALUES ('TestFoo', 'pkg/functions/TestFoo');
		-- No _lsp_refs targeting TestFoo. The runtime-invoked
		-- category skips it regardless.
	`)
	require.NoError(t, err)

	tg := &testutil.SmellTestGraph{DB: db}
	handler := MakeFindSmellsHandler(tg)

	res, err := handler(context.Background(), testutil.MakeRequest(map[string]any{"rule": "dead_code"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(testutil.ResultText(t, res)), &resp))

	for _, f := range resp.Findings {
		assert.NotEqual(t, "pkg/functions/TestFoo", f.NodeID,
			"Test* prefix is lattice-ceiling — must stay skip-listed regardless of LSP coverage")
	}
}
