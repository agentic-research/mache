package cmd

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

	extract := newASTCallExtractor(db)
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

	extract := newASTCallExtractor(db)
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

	extract := newASTCallExtractor(db)
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

	extract := newASTCallExtractor(db)
	calls, err := extract(nil, "does/not/exist.go", "go")
	require.NoError(t, err)
	assert.Empty(t, calls)
}
