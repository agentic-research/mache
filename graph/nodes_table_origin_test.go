package graph

import (
	"testing"

	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// originReader builds a ley-line-shaped fixture and wraps it in the reader
// under test.
//
// The `_ast` row is declared through fixturedb rather than hand-written DDL on
// purpose: `_ast` is ley-line-open's table, and internal/lint's LLO boundary
// rule fails any test that writes an LLO-owned table directly. It is right to
// do so — fixturedb's schema is DERIVED from the pinned producer's own
// sqlite_master output and re-verified against it, so a fixture cannot drift
// into a shape ley-line never emits. Hand-rolling the CREATE TABLE here was
// exactly the "one wrong spelling" that package exists to prevent.
//
// withAST=false models a standalone mache projection: no ley-line, no `_ast`,
// nothing locatable.
func originReader(t *testing.T, withAST bool) *NodesTableReader {
	t.Helper()
	b := fixturedb.New(t, fixturedb.Leyline).
		Construct("greeter.go/method_declaration")
	if withAST {
		// tree-sitter rows and columns are 0-based: row 6 is the SEVENTH line.
		b = b.ASTNode("greeter.go/method_declaration", "method_declaration", "greeter.go",
			fixturedb.Span{StartByte: 120, EndByte: 190, StartRow: 6, StartCol: 0, EndRow: 6, EndCol: 70})
	}
	_, f := b.Build()
	return NewNodesTableReader(f.DB(), "nodes", nil, nil, 0o644, 0o755, 16)
}

// TestOriginOf_ConvertsTreeSitterRowsToOneBased is the off-by-one guard. A
// "> 0" assertion would pass on the raw 0-based row, so this pins the exact
// value: row 6 is line 7, not line 6.
func TestOriginOf_ConvertsTreeSitterRowsToOneBased(t *testing.T) {
	origin := originReader(t, true).originOf("greeter.go/method_declaration")
	require.NotNil(t, origin, "a node with an _ast row must be locatable")

	assert.Equal(t, uint32(7), origin.StartLine, "tree-sitter row 6 is the 7th line, 1-based")
	assert.Equal(t, uint32(1), origin.StartCol, "column 0 is the 1st column, 1-based")
	assert.Equal(t, uint32(7), origin.EndLine)
	assert.Equal(t, uint32(71), origin.EndCol)

	// Byte offsets carry through UNCHANGED — write-back splices by byte, so
	// they must not get the +1 that the reader-facing line and column need.
	assert.Equal(t, uint32(120), origin.StartByte)
	assert.Equal(t, uint32(190), origin.EndByte)
	assert.NotEmpty(t, origin.FilePath, "a location without a file is not a location")
}

// TestOriginOf_NoASTTableYieldsNil covers the standalone (no ley-line-open)
// projection: nothing is locatable, and that must be a nil Origin rather than
// a zero-valued one. A zero-valued Origin reads as "line 0 of an empty file"
// to every consumer — worse than admitting we don't know.
func TestOriginOf_NoASTTableYieldsNil(t *testing.T) {
	assert.Nil(t, originReader(t, false).originOf("greeter.go/method_declaration"))
}

// TestOriginOf_NodeAbsentFromASTYieldsNil covers directories and virtual
// nodes: the table exists, the node simply has no row in it.
func TestOriginOf_NodeAbsentFromASTYieldsNil(t *testing.T) {
	assert.Nil(t, originReader(t, true).originOf("greeter.go"))
}

// TestGetNode_AttachesOrigin proves the lookup is wired into the read path —
// originOf being correct is worth nothing if GetNode never calls it.
func TestGetNode_AttachesOrigin(t *testing.T) {
	node, err := originReader(t, true).GetNode("greeter.go/method_declaration")
	require.NoError(t, err)
	require.NotNil(t, node.Origin, "GetNode must ATTACH the source location, not merely be able to compute it")
	assert.Equal(t, uint32(7), node.Origin.StartLine)
}
