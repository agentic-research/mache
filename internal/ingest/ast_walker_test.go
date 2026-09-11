package ingest

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// testASTSource is the Go file the shared walker fixture describes. The
// fixture's nodes and byte spans are the real parse of these bytes.
const testASTSource = "package main\n\nfunc Validate(x int) error {\n\treturn nil\n}\n\n" +
	"func Helper() string {\n\treturn \"ok\"\n}\n\ntype Config struct {\n\tName string\n}\n"

// seedTestAST builds the ley-line parse of testASTSource as main.go — the
// nodes, _ast, node_child and _source rows `leyline parse` writes, with the
// ids it assigns (the root is the source id; repeated sibling kinds are
// numbered) and the tree-sitter fields each child sits under:
//
//	main.go                              (source_file)
//	  package_clause/
//	    package_identifier               "main"
//	  function_declaration_0/
//	    name: identifier                 "Validate"
//	    parameters: parameter_list/
//	      parameter_declaration/
//	        name: identifier             "x"
//	        type: type_identifier        "int"
//	    result: type_identifier          "error"
//	    body: block/
//	  function_declaration_1/
//	    name: identifier                 "Helper"
//	    parameters: parameter_list/
//	    result: type_identifier          "string"
//	    body: block/
//	  type_declaration/
//	    type_spec/
//	      name: type_identifier          "Config"
//	      type: struct_type/
//
// The database is file-backed with one connection, so the concurrency tests
// share it too.
func seedTestAST(t *testing.T) *sql.DB {
	t.Helper()
	b := fixturedb.New(t, fixturedb.Leyline)
	b.Source("main.go", "go", testASTSource)
	b.ASTNode("main.go", "source_file", "main.go", fixturedb.Bytes(0, len(testASTSource)))
	b.ASTNode("main.go/package_clause", "package_clause", "main.go", fixturedb.Bytes(0, 12))
	b.ASTNode("main.go/package_clause/package_identifier", "package_identifier", "main.go",
		fixturedb.Bytes(8, 12), fixturedb.Detail{Token: "main"})

	fn := "main.go/function_declaration_0"
	b.ASTNode(fn, "function_declaration", "main.go", fixturedb.Bytes(14, 56))
	b.ASTNode(fn+"/identifier", "identifier", "main.go", fixturedb.Bytes(19, 27),
		fixturedb.Detail{Token: "Validate", Field: "name"})
	b.ASTNode(fn+"/parameter_list", "parameter_list", "main.go", fixturedb.Bytes(27, 34),
		fixturedb.Detail{Field: "parameters"})
	b.ASTNode(fn+"/parameter_list/parameter_declaration", "parameter_declaration", "main.go",
		fixturedb.Bytes(28, 33))
	b.ASTNode(fn+"/parameter_list/parameter_declaration/identifier", "identifier", "main.go",
		fixturedb.Bytes(28, 29), fixturedb.Detail{Token: "x", Field: "name"})
	b.ASTNode(fn+"/parameter_list/parameter_declaration/type_identifier", "type_identifier", "main.go",
		fixturedb.Bytes(30, 33), fixturedb.Detail{Token: "int", Field: "type"})
	b.ASTNode(fn+"/type_identifier", "type_identifier", "main.go", fixturedb.Bytes(35, 40),
		fixturedb.Detail{Token: "error", Field: "result"})
	b.ASTNode(fn+"/block", "block", "main.go", fixturedb.Bytes(41, 56), fixturedb.Detail{Field: "body"})

	fn = "main.go/function_declaration_1"
	b.ASTNode(fn, "function_declaration", "main.go", fixturedb.Bytes(58, 95))
	b.ASTNode(fn+"/identifier", "identifier", "main.go", fixturedb.Bytes(63, 69),
		fixturedb.Detail{Token: "Helper", Field: "name"})
	b.ASTNode(fn+"/parameter_list", "parameter_list", "main.go", fixturedb.Bytes(69, 71),
		fixturedb.Detail{Field: "parameters"})
	b.ASTNode(fn+"/type_identifier", "type_identifier", "main.go", fixturedb.Bytes(72, 78),
		fixturedb.Detail{Token: "string", Field: "result"})
	b.ASTNode(fn+"/block", "block", "main.go", fixturedb.Bytes(79, 95), fixturedb.Detail{Field: "body"})

	ty := "main.go/type_declaration"
	b.ASTNode(ty, "type_declaration", "main.go", fixturedb.Bytes(97, 132))
	b.ASTNode(ty+"/type_spec", "type_spec", "main.go", fixturedb.Bytes(102, 132))
	b.ASTNode(ty+"/type_spec/type_identifier", "type_identifier", "main.go", fixturedb.Bytes(102, 108),
		fixturedb.Detail{Token: "Config", Field: "name"})
	b.ASTNode(ty+"/type_spec/struct_type", "struct_type", "main.go", fixturedb.Bytes(109, 132),
		fixturedb.Detail{Field: "type"})
	_, f := b.Build()
	return f.DB()
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

// hclBlocksSource is the HCL file the predicate tests query: two top-level
// blocks whose type identifiers ("resource", "variable") the predicates
// select between.
const hclBlocksSource = "resource \"aws_instance\" {\n  ami = \"abc\"\n}\n\n" +
	"variable \"region\" {\n  default = \"us\"\n}\n"

// seedHCLBlocksAST builds the ley-line parse of hclBlocksSource as main.tf:
//
//	main.tf                      (config_file)
//	  body/
//	    block_0/
//	      identifier             "resource"
//	      string_lit             "\"aws_instance\""
//	      body/
//	    block_1/
//	      identifier             "variable"
//	      string_lit             "\"region\""
//	      body/
//
// HCL's grammar names no fields, so no child carries one — a block's type,
// labels and body are distinguished by kind and position alone.
func seedHCLBlocksAST(t *testing.T) *sql.DB {
	t.Helper()
	b := fixturedb.New(t, fixturedb.Leyline)
	b.Source("main.tf", "hcl", hclBlocksSource)
	b.ASTNode("main.tf", "config_file", "main.tf", fixturedb.Bytes(0, len(hclBlocksSource)))
	b.ASTNode("main.tf/body", "body", "main.tf", fixturedb.Bytes(0, 81))
	blk := "main.tf/body/block_0"
	b.ASTNode(blk, "block", "main.tf", fixturedb.Bytes(0, 41))
	b.ASTNode(blk+"/identifier", "identifier", "main.tf", fixturedb.Bytes(0, 8), fixturedb.Detail{Token: "resource"})
	b.ASTNode(blk+"/string_lit", "string_lit", "main.tf", fixturedb.Bytes(9, 23), fixturedb.Detail{Token: `"aws_instance"`})
	b.ASTNode(blk+"/body", "body", "main.tf", fixturedb.Bytes(28, 39))
	blk = "main.tf/body/block_1"
	b.ASTNode(blk, "block", "main.tf", fixturedb.Bytes(43, 81))
	b.ASTNode(blk+"/identifier", "identifier", "main.tf", fixturedb.Bytes(43, 51), fixturedb.Detail{Token: "variable"})
	b.ASTNode(blk+"/string_lit", "string_lit", "main.tf", fixturedb.Bytes(52, 60), fixturedb.Detail{Token: `"region"`})
	b.ASTNode(blk+"/body", "body", "main.tf", fixturedb.Bytes(65, 79))
	_, f := b.Build()
	return f.DB()
}

func TestASTWalker_PredicateEqFilter(t *testing.T) {
	db := seedHCLBlocksAST(t)
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

// TestASTWalker_MatchPredicate verifies that #match? regex predicates
// filter captures using the capture's resolved text. Implements bead
// mache-37646f — replaces the previous "rejects #match?" behavior.
func TestASTWalker_MatchPredicate(t *testing.T) {
	db := seedHCLBlocksAST(t)
	w := NewASTWalker(db)
	root := ASTRoot{DB: db, SourceID: "main.tf", ParentPrefix: ""}

	t.Run("match keeps only matching captures", func(t *testing.T) {
		// regex matches "resource" but not "variable"
		matches, err := w.Query(root, `(block (identifier) @_type (string_lit) @name (body) @scope (#match? @_type "^reso"))`)
		require.NoError(t, err)
		require.Len(t, matches, 1)
		assert.Equal(t, "resource", matches[0].Values()["_type"])
	})

	t.Run("match excludes when no capture matches", func(t *testing.T) {
		matches, err := w.Query(root, `(block (identifier) @_type (string_lit) @name (body) @scope (#match? @_type "^nope$"))`)
		require.NoError(t, err)
		assert.Empty(t, matches)
	})

	t.Run("not-match keeps captures that fail the regex", func(t *testing.T) {
		matches, err := w.Query(root, `(block (identifier) @_type (string_lit) @name (body) @scope (#not-match? @_type "^reso"))`)
		require.NoError(t, err)
		require.Len(t, matches, 1)
		assert.Equal(t, "variable", matches[0].Values()["_type"])
	})

	t.Run("invalid regex returns parse error", func(t *testing.T) {
		_, err := w.Query(root, `(block (identifier) @_type (body) @scope (#match? @_type "[unterminated"))`)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "#match?")
	})
}

func TestSelectWalker_ReturnsASTWalkerWhenASTTableExists(t *testing.T) {
	db := seedTestAST(t)
	defer func() { _ = db.Close() }()

	w, err := SelectWalker(db)
	require.NoError(t, err)
	_, ok := w.(*ASTWalker)
	assert.True(t, ok, "should return ASTWalker when _ast table exists")
}

func TestSelectWalker_ErrorsWhenNoASTTable(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// Just nodes table, no _ast
	_, err = db.Exec(`CREATE TABLE nodes (id TEXT PRIMARY KEY, name TEXT)`)
	require.NoError(t, err)

	// ADR-0012 step 4 removed in-process CGO tree-sitter, so a db with no
	// `_ast` table is an error — there is no SitterWalker fallback.
	w, err := SelectWalker(db)
	require.Error(t, err, "should error when _ast table missing (no CGO fallback)")
	assert.Nil(t, w)
	assert.Contains(t, err.Error(), "_ast")
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

// TestParseSelector_RepeatedOuterKind verifies ancestry when the outerKind
// (e.g., "call") reappears as a nested node. The Elixir def selector has this:
//
//	(call target: (identifier) @_fn (arguments (call target: (identifier) @name)) ...)
//
// The @name capture's ancestry must be [arguments, call] — not [arguments].
// Regression: the filter was removing ALL occurrences of outerKind from the
// ancestor chain instead of just the first (the scope).
func TestParseSelector_RepeatedOuterKind(t *testing.T) {
	selector := `(call target: (identifier) @_fn (arguments (call target: (identifier) @name)) (#eq? @_fn "def")) @scope`
	p, err := parseSelector(selector)
	require.NoError(t, err)
	assert.Equal(t, "call", p.outerKind)

	// Find the @name capture
	var nameCap *selectorCapture
	for i := range p.captures {
		if p.captures[i].name == "name" {
			nameCap = &p.captures[i]
			break
		}
	}
	require.NotNil(t, nameCap, "@name capture must exist")
	assert.Equal(t, "identifier", nameCap.kind)
	assert.Equal(t, []pathStep{{kind: "arguments"}, {kind: "call"}}, nameCap.ancestry,
		"ancestry must include inner 'call' — not strip it because outerKind is also 'call'")
	assert.Equal(t, "target", nameCap.field, "the capture keeps the field label it was written under")
}

// TestParseSelector_DeepAncestry verifies that the Go pointer-receiver and
// value-receiver selectors produce distinct ancestry chains, each step
// carrying the field label the selector wrote it under. This is what lets
// descendantsByKind distinguish (*Greeter).Greet from (Greeter).String — and
// the receiver's type from a parameter's (mache-91d903).
func TestParseSelector_DeepAncestry(t *testing.T) {
	// Pointer receiver: 3-level ancestry
	ptrSel := `(method_declaration receiver: (parameter_list (parameter_declaration type: (pointer_type (type_identifier) @receiver))) name: (field_identifier) @name) @scope`
	ptr, err := parseSelector(ptrSel)
	require.NoError(t, err)
	assert.Equal(t, "method_declaration", ptr.outerKind)

	var ptrReceiver, ptrName *selectorCapture
	for i := range ptr.captures {
		switch ptr.captures[i].name {
		case "receiver":
			ptrReceiver = &ptr.captures[i]
		case "name":
			ptrName = &ptr.captures[i]
		}
	}
	require.NotNil(t, ptrReceiver)
	require.NotNil(t, ptrName)
	assert.Equal(t, "type_identifier", ptrReceiver.kind)
	assert.Equal(t, []pathStep{
		{kind: "parameter_list", field: "receiver"},
		{kind: "parameter_declaration"},
		{kind: "pointer_type", field: "type"},
	}, ptrReceiver.ancestry)
	assert.Equal(t, "", ptrReceiver.field, "the type_identifier under pointer_type carries no field")
	assert.Equal(t, "field_identifier", ptrName.kind)
	assert.Equal(t, "name", ptrName.field)
	assert.Empty(t, ptrName.ancestry, "name is a direct child of method_declaration")

	// Value receiver: 2-level ancestry (no pointer_type)
	valSel := `(method_declaration receiver: (parameter_list (parameter_declaration type: (type_identifier) @receiver)) name: (field_identifier) @name) @scope`
	val, err := parseSelector(valSel)
	require.NoError(t, err)

	var valReceiver *selectorCapture
	for i := range val.captures {
		if val.captures[i].name == "receiver" {
			valReceiver = &val.captures[i]
		}
	}
	require.NotNil(t, valReceiver)
	assert.Equal(t, []pathStep{
		{kind: "parameter_list", field: "receiver"},
		{kind: "parameter_declaration"},
	}, valReceiver.ancestry, "value receiver has shorter ancestry than pointer receiver")
	assert.Equal(t, "type", valReceiver.field, "the value receiver's type_identifier is the declaration's type field")
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

// TestParseSelector_MatchPredicates verifies that #match? and #not-match?
// predicates are extracted from selectors with their regex compiled.
func TestParseSelector_MatchPredicates(t *testing.T) {
	selector := `(block (identifier) @name (body) @scope (#match? @name "^test_") (#not-match? @name "_skip$"))`
	p, err := parseSelector(selector)
	require.NoError(t, err)
	require.Len(t, p.matchPreds, 1)
	require.Len(t, p.notMatchPreds, 1)
	assert.Equal(t, "name", p.matchPreds[0].capture)
	assert.Equal(t, "^test_", p.matchPreds[0].pattern)
	assert.True(t, p.matchPreds[0].regex.MatchString("test_foo"))
	assert.False(t, p.matchPreds[0].regex.MatchString("foo"))
	assert.Equal(t, "name", p.notMatchPreds[0].capture)
	assert.Equal(t, "_skip$", p.notMatchPreds[0].pattern)
}

// TestASTWalker_NotEqPredicateSilentlyIgnored verifies that unsupported
// predicates (#not-eq?, #any-eq?, #is?, #is-not?) are rejected with an
// error rather than silently ignored.
func TestASTWalker_NotEqPredicateSilentlyIgnored(t *testing.T) {
	db := seedHCLBlocksAST(t)
	w := NewASTWalker(db)
	root := ASTRoot{DB: db, SourceID: "main.tf", ParentPrefix: ""}

	// #not-eq? should be rejected — ASTWalker only supports #eq?.
	_, err := w.Query(root, `(block (identifier) @_type) @scope (#not-eq? @_type "variable")`)
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

// TestASTWalker_MultipleChildrenSameKind pins what an UNLABELLED capture
// selects when a parent has several children of its kind: the first in
// document order. HCL's block labels carry no tree-sitter field (the grammar
// names none), so `(string_lit) @name` cannot say which label it means, and
// the second label is reachable only by position — which the selector
// language has no syntax for. A labelled capture (`name: (identifier)`) is
// constrained by field instead; see TestProjectSourceFile_FieldLabelsConstrainCaptures.
func TestASTWalker_MultipleChildrenSameKind(t *testing.T) {
	// resource "aws_instance" "my_server" {} — two string_lit labels under one block.
	const src = "resource \"aws_instance\" \"my_server\" {}\n"
	b := fixturedb.New(t, fixturedb.Leyline)
	b.Source("main.tf", "hcl", src)
	b.ASTNode("main.tf", "config_file", "main.tf", fixturedb.Bytes(0, len(src)))
	b.ASTNode("main.tf/body", "body", "main.tf", fixturedb.Bytes(0, 38))
	blk := "main.tf/body/block"
	b.ASTNode(blk, "block", "main.tf", fixturedb.Bytes(0, 38))
	b.ASTNode(blk+"/identifier", "identifier", "main.tf", fixturedb.Bytes(0, 8), fixturedb.Detail{Token: "resource"})
	b.ASTNode(blk+"/string_lit_0", "string_lit", "main.tf", fixturedb.Bytes(9, 23), fixturedb.Detail{Token: `"aws_instance"`})
	b.ASTNode(blk+"/string_lit_1", "string_lit", "main.tf", fixturedb.Bytes(24, 35), fixturedb.Detail{Token: `"my_server"`})
	_, f := b.Build()
	db := f.DB()

	w := NewASTWalker(db)
	root := ASTRoot{DB: db, SourceID: "main.tf", ParentPrefix: ""}

	matches, err := w.Query(root, `(block (identifier) @_type (string_lit) @name) @scope (#eq? @_type "resource")`)
	require.NoError(t, err)
	require.Len(t, matches, 1)

	v := matches[0].Values()
	name, _ := v["name"].(string)
	assert.Equal(t, "\"aws_instance\"", name, "an unlabelled capture takes the first child of its kind in document order")
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
	db := seedTestAST(t)
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
	db := seedTestAST(t)
	defer func() { _ = db.Close() }()

	const goroutines = 10
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)

	for range goroutines {
		wg.Go(func() {
			w, err := SelectWalker(db)
			if err != nil {
				errs <- err
				return
			}
			// Should always be ASTWalker for this DB
			if _, ok := w.(*ASTWalker); !ok {
				errs <- fmt.Errorf("expected ASTWalker, got %T", w)
			}
		})
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent SelectWalker error: %v", err)
	}
}
