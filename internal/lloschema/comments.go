package lloschema

import "strings"

// Comments are _ast and nodes occurrences from projection-v13 on (leyline
// v0.29.0, extraction epoch 7, ley-line-open-aa1634). A comment occurrence is
// a terminal leaf: no node_edge reaches it, it is in no other node's
// node_hash, and its nid comes after every code nid of its file, so nid order
// is document order among code rows only (TABLE_CONTRACT, "Comments at
// projection-v13").
//
// mache folds them: no listing, overview or mount shows a comment as a node,
// and a doc comment extends the construct it documents instead
// (mache-1e3d11). Everything that has to tell a comment apart reads this one
// predicate, in Go or in SQL.
//
// A comment is an occurrence whose grammar kind contains "comment". That is
// LLO's own test (cmd_parse.rs is_comment_extra), and its contract says to
// match the kinds that way rather than by a list; the grammars name them
// comment, line_comment, block_comment, html_comment, multiline_comment and
// js_comment today.

// commentWord is the substring every comment kind contains.
const commentWord = "comment"

// IsCommentKind reports whether rawKind, a grammar kind as kinds.raw_kind
// stores it, is a comment's.
func IsCommentKind(rawKind string) bool {
	return strings.Contains(rawKind, commentWord)
}

// CommentKindSQL is IsCommentKind as a SQL condition on expr, an expression
// holding a raw kind. instr, not LIKE: LIKE folds ASCII case and
// strings.Contains does not, and the two must agree.
func CommentKindSQL(expr string) string {
	return "instr(" + expr + ", '" + commentWord + "') > 0"
}

// CommentKindIDs is the set of kinds.kind_id values that are comments, as a
// subquery for `kind_id IN (...)`. It does not depend on the outer row, so
// SQLite evaluates it once per statement.
func CommentKindIDs() string {
	return "(SELECT kind_id FROM kinds WHERE " + CommentKindSQL("raw_kind") + ")"
}

// NotCommentNode is a SQL condition on a nodes row aliased n that holds unless
// the row is a comment occurrence. A directory has no kind_id and is not one.
func NotCommentNode(n string) string {
	return "(" + n + ".kind_id IS NULL OR " + n + ".kind_id NOT IN " + CommentKindIDs() + ")"
}
