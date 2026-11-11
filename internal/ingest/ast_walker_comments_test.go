package ingest

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/lloschema"
)

// commentSources are Go, Rust and Java files with line, block and doc
// comments: above the first construct, between constructs, inside a body, and
// after the last construct.
var commentSources = map[string]string{
	"a.go": `// Package a has comments everywhere.
package a

// Helper is documented
// on two lines.
func Helper() int { return 1 }

/* Other has a block comment above it. */
func Other() int {
	// inside
	return Helper()
}

// Spaced is not documented: a blank line sits between.

func Spaced() {}

// trailing
`,
	"lib.rs": `//! crate doc
/// Doc for foo.
/// More.
#[inline]
pub fn foo() -> i32 { 1 }

/* block */
pub fn bar() -> i32 {
    // inside
    foo()
}

pub struct S;

impl S {
    /// Doc for m.
    pub fn m(&self) {}
}
// trailing
`,
	"A.java": `package p;

/** Doc for A. */
public class A {
    /**
     * Doc for m.
     */
    public void m() {}

    // plain, with a blank line below

    int x;
}
// trailing
`,
}

// parseCommentSources parses commentSources with the pinned leyline and returns
// a walker over the result.
func parseCommentSources(t *testing.T) *ASTWalker {
	t.Helper()
	dir := t.TempDir()
	for name, src := range commentSources {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644))
	}
	db, err := sql.Open("sqlite", parseWithLeyline(t, dir))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	var comments int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM _ast a JOIN kinds k ON k.kind_id = a.kind_id
		WHERE `+lloschema.CommentKindSQL("k.raw_kind")).Scan(&comments))
	require.Positive(t, comments, "the parse must hold comment occurrences (leyline >= v0.29.0), or nothing here is tested")
	return NewASTWalker(db)
}

// docTarget returns the id of the one node of kind whose source starts with
// prefix.
func docTarget(t *testing.T, idx *fileIndex, kind, prefix string) (string, uint32) {
	t.Helper()
	var found []idxNode
	for _, i := range idx.byKind[kind] {
		n := idx.all[i]
		if strings.HasPrefix(string(idx.source[n.startByte:n.endByte]), prefix) {
			found = append(found, n)
		}
	}
	require.Len(t, found, 1, "one %s starting %q", kind, prefix)
	return found[0].id, uint32(found[0].startByte)
}

// TestFileIndex_CommentsOnARealParse is mache-1e3d11 against the real
// producer: every file with comments indexes (no node_child list holds a
// comment, so field assignment must not look for one), no comment is a node
// of the index, and a doc comment of any kind extends its construct.
func TestFileIndex_CommentsOnARealParse(t *testing.T) {
	w := parseCommentSources(t)

	docOf := func(file, kind, prefix string) string {
		idx, err := w.fileSection(file)
		require.NoError(t, err, "the index of %s must load with its comments", file)
		id, start := docTarget(t, idx, kind, prefix)
		return string(idx.source[idx.docExtendStart(id, start):start])
	}

	for file := range commentSources {
		idx, err := w.fileSection(file)
		require.NoError(t, err, "the index of %s must load with its comments", file)
		for _, n := range idx.all {
			assert.False(t, lloschema.IsCommentKind(n.astKind), "%s: comment %s is a node of the index", file, n.id)
		}
		var comments int
		for _, cs := range idx.comments {
			comments += len(cs)
		}
		assert.Positive(t, comments, "%s: its comments are held as comments", file)
	}

	assert.Equal(t, "// Helper is documented\n// on two lines.\n", docOf("a.go", "function_declaration", "func Helper"))
	assert.Equal(t, "/* Other has a block comment above it. */\n", docOf("a.go", "function_declaration", "func Other"))
	assert.Empty(t, docOf("a.go", "function_declaration", "func Spaced"), "a blank line ends a doc comment")
	assert.Equal(t, "// Package a has comments everywhere.\n", docOf("a.go", "package_clause", "package a"))

	assert.Equal(t, "/// Doc for foo.\n/// More.\n#[inline]\n", docOf("lib.rs", "function_item", "pub fn foo"),
		"a Rust line_comment run reaches its item over the attribute")
	assert.Equal(t, "/* block */\n", docOf("lib.rs", "function_item", "pub fn bar"))
	assert.Empty(t, docOf("lib.rs", "struct_item", "pub struct S"))
	assert.Equal(t, "/// Doc for m.\n    ", docOf("lib.rs", "function_item", "pub fn m"), "an indented method's doc")

	assert.Equal(t, "/** Doc for A. */\n", docOf("A.java", "class_declaration", "public class A"))
	assert.Equal(t, "/**\n     * Doc for m.\n     */\n    ", docOf("A.java", "method_declaration", "public void m"),
		"a Java block_comment directly above an indented method")
	assert.Empty(t, docOf("A.java", "field_declaration", "int x"), "a line_comment with a blank line below it")
}

// TestFileIndex_CommentsAreKnownByKindNotByEdges pins how a comment is
// identified: by its grammar kind (lloschema.IsCommentKind), never by "no
// node_edge reaches its content". node_content is deduplicated by node_hash,
// and a comment's row is hash_leaf(kind, text), the construction a folded
// leaf has too: Swift's member-separator multiline_comment is an ordinary
// child (ley-line-open-1b5f3c), and an extra with the same text shares its
// content row, which class_body's node_edge does reach.
func TestFileIndex_CommentsAreKnownByKindNotByEdges(t *testing.T) {
	// The first /* sep */ is an extra (a comment occurrence from v0.29.0);
	// the second separates two members and is an ordinary child.
	const src = "/* sep */\nfunc f() {}\nclass C {\n    var a = 1 /* sep */ var b = 2\n}\n"
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "c.swift"), []byte(src), 0o644))
	db, err := sql.Open("sqlite", parseWithLeyline(t, dir))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	var occurrences, contents, inbound int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT a.content_id),
		(SELECT COUNT(*) FROM node_edge e WHERE e.child_id = MIN(a.content_id))
		FROM _ast a JOIN kinds k ON k.kind_id = a.kind_id
		WHERE k.raw_kind = 'multiline_comment'`).Scan(&occurrences, &contents, &inbound))
	require.Equal(t, 2, occurrences, "the extra and the separator are both occurrences (leyline >= v0.29.0)")
	require.Equal(t, 1, contents, "with identical text they share one content row")
	require.Positive(t, inbound, "which a node_edge reaches: an edge test would call both code")

	w := NewASTWalker(db)
	idx, err := w.fileSection("c.swift")
	require.NoError(t, err)
	for _, n := range idx.all {
		assert.False(t, lloschema.IsCommentKind(n.astKind), "comment %s is a node of the index", n.id)
	}
	var held int
	for _, cs := range idx.comments {
		held += len(cs)
	}
	assert.Equal(t, 2, held, "both are held as comments")
	id, start := docTarget(t, idx, "function_declaration", "func f")
	assert.Equal(t, "/* sep */\n", string(idx.source[idx.docExtendStart(id, start):start]))
}
