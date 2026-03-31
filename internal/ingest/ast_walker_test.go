package ingest

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
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

// seedTestASTFile creates the same test data as seedTestAST but in a temp file
// database. Required for concurrent tests — :memory: gives each pool connection
// its own isolated database, so concurrent queries see "no such table".
func seedTestASTFile(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test_ast.db")
	db, err := sql.Open("sqlite", dbPath)
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

	nodes := []struct {
		id, parentID, name string
		kind               int
		record             string
	}{
		{"", "", "", 1, ""},
		{"source_file", "", "source_file", 1, ""},
		{"source_file/function_declaration", "source_file", "function_declaration", 1, ""},
		{"source_file/function_declaration/identifier", "source_file/function_declaration", "identifier", 0, "Validate"},
		{"source_file/function_declaration/parameter_list", "source_file/function_declaration", "parameter_list", 1, ""},
		{"source_file/function_declaration/block", "source_file/function_declaration", "block", 1, ""},
		{"source_file/function_declaration_1", "source_file", "function_declaration_1", 1, ""},
		{"source_file/function_declaration_1/identifier", "source_file/function_declaration_1", "identifier", 0, "Helper"},
		{"source_file/function_declaration_1/parameter_list", "source_file/function_declaration_1", "parameter_list", 1, ""},
		{"source_file/function_declaration_1/block", "source_file/function_declaration_1", "block", 1, ""},
		{"source_file/type_declaration", "source_file", "type_declaration", 1, ""},
		{"source_file/type_declaration/type_spec", "source_file/type_declaration", "type_spec", 1, ""},
		{"source_file/type_declaration/type_spec/type_identifier", "source_file/type_declaration/type_spec", "type_identifier", 0, "Config"},
		{"source_file/package_clause", "source_file", "package_clause", 1, ""},
		{"source_file/package_clause/package_identifier", "source_file/package_clause", "package_identifier", 0, "main"},
	}
	for _, n := range nodes {
		_, err := db.Exec(
			"INSERT INTO nodes (id, parent_id, name, kind, size, mtime, record) VALUES (?, ?, ?, ?, 0, 0, ?)",
			n.id, n.parentID, n.name, n.kind, n.record,
		)
		require.NoError(t, err)
	}

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
		require.NoError(t, err)
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

func TestASTWalker_PredicateEqFilter(t *testing.T) {
	// Build a DB with HCL-like block structure
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`
		CREATE TABLE nodes (id TEXT PRIMARY KEY, parent_id TEXT, name TEXT NOT NULL, kind INTEGER NOT NULL, size INTEGER DEFAULT 0, mtime INTEGER NOT NULL, record_id TEXT, record JSON, source_file TEXT);
		CREATE INDEX idx_parent_name ON nodes(parent_id, name);
		CREATE TABLE _ast (node_id TEXT PRIMARY KEY, source_id TEXT NOT NULL, node_kind TEXT NOT NULL, start_byte INTEGER NOT NULL, end_byte INTEGER NOT NULL, start_row INTEGER, start_col INTEGER, end_row INTEGER, end_col INTEGER);
		CREATE TABLE _source (id TEXT PRIMARY KEY, language TEXT NOT NULL, content BLOB NOT NULL);

		INSERT INTO _source VALUES ('main.tf', 'hcl', '');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, record) VALUES ('', '', '', 1, 0, '');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, record) VALUES ('block', '', 'block', 1, 0, '');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, record) VALUES ('block/identifier', 'block', 'identifier', 0, 0, 'resource');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, record) VALUES ('block/string_lit', 'block', 'string_lit', 0, 0, '"aws_instance"');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, record) VALUES ('block/body', 'block', 'body', 1, 0, '');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, record) VALUES ('block_1', '', 'block_1', 1, 0, '');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, record) VALUES ('block_1/identifier', 'block_1', 'identifier', 0, 0, 'variable');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, record) VALUES ('block_1/string_lit', 'block_1', 'string_lit', 0, 0, '"region"');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, record) VALUES ('block_1/body', 'block_1', 'body', 1, 0, '');

		INSERT INTO _ast VALUES ('block', 'main.tf', 'block', 0, 50, 0, 0, 0, 0);
		INSERT INTO _ast VALUES ('block/identifier', 'main.tf', 'identifier', 0, 8, 0, 0, 0, 0);
		INSERT INTO _ast VALUES ('block/string_lit', 'main.tf', 'string_lit', 9, 23, 0, 0, 0, 0);
		INSERT INTO _ast VALUES ('block/body', 'main.tf', 'body', 24, 50, 0, 0, 0, 0);
		INSERT INTO _ast VALUES ('block_1', 'main.tf', 'block', 52, 100, 0, 0, 0, 0);
		INSERT INTO _ast VALUES ('block_1/identifier', 'main.tf', 'identifier', 52, 60, 0, 0, 0, 0);
		INSERT INTO _ast VALUES ('block_1/string_lit', 'main.tf', 'string_lit', 61, 69, 0, 0, 0, 0);
		INSERT INTO _ast VALUES ('block_1/body', 'main.tf', 'body', 70, 100, 0, 0, 0, 0);
	`)
	require.NoError(t, err)

	w := NewASTWalker(db)
	root := ASTRoot{DB: db, SourceID: "main.tf", ParentPrefix: ""}

	// Should match only the "resource" block, not the "variable" block
	matches, err := w.Query(root, `(block (identifier) @_type (string_lit) @name (body) @scope (#eq? @_type "resource"))`)
	require.NoError(t, err)
	require.Len(t, matches, 1, "should match only the resource block")

	v := matches[0].Values()
	assert.Equal(t, "resource", v["_type"])
	assert.Equal(t, "\"aws_instance\"", v["name"])
}

func TestASTWalker_MatchPredicateRejectsNonMatch(t *testing.T) {
	// #match? predicates still require CGO
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	w := NewASTWalker(db)
	root := ASTRoot{DB: db, SourceID: "", ParentPrefix: ""}

	_, err = w.Query(root, `(identifier) @name (#match? @name "^test")`)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "#match?")
}

func TestSelectWalker_ReturnsASTWalkerWhenASTTableExists(t *testing.T) {
	db := seedTestAST(t)
	defer func() { _ = db.Close() }()

	w, err := SelectWalker(db)
	require.NoError(t, err)
	_, ok := w.(*ASTWalker)
	assert.True(t, ok, "should return ASTWalker when _ast table exists")
}

func TestSelectWalker_ReturnsSitterWalkerWhenNoASTTable(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// Just nodes table, no _ast
	_, err = db.Exec(`CREATE TABLE nodes (id TEXT PRIMARY KEY, name TEXT)`)
	require.NoError(t, err)

	w, err := SelectWalker(db)
	require.NoError(t, err)
	_, ok := w.(*SitterWalker)
	assert.True(t, ok, "should return SitterWalker when _ast table missing")
}

func TestSelectWalker_ReturnsErrorOnBrokenDB(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	_ = db.Close() // close it so queries fail

	_, err = SelectWalker(db)
	assert.Error(t, err, "should return error on broken DB")
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

// ---------------------------------------------------------------------------
// Edge case tests — ASTWalker fidelity vs SitterWalker.
// ---------------------------------------------------------------------------

// TestParseSelector_MultiplePredicates verifies that the #eq? extraction
// correctly handles multiple predicates in a single selector.
func TestParseSelector_MultiplePredicates(t *testing.T) {
	// Terraform-like: match blocks where _type="resource" AND _provider="aws"
	selector := `(block (identifier) @_type (identifier) @_provider (string_lit) @name (body) @scope (#eq? @_type "resource") (#eq? @_provider "aws"))`
	p, err := parseSelector(selector)
	require.NoError(t, err, "should parse selector with two #eq? predicates")
	require.Len(t, p.predicates, 2, "should extract both #eq? predicates")

	// Verify both predicates are correctly extracted
	predMap := map[string]string{}
	for _, pred := range p.predicates {
		predMap[pred.capture] = pred.literal
	}
	assert.Equal(t, "resource", predMap["_type"], "first predicate")
	assert.Equal(t, "aws", predMap["_provider"], "second predicate")

	// Stress test: three predicates — does the shrinking-string loop handle it?
	sel3 := `(block (identifier) @a (identifier) @b (identifier) @c) @scope (#eq? @a "x") (#eq? @b "y") (#eq? @c "z")`
	p3, err := parseSelector(sel3)
	require.NoError(t, err, "should parse selector with three #eq? predicates")
	require.Len(t, p3.predicates, 3, "should extract all three #eq? predicates")
	pred3Map := map[string]string{}
	for _, pred := range p3.predicates {
		pred3Map[pred.capture] = pred.literal
	}
	assert.Equal(t, "x", pred3Map["a"], "first of three predicates")
	assert.Equal(t, "y", pred3Map["b"], "second of three predicates")
	assert.Equal(t, "z", pred3Map["c"], "third of three predicates")
}

// TestASTWalker_NotEqPredicateSilentlyIgnored verifies that unsupported
// predicates (#not-eq?, #any-eq?, #is?, #is-not?) are rejected with an
// error rather than silently ignored.
func TestASTWalker_NotEqPredicateSilentlyIgnored(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`
		CREATE TABLE nodes (id TEXT PRIMARY KEY, parent_id TEXT, name TEXT NOT NULL, kind INTEGER NOT NULL, size INTEGER DEFAULT 0, mtime INTEGER NOT NULL, record_id TEXT, record JSON, source_file TEXT);
		CREATE TABLE _ast (node_id TEXT PRIMARY KEY, source_id TEXT NOT NULL, node_kind TEXT NOT NULL, start_byte INTEGER NOT NULL, end_byte INTEGER NOT NULL, start_row INTEGER, start_col INTEGER, end_row INTEGER, end_col INTEGER);
		CREATE TABLE _source (id TEXT PRIMARY KEY, language TEXT NOT NULL, content BLOB NOT NULL);

		INSERT INTO _source VALUES ('test.tf', 'hcl', '');
		INSERT INTO nodes VALUES ('', '', '', 1, 0, 0, NULL, '', NULL);
		INSERT INTO nodes VALUES ('block', '', 'block', 1, 0, 0, NULL, '', NULL);
		INSERT INTO nodes VALUES ('block/identifier', 'block', 'identifier', 0, 0, 0, NULL, 'variable', NULL);
		INSERT INTO _ast VALUES ('block', 'test.tf', 'block', 0, 50, 0, 0, 0, 0);
		INSERT INTO _ast VALUES ('block/identifier', 'test.tf', 'identifier', 0, 8, 0, 0, 0, 0);
	`)
	require.NoError(t, err)

	w := NewASTWalker(db)
	root := ASTRoot{DB: db, SourceID: "test.tf", ParentPrefix: ""}

	// #not-eq? should be rejected — ASTWalker only supports #eq?.
	_, err = w.Query(root, `(block (identifier) @_type) @scope (#not-eq? @_type "variable")`)
	require.Error(t, err, "should reject #not-eq? predicate")
	assert.Contains(t, err.Error(), "#not-eq?")
	assert.Contains(t, err.Error(), "SitterWalker")
}

// TestASTWalker_CaptureOriginNamedCapture verifies that CaptureOrigin returns
// byte ranges for named captures (e.g., @name), not just @scope. The _ast table
// has start_byte/end_byte for every node — these are stored in astMatch.captureRanges.
func TestASTWalker_CaptureOriginNamedCapture(t *testing.T) {
	db := seedTestAST(t)
	defer func() { _ = db.Close() }()

	w := NewASTWalker(db)
	root := ASTRoot{DB: db, SourceID: "main.go", ParentPrefix: ""}

	matches, err := w.Query(root, "(function_declaration name: (identifier) @name) @scope")
	require.NoError(t, err)
	require.NotEmpty(t, matches)

	op, ok := matches[0].(OriginProvider)
	require.True(t, ok, "astMatch should implement OriginProvider")

	// @scope works (existing test confirms this)
	_, _, scopeOK := op.CaptureOrigin("scope")
	assert.True(t, scopeOK, "@scope should return byte ranges")

	// @name should also return byte ranges — the _ast table has start_byte/end_byte
	// for every node. Now stored in astMatch.captureRanges during Query.
	start, end, nameOK := op.CaptureOrigin("name")
	require.True(t, nameOK, "CaptureOrigin(\"name\") should return true — byte ranges from _ast table")
	assert.True(t, start < end, "@name should have valid byte range (got %d-%d)", start, end)

	// Unknown captures should still return false
	_, _, unknownOK := op.CaptureOrigin("nonexistent")
	assert.False(t, unknownOK, "unknown capture should return false")
}

// TestASTWalker_MultipleChildrenSameKind documents the LIMIT 1 behavior in
// findChildByKindAST. When a parent has multiple descendants of the same
// node_kind (e.g., two string_lit children in an HCL block), only the first
// by start_byte is returned. Tree-sitter distinguishes by field name;
// ASTWalker only matches by node_kind.
func TestASTWalker_MultipleChildrenSameKind(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// Simulate: resource "aws_instance" "my_server" { ... }
	// Two string_lit children under the same block.
	_, err = db.Exec(`
		CREATE TABLE nodes (id TEXT PRIMARY KEY, parent_id TEXT, name TEXT NOT NULL, kind INTEGER NOT NULL, size INTEGER DEFAULT 0, mtime INTEGER NOT NULL, record_id TEXT, record JSON, source_file TEXT);
		CREATE TABLE _ast (node_id TEXT PRIMARY KEY, source_id TEXT NOT NULL, node_kind TEXT NOT NULL, start_byte INTEGER NOT NULL, end_byte INTEGER NOT NULL, start_row INTEGER, start_col INTEGER, end_row INTEGER, end_col INTEGER);
		CREATE TABLE _source (id TEXT PRIMARY KEY, language TEXT NOT NULL, content BLOB NOT NULL);

		INSERT INTO _source VALUES ('main.tf', 'hcl', 'resource "aws_instance" "my_server" {}');
		INSERT INTO nodes VALUES ('', '', '', 1, 0, 0, NULL, '', NULL);
		INSERT INTO nodes VALUES ('block', '', 'block', 1, 0, 0, NULL, '', NULL);
		INSERT INTO nodes VALUES ('block/identifier', 'block', 'identifier', 0, 0, 0, NULL, 'resource', NULL);
		INSERT INTO nodes VALUES ('block/string_lit', 'block', 'string_lit', 0, 0, 0, NULL, '"aws_instance"', NULL);
		INSERT INTO nodes VALUES ('block/string_lit_1', 'block', 'string_lit_1', 0, 0, 0, NULL, '"my_server"', NULL);

		INSERT INTO _ast VALUES ('block', 'main.tf', 'block', 0, 38, 0, 0, 0, 0);
		INSERT INTO _ast VALUES ('block/identifier', 'main.tf', 'identifier', 0, 8, 0, 0, 0, 0);
		INSERT INTO _ast VALUES ('block/string_lit', 'main.tf', 'string_lit', 9, 23, 0, 0, 0, 0);
		INSERT INTO _ast VALUES ('block/string_lit_1', 'main.tf', 'string_lit', 24, 35, 0, 0, 0, 0);
	`)
	require.NoError(t, err)

	w := NewASTWalker(db)
	root := ASTRoot{DB: db, SourceID: "main.tf", ParentPrefix: ""}

	// Schema captures two string_lit children with different capture names.
	// But ASTWalker's parseSelector maps capture→kind, not capture→position.
	// Both @res_type and @res_name look for kind=string_lit, and LIMIT 1
	// means one gets the first row and the other... also gets the first row.
	//
	// This is a simplified reproduction. The real issue: if a schema has
	// TWO captures of the same kind (uncommon but valid in tree-sitter),
	// ASTWalker returns the same node for both.
	matches, err := w.Query(root, `(block (identifier) @_type (string_lit) @name) @scope (#eq? @_type "resource")`)
	require.NoError(t, err)
	require.Len(t, matches, 1)

	v := matches[0].Values()
	name, _ := v["name"].(string)
	// ORDER BY start_byte ASC ensures we get the first child deterministically.
	assert.Equal(t, "\"aws_instance\"", name, "should capture first string_lit by document order")

	// Second string_lit ("my_server") is not separately addressable —
	// ASTWalker matches by node_kind, not by field name.
}

// ---------------------------------------------------------------------------
// Fuzz tests — parseSelector is a hand-rolled parser operating on untrusted
// schema selectors. Fuzz it to find panics, infinite loops, and malformed
// output from adversarial inputs.
// ---------------------------------------------------------------------------

// FuzzParseSelector feeds random strings into parseSelector to find panics
// and hangs. The parser does string slicing, index arithmetic, and in-place
// mutation — all classic fuzz targets.
func FuzzParseSelector(f *testing.F) {
	// Seed with real selectors from mache schemas
	f.Add(`(function_declaration name: (identifier) @name) @scope`)
	f.Add(`(type_declaration (type_spec name: (type_identifier) @name) @scope)`)
	f.Add(`(block (identifier) @_type (string_lit) @name (body) @scope (#eq? @_type "resource"))`)
	f.Add(`(block (identifier) @a) @scope (#eq? @a "x") (#eq? @a "y") (#eq? @a "z")`)
	// Edge cases
	f.Add(``)
	f.Add(`(`)
	f.Add(`)`)
	f.Add(`@scope`)
	f.Add(`((()))`)
	f.Add(`(foo (#eq? @bar "baz") (#eq? @qux ""))`)
	f.Add(`(foo) @scope (#match? @foo "^test")`)
	f.Add(`(foo (#not-eq? @bar "x"))`)
	// Adversarial: deeply nested, huge, repeated @
	f.Add(`(` + strings.Repeat("(a ", 100) + strings.Repeat(") ", 100) + `@name) @scope`)
	f.Add(strings.Repeat(`@x `, 200))

	f.Fuzz(func(t *testing.T, selector string) {
		// Must not panic. Errors are fine.
		p, err := parseSelector(selector)
		if err != nil {
			return
		}
		// Basic sanity: if it parsed, outerKind should be non-empty
		if p.outerKind == "" {
			t.Errorf("parseSelector returned nil error but outerKind is empty for: %q", selector)
		}
		// Captures should have non-empty kind and name
		for i, c := range p.captures {
			if c.kind == "" || c.name == "" {
				t.Errorf("capture[%d] has empty kind=%q or name=%q for: %q", i, c.kind, c.name, selector)
			}
		}
		// Predicates should have non-empty fields
		for i, pred := range p.predicates {
			if pred.capture == "" {
				t.Errorf("predicate[%d] has empty capture for: %q", i, selector)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Race/concurrency tests — ASTWalker shares a *sql.DB across goroutines.
// The Query method builds SQL strings and executes them; concurrent queries
// on the same db could expose connection pool exhaustion or data races.
// ---------------------------------------------------------------------------

// TestASTWalker_ConcurrentQueries runs multiple Query calls in parallel on
// the same ASTWalker + DB to detect data races (run with -race flag).
// Uses a temp file DB because :memory: gives each pool connection its own
// isolated database — concurrent queries would fail with "no such table".
func TestASTWalker_ConcurrentQueries(t *testing.T) {
	db := seedTestASTFile(t)
	defer func() { _ = db.Close() }()

	w := NewASTWalker(db)
	root := ASTRoot{DB: db, SourceID: "main.go", ParentPrefix: ""}

	selectors := []string{
		`(function_declaration name: (identifier) @name) @scope`,
		`(type_declaration (type_spec name: (type_identifier) @name) @scope)`,
		`(function_declaration name: (identifier) @name) @scope`,
	}

	const goroutines = 20
	errs := make(chan error, goroutines*len(selectors))

	var wg sync.WaitGroup
	for range goroutines {
		for _, sel := range selectors {
			wg.Add(1)
			go func(s string) {
				defer wg.Done()
				matches, err := w.Query(root, s)
				if err != nil {
					errs <- fmt.Errorf("Query(%q): %w", s, err)
					return
				}
				// Read values to exercise data paths
				for _, m := range matches {
					_ = m.Values()
					_ = m.Context()
					if op, ok := m.(OriginProvider); ok {
						op.CaptureOrigin("scope")
						op.CaptureOrigin("name")
					}
				}
			}(sel)
		}
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent query error: %v", err)
	}
}

// TestASTWalker_RaceSelectWalker runs SelectWalker concurrently — it queries
// sqlite_master and creates walkers, so connection pool behavior matters.
func TestASTWalker_RaceSelectWalker(t *testing.T) {
	db := seedTestASTFile(t)
	defer func() { _ = db.Close() }()

	const goroutines = 10
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)

	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w, err := SelectWalker(db)
			if err != nil {
				errs <- err
				return
			}
			// Should always be ASTWalker for this DB
			if _, ok := w.(*ASTWalker); !ok {
				errs <- fmt.Errorf("expected ASTWalker, got %T", w)
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent SelectWalker error: %v", err)
	}
}
