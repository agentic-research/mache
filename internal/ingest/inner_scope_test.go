package ingest

import (
	"database/sql"
	"testing"

	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedGroupedTypeAST builds the ley-line parse of a single grouped Go
// declaration:
//
//	type ( Alpha int; Beta string )
//
// one type_declaration containing TWO type_spec nodes (leyline numbers them
// type_spec_0 and type_spec_1), each with a `name:` type_identifier and a
// `type:` type_identifier. This is the shape that exercises the inner-@scope
// path in ASTWalker.Query.
func seedGroupedTypeAST(t *testing.T) *sql.DB {
	t.Helper()
	const src = "package main\n\ntype (\n\tAlpha int\n\tBeta string\n)\n"
	b := fixturedb.New(t, fixturedb.Leyline)
	b.Source("main.go", "go", src)
	b.ASTNode("main.go", "source_file", "main.go", fixturedb.Bytes(0, len(src)))
	b.ASTNode("main.go/package_clause", "package_clause", "main.go", fixturedb.Bytes(0, 12))
	b.ASTNode("main.go/package_clause/package_identifier", "package_identifier", "main.go",
		fixturedb.Bytes(8, 12), fixturedb.Detail{Token: "main"})
	ty := "main.go/type_declaration"
	b.ASTNode(ty, "type_declaration", "main.go", fixturedb.Bytes(14, 46))
	b.ASTNode(ty+"/type_spec_0", "type_spec", "main.go", fixturedb.Bytes(22, 31))
	b.ASTNode(ty+"/type_spec_0/type_identifier_0", "type_identifier", "main.go", fixturedb.Bytes(22, 27),
		fixturedb.Detail{Token: "Alpha", Field: "name"})
	b.ASTNode(ty+"/type_spec_0/type_identifier_1", "type_identifier", "main.go", fixturedb.Bytes(28, 31),
		fixturedb.Detail{Token: "int", Field: "type"})
	b.ASTNode(ty+"/type_spec_1", "type_spec", "main.go", fixturedb.Bytes(33, 44))
	b.ASTNode(ty+"/type_spec_1/type_identifier_0", "type_identifier", "main.go", fixturedb.Bytes(33, 37),
		fixturedb.Detail{Token: "Beta", Field: "name"})
	b.ASTNode(ty+"/type_spec_1/type_identifier_1", "type_identifier", "main.go", fixturedb.Bytes(38, 44),
		fixturedb.Detail{Token: "string", Field: "type"})
	_, f := b.Build()
	return f.DB()
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
