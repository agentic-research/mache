package ingest

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// seedGroupedTypeAST builds an _ast for a single grouped Go declaration:
//
//	type ( Alpha int; Beta string )
//
// one type_declaration containing TWO type_spec nodes (leyline suffixes the
// second as type_spec_1), each with a type_identifier name. This is the shape
// that exercises the inner-@scope path in ASTWalker.Query.
func seedGroupedTypeAST(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	_, err = db.Exec(`
		CREATE TABLE nodes (id TEXT PRIMARY KEY, parent_id TEXT, name TEXT NOT NULL,
			kind INTEGER NOT NULL, size INTEGER DEFAULT 0, mtime INTEGER NOT NULL,
			record_id TEXT, record TEXT, source_file TEXT);
		CREATE TABLE _ast (node_id TEXT PRIMARY KEY, source_id TEXT NOT NULL,
			node_kind TEXT NOT NULL, start_byte INTEGER NOT NULL, end_byte INTEGER NOT NULL,
			start_row INTEGER NOT NULL, start_col INTEGER NOT NULL,
			end_row INTEGER NOT NULL, end_col INTEGER NOT NULL);
		CREATE INDEX idx_ast_source ON _ast(source_id);
		CREATE TABLE _source (id TEXT PRIMARY KEY, language TEXT NOT NULL, content BLOB, path TEXT);
	`)
	require.NoError(t, err)

	src := "package main\n\ntype (\n\tAlpha int\n\tBeta string\n)\n"
	_, err = db.Exec(`INSERT INTO _source (id, language, content, path) VALUES ('main.go','go',?,NULL)`, []byte(src))
	require.NoError(t, err)

	// id, node_kind, record, start, end
	rows := []struct {
		id, kind, record string
		start, end       int
	}{
		{"main.go/type_declaration", "type_declaration", "", 14, 46},
		{"main.go/type_declaration/type_spec", "type_spec", "", 22, 31},
		{"main.go/type_declaration/type_spec/type_identifier", "type_identifier", "Alpha", 22, 27},
		{"main.go/type_declaration/type_spec_1", "type_spec", "", 33, 44},
		{"main.go/type_declaration/type_spec_1/type_identifier", "type_identifier", "Beta", 33, 37},
	}
	for _, r := range rows {
		_, err = db.Exec(`INSERT INTO nodes (id, parent_id, name, kind, mtime, record, source_file)
			VALUES (?,?,?,0,0,?, 'main.go')`, r.id, parentOf(r.id), leaf(r.id), r.record)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte,
			start_row, start_col, end_row, end_col) VALUES (?, 'main.go', ?, ?, ?, 0,0,0,0)`,
			r.id, r.kind, r.start, r.end)
		require.NoError(t, err)
	}
	return db
}

func parentOf(id string) string {
	i := lastSlash(id)
	if i < 0 {
		return ""
	}
	return id[:i]
}

func leaf(id string) string {
	i := lastSlash(id)
	return id[i+1:]
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

// TestASTWalker_Query_InnerScope_OneMatchPerInner locks the semantics that
// rode in on the mache-4f3840 perf commit: when a selector's @scope is an
// INNER node kind that occurs multiple times under one outer match (grouped
// `type ( A; B )`), Query yields ONE match per inner node — mirroring
// tree-sitter — not just the first. A regression to one-match-per-outer would
// silently drop every grouped-declaration member after the first.
func TestASTWalker_Query_InnerScope_OneMatchPerInner(t *testing.T) {
	db := seedGroupedTypeAST(t)
	defer func() { _ = db.Close() }()
	w := NewASTWalker(db)

	root := ASTRoot{DB: db, SourceID: "main.go", ParentPrefix: ""}
	matches, err := w.Query(root, "(type_declaration (type_spec (type_identifier) @name) @scope)")
	require.NoError(t, err)
	require.Len(t, matches, 2, "grouped type_declaration must project one match per inner type_spec")

	var names []string
	for _, m := range matches {
		if n, ok := m.Values()["name"].(string); ok {
			names = append(names, n)
		}
	}
	assert.ElementsMatch(t, []string{"Alpha", "Beta"}, names,
		"each grouped member resolves its own @name")
}
