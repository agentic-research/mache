package ingest

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestASTWalker_EnsureIndexes verifies EnsureIndexes is idempotent and
// creates the compound index used by findNodesByKind. Exercises the
// EnsureIndexes method left uncovered by other tests.
func TestASTWalker_EnsureIndexes(t *testing.T) {
	db := seedTestAST(t)
	defer func() { _ = db.Close() }()

	w := NewASTWalker(db)
	// First call creates the index.
	require.NoError(t, w.EnsureIndexes())
	// Second call is a no-op due to IF NOT EXISTS.
	require.NoError(t, w.EnsureIndexes())

	// Confirm the index exists in sqlite_master.
	var name string
	err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_ast_kind_source'`,
	).Scan(&name)
	require.NoError(t, err)
	assert.Equal(t, "idx_ast_kind_source", name)
}

// TestASTWalker_Close is a smoke test that Close doesn't panic and doesn't
// touch the database (the walker doesn't own the connection).
func TestASTWalker_Close(t *testing.T) {
	db := seedTestAST(t)
	defer func() { _ = db.Close() }()

	w := NewASTWalker(db)
	w.Close()

	// DB should still be usable afterwards.
	var count int
	require.NoError(t, db.QueryRow("SELECT count(*) FROM nodes").Scan(&count))
	assert.Positive(t, count)
}

// TestASTWalker_Query_WildcardSelector verifies that the "$" selector
// returns a single empty-values match — the grouping-container path used
// by schemas like functions/, types/, imports/.
func TestASTWalker_Query_WildcardSelector(t *testing.T) {
	db := seedTestAST(t)
	defer func() { _ = db.Close() }()

	w := NewASTWalker(db)
	root := ASTRoot{DB: db, SourceID: "main.go", ParentPrefix: ""}

	matches, err := w.Query(root, "$")
	require.NoError(t, err)
	require.Len(t, matches, 1)
	assert.Empty(t, matches[0].Values())
	// The match's context should preserve the root for nested traversal.
	ctx, ok := matches[0].Context().(ASTRoot)
	require.True(t, ok)
	assert.Equal(t, root, ctx)
}

// TestASTWalker_Query_WrongRootType verifies Query rejects roots that
// aren't ASTRoot — exercises the type-assertion error path.
func TestASTWalker_Query_WrongRootType(t *testing.T) {
	db := seedTestAST(t)
	defer func() { _ = db.Close() }()

	w := NewASTWalker(db)
	_, err := w.Query("not an ASTRoot", "(function_declaration) @scope")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected ASTRoot")
}

// TestASTWalker_Query_MissingRequiredCapture verifies that when a required
// capture (one whose name doesn't start with "_") can't be resolved, the
// whole match is dropped. Uses a selector that names a capture whose
// child_kind doesn't exist in the seeded tree.
func TestASTWalker_Query_MissingRequiredCapture(t *testing.T) {
	db := seedTestAST(t)
	defer func() { _ = db.Close() }()

	w := NewASTWalker(db)
	root := ASTRoot{DB: db, SourceID: "main.go", ParentPrefix: ""}

	// function_declaration nodes exist, but require a "return_type" capture
	// of kind "nonexistent_kind" — should drop the matches.
	matches, err := w.Query(root,
		`(function_declaration (nonexistent_kind) @return_type) @scope`)
	require.NoError(t, err)
	assert.Empty(t, matches, "matches should be dropped when required capture is missing")
}

// TestASTWalker_Query_OptionalUnderscoreCaptureMissing verifies that when
// an underscore-prefixed capture (treated as optional, used for predicates)
// is missing, the match is NOT dropped — the loop continues.
func TestASTWalker_Query_OptionalUnderscoreCaptureMissing(t *testing.T) {
	db := seedTestAST(t)
	defer func() { _ = db.Close() }()

	w := NewASTWalker(db)
	root := ASTRoot{DB: db, SourceID: "main.go", ParentPrefix: ""}

	// Function captures @name (required, identifier) and @_marker (optional,
	// nonexistent_kind). Matches should still come through.
	matches, err := w.Query(root,
		`(function_declaration (identifier) @name (nonexistent_kind) @_marker) @scope`)
	require.NoError(t, err)
	assert.NotEmpty(t, matches, "match should survive missing optional capture")
}

// TestASTWalker_readSource_PathFallback verifies that when a _source row
// has empty content but a non-empty path column, readSource falls back to
// reading the file from disk.
func TestASTWalker_readSource_PathFallback(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "x.go")
	body := []byte("package x\n")
	require.NoError(t, os.WriteFile(srcPath, body, 0o600))

	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`CREATE TABLE _source (
		id TEXT PRIMARY KEY,
		language TEXT NOT NULL,
		content BLOB,
		path TEXT
	)`)
	require.NoError(t, err)
	_, err = db.Exec(
		"INSERT INTO _source (id, language, content, path) VALUES (?, ?, NULL, ?)",
		"x.go", "go", srcPath,
	)
	require.NoError(t, err)

	w := NewASTWalker(db)
	got, err := w.readSource(db, "x.go")
	require.NoError(t, err)
	assert.Equal(t, body, got)
}

// TestASTWalker_Query_FindNodesByKindError verifies the error path in
// Query when findNodesByKind fails. Closing the DB before Query gives the
// underlying SQL query a guaranteed error.
func TestASTWalker_Query_FindNodesByKindError(t *testing.T) {
	db := seedTestAST(t)
	w := NewASTWalker(db)

	// Close the DB so the subsequent SQL query fails.
	require.NoError(t, db.Close())

	root := ASTRoot{DB: db, SourceID: "main.go", ParentPrefix: ""}
	_, err := w.Query(root, "(function_declaration) @scope")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "find function_declaration nodes")
}

// TestASTWalker_Query_ByteRangeFallback verifies that when a captured leaf
// node has an empty `record` column, the walker falls back to slicing the
// source bytes by the AST byte range. Builds a dedicated schema so the
// fallback path is exercised end-to-end (independent of the shared seeder).
func TestASTWalker_Query_ByteRangeFallback(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "fallback.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY,
			parent_id TEXT,
			name TEXT NOT NULL,
			kind INTEGER NOT NULL,
			size INTEGER DEFAULT 0,
			mtime INTEGER NOT NULL,
			record_id TEXT,
			record TEXT,
			source_file TEXT
		);
		CREATE TABLE _ast (
			node_id TEXT PRIMARY KEY,
			source_id TEXT NOT NULL,
			node_kind TEXT NOT NULL,
			start_byte INTEGER NOT NULL,
			end_byte INTEGER NOT NULL,
			start_row INTEGER NOT NULL,
			start_col INTEGER NOT NULL,
			end_row INTEGER NOT NULL,
			end_col INTEGER NOT NULL
		);
		CREATE TABLE _source (
			id TEXT PRIMARY KEY,
			language TEXT NOT NULL,
			content BLOB,
			path TEXT
		);
	`)
	require.NoError(t, err)

	src := []byte("package main\n\nfunc Validate(x int) error {\n\treturn nil\n}\n")
	_, err = db.Exec("INSERT INTO _source (id, language, content, path) VALUES (?, ?, ?, NULL)",
		"main.go", "go", src)
	require.NoError(t, err)

	// Function declaration with an empty-record identifier child — forces
	// the walker into the byte-range fallback branch.
	insertNode := func(id, parent, name string, kind int, record string) {
		_, err := db.Exec(
			"INSERT INTO nodes (id, parent_id, name, kind, size, mtime, record) VALUES (?, ?, ?, ?, 0, 0, ?)",
			id, parent, name, kind, record,
		)
		require.NoError(t, err)
	}
	insertNode("source_file", "", "source_file", 1, "")
	insertNode("source_file/function_declaration", "source_file", "function_declaration", 1, "")
	// Record column is EMPTY here on purpose.
	insertNode("source_file/function_declaration/identifier", "source_file/function_declaration", "identifier", 0, "")

	insertAST := func(nodeID, kind string, start, end int) {
		_, err := db.Exec(
			"INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col) VALUES (?, 'main.go', ?, ?, ?, 0, 0, 0, 0)",
			nodeID, kind, start, end,
		)
		require.NoError(t, err)
	}
	insertAST("source_file/function_declaration", "function_declaration", 14, 56)
	// "Validate" lives at bytes [19, 27) in src.
	insertAST("source_file/function_declaration/identifier", "identifier", 19, 27)

	w := NewASTWalker(db)
	root := ASTRoot{DB: db, SourceID: "main.go", ParentPrefix: ""}

	matches, err := w.Query(root, `(function_declaration (identifier) @name) @scope`)
	require.NoError(t, err)
	require.Len(t, matches, 1)

	// The walker must reconstruct "Validate" from the source bytes since the
	// record column is empty.
	got, _ := matches[0].Values()["name"].(string)
	assert.Equal(t, "Validate", got, "expected byte-range fallback to recover the identifier")
}

// TestASTWalker_readSource_NoContentNoPath verifies the error path when
// _source has neither inline content nor a path reference.
func TestASTWalker_readSource_NoContentNoPath(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`CREATE TABLE _source (
		id TEXT PRIMARY KEY,
		language TEXT NOT NULL,
		content BLOB,
		path TEXT
	)`)
	require.NoError(t, err)
	_, err = db.Exec(
		"INSERT INTO _source (id, language, content, path) VALUES (?, ?, NULL, '')",
		"empty.go", "go",
	)
	require.NoError(t, err)

	w := NewASTWalker(db)
	_, err = w.readSource(db, "empty.go")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no content")
}
