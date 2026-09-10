package ingest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// TestReadSource_PathFallback verifies that when a _source row has empty
// content but a non-empty path column, readSource falls back to reading the
// file from disk.
func TestReadSource_PathFallback(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "x.go")
	body := []byte("package x\n")
	require.NoError(t, os.WriteFile(srcPath, body, 0o600))

	b := fixturedb.New(t, fixturedb.Leyline)
	b.SourceFile("x.go", "go", srcPath)
	_, f := b.Build()

	lang, got, err := readSource(f.DB(), "x.go")
	require.NoError(t, err)
	assert.Equal(t, "go", lang)
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
// source bytes by the AST byte range. Builds a dedicated fixture so the
// fallback path is exercised end-to-end (independent of the shared seeder).
func TestASTWalker_Query_ByteRangeFallback(t *testing.T) {
	const src = "package main\n\nfunc Validate(x int) error {\n\treturn nil\n}\n"
	b := fixturedb.New(t, fixturedb.Leyline)
	b.Source("main.go", "go", src)
	b.ASTNode("main.go", "source_file", "main.go", fixturedb.Bytes(0, len(src)))
	b.ASTNode("main.go/function_declaration", "function_declaration", "main.go", fixturedb.Bytes(14, 56))
	// No Token: the record column is EMPTY on purpose. "Validate" lives at
	// bytes [19, 27) of src.
	b.ASTNode("main.go/function_declaration/identifier", "identifier", "main.go", fixturedb.Bytes(19, 27),
		fixturedb.Detail{Field: "name"})
	_, f := b.Build()
	db := f.DB()

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

// TestReadSource_NoContentNoPath verifies the error path when _source has
// neither inline content nor a path reference.
func TestReadSource_NoContentNoPath(t *testing.T) {
	b := fixturedb.New(t, fixturedb.Leyline)
	b.SourceFile("empty.go", "go", "")
	_, f := b.Build()

	_, _, err := readSource(f.DB(), "empty.go")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no content")
}
