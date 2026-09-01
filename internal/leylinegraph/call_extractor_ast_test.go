package leylinegraph

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// seedASTCallFixture builds a minimal SQLite DB with the schema
// ASTWalker expects (nodes + _ast tables) and inserts one Go-shaped
// call: caller `pkg.Foo` invokes `Bar()`.
//
// AST shape (matches Go's first call pattern: OuterKind=call_expression,
// LeafKind=identifier):
//
//	pkg/main.go
//	└── call_expression  (outer)
//	    └── identifier   (leaf, record="Bar")
func seedASTCallFixture(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "ast.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)

	_, err = db.Exec(`
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY,
			parent_id TEXT,
			name TEXT,
			kind INTEGER,
			mtime INTEGER,
			source_file TEXT,
			record TEXT
		);
		CREATE TABLE _ast (
			node_id TEXT PRIMARY KEY,
			source_id TEXT NOT NULL,
			node_kind TEXT NOT NULL,
			start_byte INTEGER, end_byte INTEGER,
			start_row INTEGER, start_col INTEGER,
			end_row INTEGER, end_col INTEGER
		);

		-- The call: a Go file containing 'Bar()'. Two AST nodes:
		-- the call_expression and the identifier child it wraps.
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('call_outer', '',           'Bar()', 1, 0, 'main.go', 'Bar()'),
		  ('call_leaf',  'call_outer', 'Bar',   0, 0, 'main.go', 'Bar');
		INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col) VALUES
		  ('call_outer', 'main.go', 'call_expression', 0, 5, 1, 0, 1, 5),
		  ('call_leaf',  'main.go', 'identifier',      0, 3, 1, 0, 1, 3);
	`)
	require.NoError(t, err)
	return db, "main.go"
}

// TestNewASTCallExtractor_ResolvesGoCall pins the basic happy path:
// given a synthetic _ast row for a Go call_expression, the extractor
// returns the call token. Mirrors what newCallExtractor (CGO) returns
// for the same input shape, but via SQL — no tree-sitter, no parser.
func TestNewASTCallExtractor_ResolvesGoCall(t *testing.T) {
	db, sourcePath := seedASTCallFixture(t)
	defer func() { _ = db.Close() }()

	extract := NewASTCallExtractor(db)
	calls, err := extract(nil, sourcePath, "go")
	require.NoError(t, err)
	require.Len(t, calls, 1, "synthetic call_expression(identifier=Bar) must surface")
	assert.Equal(t, "Bar", calls[0].Token)
	assert.Empty(t, calls[0].Qualifier, "bare identifier pattern has no qualifier")
}

// TestNewASTCallExtractor_UnknownLanguageReturnsNil mirrors the
// SitterWalker-backed extractor's "no grammar" behavior — when the
// language has no registered call pattern, return nil, nil rather
// than erroring. Callers treat empty as "no calls in this file."
func TestNewASTCallExtractor_UnknownLanguageReturnsNil(t *testing.T) {
	db, sourcePath := seedASTCallFixture(t)
	defer func() { _ = db.Close() }()

	extract := NewASTCallExtractor(db)
	calls, err := extract(nil, sourcePath, "esperanto")
	require.NoError(t, err)
	assert.Empty(t, calls)
}

// TestNewASTCallExtractor_ContentArgIgnored pins the contract that
// the extractor's `content` parameter is unused — calls are resolved
// from the pre-parsed _ast table keyed by `path`, not by re-parsing.
// This is the central design difference vs newCallExtractor (CGO,
// re-parses content via tree-sitter).
func TestNewASTCallExtractor_ContentArgIgnored(t *testing.T) {
	db, sourcePath := seedASTCallFixture(t)
	defer func() { _ = db.Close() }()

	extract := NewASTCallExtractor(db)
	// Pass garbage as content; the extractor must ignore it.
	calls, err := extract([]byte("not Go at all — totally bogus bytes"), sourcePath, "go")
	require.NoError(t, err)
	require.Len(t, calls, 1, "extractor must trust the AST, not the content arg")
	assert.Equal(t, "Bar", calls[0].Token)
}

// TestNewASTCallExtractor_NonexistentSourcePathReturnsEmpty pins
// graceful handling of a stale path arg — querying _ast for a
// source_id that doesn't exist yields no rows, not an error. Same
// shape as the SitterWalker-backed extractor's response when given
// an empty content slice.
func TestNewASTCallExtractor_NonexistentSourcePathReturnsEmpty(t *testing.T) {
	db, _ := seedASTCallFixture(t)
	defer func() { _ = db.Close() }()

	extract := NewASTCallExtractor(db)
	calls, err := extract(nil, "does/not/exist.go", "go")
	require.NoError(t, err)
	assert.Empty(t, calls)
}

// TestPickCallExtractor_PrefersASTWhenAvailable pins the dispatch
// at the wiring sites: a SQLiteGraph whose .db carries `_ast`
// gets the pure-Go extractor, not the CGO one. We can't directly
// observe which closure was returned (CallExtractor is opaque),
// so we observe via the central design difference — the AST
// extractor ignores `content` and trusts the AST. Pass garbage
// content with a path that resolves in `_ast`; if the result is
// the AST-derived call, we know we got the AST extractor. If it
// were the CGO one, parsing the garbage would yield no calls.
func TestPickCallExtractor_PrefersASTWhenAvailable(t *testing.T) {
	db, sourcePath := seedASTCallFixture(t)
	defer func() { _ = db.Close() }()

	extract := PickCallExtractor(db)
	calls, err := extract([]byte("garbage that wouldn't parse as Go"), sourcePath, "go")
	require.NoError(t, err)
	require.Len(t, calls, 1, "AST extractor must surface 'Bar' from the pre-parsed _ast")
	assert.Equal(t, "Bar", calls[0].Token)
}

// TestPickCallExtractor_FallsBackWhenASTAbsent pins the inverse:
// a SQLiteGraph whose .db has no `_ast` table gets the CGO
// extractor as fallback. We don't try to actually run the CGO
// extractor in this test (CGO + tests is the mache-2y9w story);
// we observe the dispatch by checking it doesn't return nil and
// — since the closure can't be compared — by trusting the
// detection logic that gated the dispatch.
func TestPickCallExtractor_FallsBackWhenASTAbsent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "noast.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// Schema with no _ast table — only nodes/_source — to mirror
	// what mache build (standalone CGO path) produces today.
	_, err = db.Exec(`
		CREATE TABLE nodes (id TEXT, parent_id TEXT, name TEXT, kind INTEGER, mtime INTEGER, source_file TEXT, record TEXT);
		CREATE TABLE _source (id TEXT PRIMARY KEY, language TEXT, content BLOB);
	`)
	require.NoError(t, err)

	extract := PickCallExtractor(db)
	require.NotNil(t, extract, "fallback extractor must not be nil")
	// We can't easily verify it's the CGO closure without invoking
	// it (which exercises CGO). The dispatch contract — _ast
	// absent → fall back — is enforced by the picker's SQL check;
	// see TestPickCallExtractor_DetectsAST for that.
}

// TestPickCallExtractor_HandlesNilDB pins the safety contract for
// callers that might pass a nil DB handle (not a current call site
// but a contract worth preserving as wiring evolves).
func TestPickCallExtractor_HandlesNilDB(t *testing.T) {
	extract := PickCallExtractor(nil)
	assert.NotNil(t, extract, "nil DB must yield the CGO fallback, not a nil closure")
}

// TestNewASTScopedCallExtractor_ResolvesGoCall pins the scoped-extractor
// wiring (bead mache-fd9982): unlike NewASTCallExtractor, sourceID/scopeID
// here are the REAL `_ast` source_id + scope node id, not a graph node id.
// With an empty scopeID (whole-file match, mirroring the unscoped fixture),
// it must still resolve the same call NewASTCallExtractor finds.
func TestNewASTScopedCallExtractor_ResolvesGoCall(t *testing.T) {
	db, sourcePath := seedASTCallFixture(t)
	defer func() { _ = db.Close() }()

	extract := NewASTScopedCallExtractor(db)
	calls, err := extract(sourcePath, "", "go")
	require.NoError(t, err)
	require.Len(t, calls, 1, "synthetic call_expression(identifier=Bar) must surface")
	assert.Equal(t, "Bar", calls[0].Token)
	assert.Empty(t, calls[0].Qualifier, "bare identifier pattern has no qualifier")
}

// TestNewASTScopedCallExtractor_NonexistentScopeReturnsEmpty pins the
// scoping contract: a scopeID prefix that matches nothing in `_ast` yields
// no calls, not an error — the same "stale/mismatched id degrades to empty"
// shape as the unscoped extractor's nonexistent-source-path test.
func TestNewASTScopedCallExtractor_NonexistentScopeReturnsEmpty(t *testing.T) {
	db, sourcePath := seedASTCallFixture(t)
	defer func() { _ = db.Close() }()

	extract := NewASTScopedCallExtractor(db)
	calls, err := extract(sourcePath, "no/such/scope", "go")
	require.NoError(t, err)
	assert.Empty(t, calls)
}

// TestPickScopedCallExtractor_PrefersASTWhenAvailable mirrors
// TestPickCallExtractor_PrefersASTWhenAvailable for the scoped picker: a
// .db carrying `_ast` yields a working scoped extractor.
func TestPickScopedCallExtractor_PrefersASTWhenAvailable(t *testing.T) {
	db, sourcePath := seedASTCallFixture(t)
	defer func() { _ = db.Close() }()

	extract := PickScopedCallExtractor(db)
	require.NotNil(t, extract)
	calls, err := extract(sourcePath, "", "go")
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.Equal(t, "Bar", calls[0].Token)
}

// TestPickScopedCallExtractor_NilWhenASTAbsentOrDBNil pins the inverse of
// the CallExtractor picker: since there is no CGO fallback for the scoped
// extractor, absence of `_ast` (or a nil db) must yield nil, not a closure
// that would silently no-op. Callers (GetCallees) already treat a nil
// scopedExtractor as "fall back to the legacy path".
func TestPickScopedCallExtractor_NilWhenASTAbsentOrDBNil(t *testing.T) {
	assert.Nil(t, PickScopedCallExtractor(nil))

	dbPath := filepath.Join(t.TempDir(), "noast.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`CREATE TABLE nodes (id TEXT, parent_id TEXT, name TEXT, kind INTEGER, mtime INTEGER, source_file TEXT, record TEXT);`)
	require.NoError(t, err)

	assert.Nil(t, PickScopedCallExtractor(db))
}
