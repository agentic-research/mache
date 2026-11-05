package ingest

import (
	"os"
	"testing"

	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/agentic-research/mache/internal/lloschema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestASTWalker_EnsureIndexes verifies EnsureIndexes is idempotent and
// creates the compound index used by findNodesByKind. Exercises the
// EnsureIndexes method left uncovered by other tests.
func TestASTWalker_EnsureIndexes(t *testing.T) {
	db := seedTestAST(t, fixturedb.Leyline)
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
	db := seedTestAST(t, fixturedb.Leyline)
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
	db := seedTestAST(t, fixturedb.Leyline)
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
	db := seedTestAST(t, fixturedb.Leyline)
	defer func() { _ = db.Close() }()

	w := NewASTWalker(db)
	_, err := w.Query("not an ASTRoot", "(function_declaration) @scope")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected ASTRoot")
}

// TestASTWalker_Query_MissingCaptures pins what a missing capture does to a
// match. A required capture whose child_kind does not exist in the seeded
// tree drops the whole match. An underscore-prefixed capture is optional
// (used for predicates), so its absence keeps the match.
func TestASTWalker_Query_MissingCaptures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		selector string
		kept     bool
	}{
		// function_declaration nodes exist, but require a "return_type"
		// capture of a kind that does not.
		{"required", `(function_declaration (nonexistent_kind) @return_type) @scope`, false},
		// @name is required and present; @_marker is optional and absent.
		{"optional", `(function_declaration (identifier) @name (nonexistent_kind) @_marker) @scope`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := seedTestAST(t, fixturedb.Leyline)
			defer func() { _ = db.Close() }()
			w := NewASTWalker(db)
			matches, err := w.Query(ASTRoot{DB: db, SourceID: "main.go"}, tc.selector)
			require.NoError(t, err)
			assert.Equal(t, tc.kept, len(matches) > 0)
		})
	}
}

// TestReadSource_ReadsTheParsedBytesNotTheFile is mache-46c59d. ley-line
// leaves _source.content NULL and keeps the parsed bytes in source_blobs; the
// file at _source.path may have changed since, or be gone. Every node's offsets
// refer to the parsed bytes, so those are what the walker reads, whatever is
// on disk; here nothing is.
func TestReadSource_ReadsTheParsedBytesNotTheFile(t *testing.T) {
	for _, p := range []fixturedb.Producer{fixturedb.Leyline, fixturedb.LeylineV6} {
		t.Run(p.String(), func(t *testing.T) {
			b := fixturedb.New(t, p)
			b.Source("x.go", "go", "package parsed\n")
			_, f := b.Build()
			var path string
			require.NoError(t, f.DB().QueryRow(`SELECT path FROM _source WHERE id = 'x.go'`).Scan(&path))
			_, statErr := os.Stat(path)
			require.True(t, os.IsNotExist(statErr), "the row's path names no file, so only the projection can answer")

			lang, got, _, err := NewASTWalker(f.DB()).readSource("x.go")
			require.NoError(t, err)
			assert.Equal(t, "go", lang)
			assert.Equal(t, "package parsed\n", string(got))
		})
	}
}

// TestASTWalker_Query_FindNodesByKindError verifies the error path in
// Query when findNodesByKind fails. Closing the DB before Query gives the
// underlying SQL query a guaranteed error.
func TestASTWalker_Query_FindNodesByKindError(t *testing.T) {
	db := seedTestAST(t, fixturedb.Leyline)
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

// TestReadSource_NoContentNoPath: a row whose bytes the projection does not
// hold is an error. The file at its path is not read in their place.
func TestReadSource_NoContentNoPath(t *testing.T) {
	b := fixturedb.New(t, fixturedb.Leyline)
	b.Source("empty.go", "go", "")
	_, f := b.Build()

	_, _, _, err := NewASTWalker(f.DB()).readSource("empty.go")
	require.ErrorIs(t, err, lloschema.ErrNoSourceBytes)
}

// TestReadSource_RefusesTamperedBytes: bytes changed under their hash are
// refused, naming the hash, and the walker never slices them (mache-c1eb7a).
func TestReadSource_RefusesTamperedBytes(t *testing.T) {
	b := fixturedb.New(t, fixturedb.LeylineV6)
	b.Source("a.go", "go", "package a\n")
	_, f := b.Build()
	_, err := f.DB().Exec(`UPDATE source_blobs SET blob_bytes = CAST('package b' AS BLOB)
		WHERE blob_hash = (SELECT content_hash FROM _source WHERE id = 'a.go')`)
	require.NoError(t, err)

	_, content, _, err := NewASTWalker(f.DB()).readSource("a.go")
	require.ErrorIs(t, err, lloschema.ErrBlobTampered)
	assert.Nil(t, content)
}
