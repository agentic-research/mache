package ingest

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedCallExtractionAST builds the ley-line parse of a Go file with one
// qualified and one bare call — the nodes, _ast and node_child rows
// `leyline parse` writes, under the ids it assigns:
//
//	package main
//	func A() {
//	    fmt.Println("x")  // qualified: pkg=fmt, call=Println
//	    Helper()          // bare: call=Helper
//	}
func seedCallExtractionAST(t *testing.T) *sql.DB {
	t.Helper()
	return seedCallsAST(t, 1)
}

// seedCallsAST is seedCallExtractionAST with the bare Helper() call repeated
// helperCalls times, for the dedupe test.
func seedCallsAST(t *testing.T, helperCalls int) *sql.DB {
	t.Helper()
	src := "package main\n\nfunc A() {\n\tfmt.Println(\"x\")\n" + strings.Repeat("\tHelper()\n", helperCalls) + "}\n"
	b := fixturedb.New(t, fixturedb.Leyline)
	b.Source("main.go", "go", src)
	b.ASTNode("main.go", "source_file", "main.go", fixturedb.Bytes(0, len(src)))
	b.ASTNode("main.go/package_clause", "package_clause", "main.go", fixturedb.Bytes(0, 12))
	b.ASTNode("main.go/package_clause/package_identifier", "package_identifier", "main.go",
		fixturedb.Bytes(8, 12), fixturedb.Detail{Token: "main"})
	fn := "main.go/function_declaration"
	b.ASTNode(fn, "function_declaration", "main.go", fixturedb.Bytes(14, len(src)-1))
	b.ASTNode(fn+"/identifier", "identifier", "main.go", fixturedb.Bytes(19, 20),
		fixturedb.Detail{Token: "A", Field: "name"})
	b.ASTNode(fn+"/parameter_list", "parameter_list", "main.go", fixturedb.Bytes(20, 22),
		fixturedb.Detail{Field: "parameters"})
	b.ASTNode(fn+"/block", "block", "main.go", fixturedb.Bytes(23, len(src)-1), fixturedb.Detail{Field: "body"})

	// fmt.Println("x")
	st := fn + "/block/expression_statement_0"
	b.ASTNode(st, "expression_statement", "main.go", fixturedb.Bytes(26, 42))
	b.ASTNode(st+"/call_expression", "call_expression", "main.go", fixturedb.Bytes(26, 42))
	b.ASTNode(st+"/call_expression/selector_expression", "selector_expression", "main.go",
		fixturedb.Bytes(26, 37), fixturedb.Detail{Field: "function"})
	b.ASTNode(st+"/call_expression/selector_expression/identifier", "identifier", "main.go",
		fixturedb.Bytes(26, 29), fixturedb.Detail{Token: "fmt", Field: "operand"})
	b.ASTNode(st+"/call_expression/selector_expression/field_identifier", "field_identifier", "main.go",
		fixturedb.Bytes(30, 37), fixturedb.Detail{Token: "Println", Field: "field"})
	b.ASTNode(st+"/call_expression/argument_list", "argument_list", "main.go",
		fixturedb.Bytes(37, 42), fixturedb.Detail{Field: "arguments"})
	b.ASTNode(st+"/call_expression/argument_list/interpreted_string_literal", "interpreted_string_literal",
		"main.go", fixturedb.Bytes(38, 41), fixturedb.Detail{Token: `"x"`})

	// Helper(), helperCalls times: identical statements share one subtree
	// hash, as ley-line's merkle ids give them.
	for i := range helperCalls {
		start := 44 + 10*i
		st := fmt.Sprintf("%s/block/expression_statement_%d", fn, i+1)
		b.ASTNode(st, "expression_statement", "main.go", fixturedb.Bytes(start, start+8),
			fixturedb.Detail{Subtree: "helper-stmt"})
		b.ASTNode(st+"/call_expression", "call_expression", "main.go", fixturedb.Bytes(start, start+8),
			fixturedb.Detail{Subtree: "helper-call"})
		b.ASTNode(st+"/call_expression/identifier", "identifier", "main.go", fixturedb.Bytes(start, start+6),
			fixturedb.Detail{Token: "Helper", Field: "function", Subtree: "helper-ident"})
		b.ASTNode(st+"/call_expression/argument_list", "argument_list", "main.go", fixturedb.Bytes(start+6, start+8),
			fixturedb.Detail{Field: "arguments", Subtree: "helper-args"})
	}
	_, f := b.Build()
	return f.DB()
}

func TestASTWalker_ExtractCalls_Go(t *testing.T) {
	db := seedCallExtractionAST(t)
	defer func() { _ = db.Close() }()

	w := NewASTWalker(db)
	calls, err := w.ExtractCalls("main.go", "go")
	require.NoError(t, err)

	sort.Strings(calls)
	assert.Equal(t, []string{"Helper", "Println"}, calls,
		"both bare and qualified calls should be returned by ExtractCalls")
}

func TestASTWalker_ExtractQualifiedCalls_Go(t *testing.T) {
	db := seedCallExtractionAST(t)
	defer func() { _ = db.Close() }()

	w := NewASTWalker(db)
	calls, err := w.ExtractQualifiedCalls("main.go", "go")
	require.NoError(t, err)

	sort.Slice(calls, func(i, j int) bool {
		if calls[i].Token != calls[j].Token {
			return calls[i].Token < calls[j].Token
		}
		return calls[i].Qualifier < calls[j].Qualifier
	})

	require.Len(t, calls, 2)
	assert.Equal(t, graph.QualifiedCall{Token: "Helper", Qualifier: ""}, calls[0],
		"bare call has no qualifier")
	assert.Equal(t, graph.QualifiedCall{Token: "Println", Qualifier: "fmt"}, calls[1],
		"qualified call captures pkg=fmt")
}

func TestASTWalker_ExtractCalls_UnknownLanguageReturnsNil(t *testing.T) {
	db := seedCallExtractionAST(t)
	defer func() { _ = db.Close() }()

	w := NewASTWalker(db)
	calls, err := w.ExtractCalls("main.go", "unknown-lang")
	require.NoError(t, err)
	assert.Nil(t, calls, "unregistered language returns nil, not error")
}

// TestASTWalker_ExtractContext verifies that import/const/var/type
// declarations are concatenated into a context blob from _source byte
// ranges. Bead mache-37926d.
func TestASTWalker_ExtractContext(t *testing.T) {
	const src = "package main\n\nimport \"fmt\"\n\nconst Pi = 3.14\n\ntype Greeter struct{}\n"
	b := fixturedb.New(t, fixturedb.Leyline)
	b.Source("main.go", "go", src)
	b.ASTNode("main.go", "source_file", "main.go", fixturedb.Bytes(0, len(src)))
	b.ASTNode("main.go/import_declaration", "import_declaration", "main.go", fixturedb.Bytes(14, 26))
	b.ASTNode("main.go/const_declaration", "const_declaration", "main.go", fixturedb.Bytes(28, 43))
	b.ASTNode("main.go/type_declaration", "type_declaration", "main.go", fixturedb.Bytes(45, 66))
	_, f := b.Build()

	w := NewASTWalker(f.DB())
	got, err := w.ExtractContext("main.go", "go")
	require.NoError(t, err)
	require.NotEmpty(t, got)

	gotStr := string(got)
	assert.Contains(t, gotStr, "import \"fmt\"")
	assert.Contains(t, gotStr, "const Pi = 3.14")
	assert.Contains(t, gotStr, "type Greeter struct{}")
}

func TestASTWalker_ExtractContext_UnknownLanguageReturnsNil(t *testing.T) {
	db := seedCallExtractionAST(t)
	defer func() { _ = db.Close() }()

	w := NewASTWalker(db)
	got, err := w.ExtractContext("main.go", "no-such-lang")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestASTWalker_ExtractCalls_Dedupes(t *testing.T) {
	// Two bare calls to the same name "Helper".
	db := seedCallsAST(t, 2)
	defer func() { _ = db.Close() }()

	w := NewASTWalker(db)
	calls, err := w.ExtractCalls("main.go", "go")
	require.NoError(t, err)

	helperCount := 0
	for _, c := range calls {
		if c == "Helper" {
			helperCount++
		}
	}
	assert.Equal(t, 1, helperCount, "duplicate Helper calls must be deduplicated")
}
