package ingest

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// seedTestAST creates an in-memory SQLite database with the ley-line AST schema
// and populates it with a Go-like AST structure:
//
//	source_file/
//	  package_clause/
//	    package_identifier  ("main")
//	  function_declaration/
//	    identifier          ("Validate")
//	    parameter_list/     (dir)
//	    block/              (dir)
//	  function_declaration_1/
//	    identifier          ("Helper")
//	    parameter_list/     (dir)
//	    block/              (dir)
//	  type_declaration/
//	    type_spec/
//	      type_identifier   ("Config")
func seedTestAST(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)

	_, err = db.Exec(`
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY,
			parent_id TEXT,
			name TEXT NOT NULL,
			kind INTEGER NOT NULL,
			size INTEGER DEFAULT 0,
			mtime INTEGER NOT NULL,
			record_id TEXT,
			record JSON,
			source_file TEXT
		);
		CREATE INDEX idx_parent_name ON nodes(parent_id, name);

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
		CREATE INDEX idx_ast_source ON _ast(source_id);

		CREATE TABLE _source (
			id TEXT PRIMARY KEY,
			language TEXT NOT NULL,
			content BLOB NOT NULL
		);
	`)
	require.NoError(t, err)

	// Source content
	src := `package main

func Validate(x int) error {
	return nil
}

func Helper() string {
	return "ok"
}

type Config struct {
	Name string
}
`
	_, err = db.Exec("INSERT INTO _source VALUES (?, ?, ?)", "main.go", "go", []byte(src))
	require.NoError(t, err)

	// nodes table (ley-line projection format)
	nodes := []struct {
		id, parentID, name string
		kind               int
		record             string
	}{
		{"", "", "", 1, ""},
		{"source_file", "", "source_file", 1, ""},
		// First function
		{"source_file/function_declaration", "source_file", "function_declaration", 1, ""},
		{"source_file/function_declaration/identifier", "source_file/function_declaration", "identifier", 0, "Validate"},
		{"source_file/function_declaration/parameter_list", "source_file/function_declaration", "parameter_list", 1, ""},
		{"source_file/function_declaration/block", "source_file/function_declaration", "block", 1, ""},
		// Second function (disambiguated name)
		{"source_file/function_declaration_1", "source_file", "function_declaration_1", 1, ""},
		{"source_file/function_declaration_1/identifier", "source_file/function_declaration_1", "identifier", 0, "Helper"},
		{"source_file/function_declaration_1/parameter_list", "source_file/function_declaration_1", "parameter_list", 1, ""},
		{"source_file/function_declaration_1/block", "source_file/function_declaration_1", "block", 1, ""},
		// Type declaration
		{"source_file/type_declaration", "source_file", "type_declaration", 1, ""},
		{"source_file/type_declaration/type_spec", "source_file/type_declaration", "type_spec", 1, ""},
		{"source_file/type_declaration/type_spec/type_identifier", "source_file/type_declaration/type_spec", "type_identifier", 0, "Config"},
		// Package clause
		{"source_file/package_clause", "source_file", "package_clause", 1, ""},
		{"source_file/package_clause/package_identifier", "source_file/package_clause", "package_identifier", 0, "main"},
	}

	for _, n := range nodes {
		_, err := db.Exec(
			"INSERT INTO nodes (id, parent_id, name, kind, size, mtime, record) VALUES (?, ?, ?, ?, 0, 0, ?)",
			n.id, n.parentID, n.name, n.kind, n.record,
		)
		require.NoError(t, err, "insert node %s", n.id)
	}

	// _ast table (byte ranges — approximate for test purposes)
	astRows := []struct {
		nodeID, kind string
		startByte    int
		endByte      int
	}{
		{"source_file/function_declaration", "function_declaration", 14, 64},
		{"source_file/function_declaration/identifier", "identifier", 19, 27},
		{"source_file/function_declaration_1", "function_declaration", 66, 104},
		{"source_file/function_declaration_1/identifier", "identifier", 71, 77},
		{"source_file/type_declaration", "type_declaration", 106, 141},
		{"source_file/type_declaration/type_spec", "type_spec", 111, 141},
		{"source_file/type_declaration/type_spec/type_identifier", "type_identifier", 116, 122},
	}
	for _, a := range astRows {
		_, err := db.Exec(
			"INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col) VALUES (?, 'main.go', ?, ?, ?, 0, 0, 0, 0)",
			a.nodeID, a.kind, a.startByte, a.endByte,
		)
		require.NoError(t, err, "insert _ast %s", a.nodeID)
	}

	return db
}

func TestASTWalker_QueryFunctionDeclarations(t *testing.T) {
	db := seedTestAST(t)
	defer func() { _ = db.Close() }()

	w := NewASTWalker(db)
	root := ASTRoot{DB: db, SourceID: "main.go", ParentPrefix: ""}

	matches, err := w.Query(root, "(function_declaration name: (identifier) @name) @scope")
	require.NoError(t, err)
	require.Len(t, matches, 2, "should find 2 function declarations")

	names := make([]string, len(matches))
	for i, m := range matches {
		v := m.Values()
		names[i], _ = v["name"].(string)
	}
	assert.Contains(t, names, "Validate")
	assert.Contains(t, names, "Helper")
}

func TestASTWalker_QueryTypeDeclarations(t *testing.T) {
	db := seedTestAST(t)
	defer func() { _ = db.Close() }()

	w := NewASTWalker(db)
	root := ASTRoot{DB: db, SourceID: "main.go", ParentPrefix: ""}

	matches, err := w.Query(root, "(type_declaration (type_spec name: (type_identifier) @name) @scope)")
	require.NoError(t, err)
	require.Len(t, matches, 1)

	v := matches[0].Values()
	assert.Equal(t, "Config", v["name"])
}

func TestASTWalker_ContextScopesSubtree(t *testing.T) {
	db := seedTestAST(t)
	defer func() { _ = db.Close() }()

	w := NewASTWalker(db)
	root := ASTRoot{DB: db, SourceID: "main.go", ParentPrefix: ""}

	// First find function declarations
	matches, err := w.Query(root, "(function_declaration name: (identifier) @name) @scope")
	require.NoError(t, err)
	require.NotEmpty(t, matches)

	// Context() should return an ASTRoot scoped to the matched node
	ctx := matches[0].Context()
	ar, ok := ctx.(ASTRoot)
	require.True(t, ok)
	assert.Contains(t, ar.ParentPrefix, "function_declaration")
}

func TestASTWalker_CaptureOrigin(t *testing.T) {
	db := seedTestAST(t)
	defer func() { _ = db.Close() }()

	w := NewASTWalker(db)
	root := ASTRoot{DB: db, SourceID: "main.go", ParentPrefix: ""}

	matches, err := w.Query(root, "(function_declaration name: (identifier) @name) @scope")
	require.NoError(t, err)
	require.NotEmpty(t, matches)

	// OriginProvider should return byte ranges for @scope
	op, ok := matches[0].(OriginProvider)
	require.True(t, ok)

	start, end, ok := op.CaptureOrigin("scope")
	assert.True(t, ok)
	assert.True(t, start < end, "scope should have valid byte range")
}

func TestASTWalker_PredicateSelectorsReturnError(t *testing.T) {
	db := seedTestAST(t)
	defer func() { _ = db.Close() }()

	w := NewASTWalker(db)
	root := ASTRoot{DB: db, SourceID: "main.go", ParentPrefix: ""}

	// Predicate selectors require SitterWalker (CGO)
	_, err := w.Query(root, `(block (identifier) @_type (#eq? @_type "resource"))`)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "predicate")
}

func TestParseSelector_Simple(t *testing.T) {
	p, err := parseSelector("(function_declaration name: (identifier) @name) @scope")
	require.NoError(t, err)
	assert.Equal(t, "function_declaration", p.outerKind)
	require.Len(t, p.captures, 1)
	assert.Equal(t, "identifier", p.captures[0].kind)
	assert.Equal(t, "name", p.captures[0].name)
}

func TestParseSelector_Nested(t *testing.T) {
	p, err := parseSelector("(type_declaration (type_spec name: (type_identifier) @name) @scope)")
	require.NoError(t, err)
	assert.Equal(t, "type_declaration", p.outerKind)
	require.Len(t, p.captures, 1)
	assert.Equal(t, "type_identifier", p.captures[0].kind)
	assert.Equal(t, "name", p.captures[0].name)
}
