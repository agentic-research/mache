package lloschema_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/lloschema"
	"github.com/agentic-research/mache/internal/lltest"

	_ "modernc.org/sqlite"
)

// TestCommentKind_GoAndSQLAgree pins the one comment predicate in both of its
// encodings over every comment kind the grammars have today and kinds that
// are not comments.
func TestCommentKind_GoAndSQLAgree(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	for kind, want := range map[string]bool{
		"comment": true, "line_comment": true, "block_comment": true,
		"html_comment": true, "multiline_comment": true, "js_comment": true,
		"function_declaration": false, "line_continuation": false,
		"string_literal": false, "Comment": false, "": false,
	} {
		assert.Equal(t, want, lloschema.IsCommentKind(kind), "Go: %q", kind)
		var got bool
		require.NoError(t, db.QueryRow(`SELECT `+lloschema.CommentKindSQL("?"), kind).Scan(&got))
		assert.Equal(t, want, got, "SQL: %q", kind)
	}
}

// commentsSource has a comment above the first function, one in a body,
// one between functions, and one after the last.
const commentsSource = `// Package c is first.
package c

// First is documented.
func First() int {
	// inside First
	return 1
}

// Between.
func Last() int {
	if true {
		return 2 // after a statement
	}
	return 3
}
// after the last
`

// TestASTSelect_CommentsAreInsideOnlyTheirAncestors is mache-1e3d11's v_ast
// half, on a real parse. From projection-v13 a file's comments are numbered
// after all its code, so a nid range says nothing about them. subtree_lo/hi
// are the extent over code: a code node's range holds exactly its code
// descendants and no comment, and a comment's own range is empty. A comment is
// inside a node exactly when its parent (v_nodes.parent_id, nodes.parent_nid)
// is that node or inside its range, which must give each comment exactly its
// real ancestors.
func TestASTSelect_CommentsAreInsideOnlyTheirAncestors(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "c.go"), []byte(commentsSource), 0o644))
	db, _ := lltest.ParseSourceViaLeyline(t, src)
	body, err := lloschema.ASTSelect(lloschema.DB(db))
	require.NoError(t, err)

	type extent struct {
		lo, hi, parent int64
		comment        bool
	}
	nodes := map[int64]extent{}
	rows, err := db.Query(`SELECT a.node_id, a.subtree_lo, a.subtree_hi, a.is_comment, n.parent_nid
		FROM (` + body + `) a JOIN nodes n ON n.nid = a.node_id`)
	require.NoError(t, err)
	for rows.Next() {
		var id int64
		var r extent
		require.NoError(t, rows.Scan(&id, &r.lo, &r.hi, &r.comment, &r.parent))
		nodes[id] = r
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())

	ancestors := func(id int64) map[int64]bool {
		out := map[int64]bool{}
		for p := nodes[id].parent; ; p = nodes[p].parent {
			if _, ok := nodes[p]; !ok {
				return out
			}
			out[p] = true
		}
	}
	comments := 0
	for id, r := range nodes {
		real := ancestors(id)
		byRange := map[int64]bool{}
		for a, ar := range nodes {
			if ar.lo <= id && id < ar.hi {
				byRange[a] = true
			}
		}
		if !r.comment {
			assert.Equal(t, real, byRange, "code node %d: the ranges that hold it are its ancestors", id)
			continue
		}
		comments++
		assert.Empty(t, byRange, "comment %d is in no code range: its nid is past the file's code", id)
		assert.Equal(t, r.lo, r.hi, "comment %d is terminal: its range is empty", id)
		byParent := map[int64]bool{}
		for a, ar := range nodes {
			if a == r.parent || (ar.lo <= r.parent && r.parent < ar.hi) {
				byParent[a] = true
			}
		}
		assert.Equal(t, real, byParent, "comment %d: through its parent it is inside exactly its ancestors", id)
	}
	assert.Equal(t, 6, comments, "every comment of the source is an occurrence (leyline >= v0.29.0)")
}

// TestCommentKindIDs_AndNotCommentNode, on a real parse: CommentKindIDs is
// every kind IsCommentKind names, and NotCommentNode keeps every nodes row but
// the comments, directories (no kind_id) included.
func TestCommentKindIDs_AndNotCommentNode(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(src, "pkg"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "pkg", "c.go"), []byte(commentsSource), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "lib.rs"), []byte("/// Doc.\nfn f() {}\n/* end */\n"), 0o644))
	db, _ := lltest.ParseSourceViaLeyline(t, src)

	var want, got []string
	rows, err := db.Query(`SELECT raw_kind FROM kinds ORDER BY raw_kind`)
	require.NoError(t, err)
	for rows.Next() {
		var k string
		require.NoError(t, rows.Scan(&k))
		if lloschema.IsCommentKind(k) {
			want = append(want, k)
		}
	}
	require.NoError(t, rows.Close())
	rows, err = db.Query(`SELECT raw_kind FROM kinds WHERE kind_id IN ` + lloschema.CommentKindIDs() + ` ORDER BY raw_kind`)
	require.NoError(t, err)
	for rows.Next() {
		var k string
		require.NoError(t, rows.Scan(&k))
		got = append(got, k)
	}
	require.NoError(t, rows.Close())
	require.ElementsMatch(t, []string{"block_comment", "comment", "line_comment"}, want, "the parse holds three comment kinds")
	assert.Equal(t, want, got)

	var all, kept, comments, dirsKept int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&all))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM nodes n WHERE `+lloschema.NotCommentNode("n")).Scan(&kept))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM nodes n JOIN kinds k ON k.kind_id = n.kind_id
		WHERE `+lloschema.CommentKindSQL("k.raw_kind")).Scan(&comments))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM nodes n WHERE n.kind_id IS NULL AND `+
		lloschema.NotCommentNode("n")).Scan(&dirsKept))
	assert.Equal(t, 8, comments, "six comments in c.go, two in lib.rs")
	assert.Equal(t, all-comments, kept, "every row but the comments")
	assert.Positive(t, dirsKept, "directories carry no kind and are kept")
}
