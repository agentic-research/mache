package ingest

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// docIndex is a file whose nodes, in start_byte order, are:
//
//	[0,10)  comment          "// A"      ─┐ contiguous (gap 1)
//	[11,21) comment          "// B"      ─┘
//	[22,60) function_declaration  Fn     ← scope; gap 1 to B
//	[70,80) comment          "// far"    (gap 10 to Gn: not a doc)
//	[90,120) function_declaration_1  Gn
//	[121,130) comment        "// inner"  child of Gn, not a sibling of anything scoped
//
// all under source_file — the shape SitterWalker's PrevSibling scan and the
// SQL that replaced it both read.
func docIndex() *fileIndex {
	idx := newFileIndex()
	for _, n := range []idxNode{
		{id: "sf", astKind: "source_file", startByte: 0, endByte: 130},
		{id: "sf/comment_0", parentID: "sf", astKind: "comment", startByte: 0, endByte: 10},
		{id: "sf/comment_1", parentID: "sf", astKind: "comment", startByte: 11, endByte: 21},
		{id: "sf/function_declaration_0", parentID: "sf", astKind: "function_declaration", startByte: 22, endByte: 60},
		{id: "sf/comment_2", parentID: "sf", astKind: "comment", startByte: 70, endByte: 80},
		{id: "sf/function_declaration_1", parentID: "sf", astKind: "function_declaration", startByte: 90, endByte: 120},
		{id: "sf/function_declaration_1/comment", parentID: "sf/function_declaration_1", astKind: "comment", startByte: 121, endByte: 130},
	} {
		idx.add(n)
	}
	return idx
}

// TestFileIndex_DocExtendStart pins the in-memory doc-comment scan: it walks
// back over CONTIGUOUS preceding comment siblings (gap <= 2 bytes) and stops
// at the first gap, exactly like the SQL loop it replaced (mache-40ce82).
func TestFileIndex_DocExtendStart(t *testing.T) {
	idx := docIndex()

	assert.Equal(t, uint32(0), idx.docExtendStart("sf/function_declaration_0", 22),
		"both comments are contiguous with Fn and each other: extend to the first")
	assert.Equal(t, uint32(90), idx.docExtendStart("sf/function_declaration_1", 90),
		"a comment 10 bytes above Gn is not its doc")
	assert.Equal(t, uint32(22), idx.docExtendStart("no-such-node", 22),
		"a scope the file does not hold (the \"$\" grouping match) extends nothing")

	// The nearest comment is contiguous but the one above it is not.
	idx2 := newFileIndex()
	for _, n := range []idxNode{
		{id: "sf", astKind: "source_file", startByte: 0, endByte: 100},
		{id: "sf/comment_0", parentID: "sf", astKind: "comment", startByte: 0, endByte: 10},
		{id: "sf/comment_1", parentID: "sf", astKind: "comment", startByte: 20, endByte: 30},
		{id: "sf/function_declaration", parentID: "sf", astKind: "function_declaration", startByte: 31, endByte: 100},
	} {
		idx2.add(n)
	}
	assert.Equal(t, uint32(20), idx2.docExtendStart("sf/function_declaration", 31),
		"the scan stops at the first gap wider than 2 bytes")
}

// TestFileIndex_CallRows_QualifierIsEverySibling pins the qualified-call
// shape callRows reproduces from the SQL it replaced: one row per sibling of
// the leaf, with the sibling's leaf text as qualifier — "" for a non-leaf
// sibling — and no row for a leaf with no sibling.
func TestFileIndex_CallRows_QualifierIsEverySibling(t *testing.T) {
	idx := newFileIndex()
	for _, n := range []idxNode{
		// fmt.Println(): identifier sibling → qualifier "fmt".
		{id: "c0", astKind: "call_expression", startByte: 0, endByte: 20},
		{id: "c0/selector_expression", parentID: "c0", astKind: "selector_expression", startByte: 0, endByte: 11},
		{id: "c0/selector_expression/identifier", parentID: "c0/selector_expression", astKind: "identifier", record: "fmt", startByte: 0, endByte: 3},
		{id: "c0/selector_expression/field_identifier", parentID: "c0/selector_expression", astKind: "field_identifier", record: "Println", startByte: 4, endByte: 11},
		// a.b.C(): the operand is a selector_expression (no text) → qualifier "".
		{id: "c1", astKind: "call_expression", startByte: 30, endByte: 50},
		{id: "c1/selector_expression", parentID: "c1", astKind: "selector_expression", startByte: 30, endByte: 41},
		{id: "c1/selector_expression/selector_expression", parentID: "c1/selector_expression", astKind: "selector_expression", startByte: 30, endByte: 33},
		{id: "c1/selector_expression/field_identifier", parentID: "c1/selector_expression", astKind: "field_identifier", record: "C", startByte: 34, endByte: 35},
		// A lone field_identifier with no sibling at all → no row when qualified.
		{id: "c2", astKind: "call_expression", startByte: 60, endByte: 70},
		{id: "c2/selector_expression", parentID: "c2", astKind: "selector_expression", startByte: 60, endByte: 65},
		{id: "c2/selector_expression/field_identifier", parentID: "c2/selector_expression", astKind: "field_identifier", record: "Lone", startByte: 60, endByte: 64},
	} {
		idx.add(n)
	}
	p := CallPattern{OuterKind: "call_expression", Ancestors: []string{"selector_expression"}, LeafKind: "field_identifier", QualifierKind: "identifier"}

	assert.Equal(t, []callRow{
		{token: "Println", qualifier: "fmt", leafID: "c0/selector_expression/field_identifier"},
		{token: "C", qualifier: "", leafID: "c1/selector_expression/field_identifier"},
	}, idx.callRows(p, true, ""))

	assert.Equal(t, []callRow{
		{token: "Println", leafID: "c0/selector_expression/field_identifier"},
		{token: "C", leafID: "c1/selector_expression/field_identifier"},
		{token: "Lone", leafID: "c2/selector_expression/field_identifier"},
	}, idx.callRows(p, false, ""), "unqualified: every matching leaf, sibling or not")

	assert.Equal(t, []callRow{
		{token: "C", leafID: "c1/selector_expression/field_identifier"},
	}, idx.callRows(p, false, "c1"), "scopePrefix restricts to that subtree")
}

// TestFileIndex_CallRows_RequirePriorSibling pins the value-position
// constraint: only a literal_element with an EARLIER literal_element sibling
// under the keyed_element (the value, not the key) yields its identifier.
func TestFileIndex_CallRows_RequirePriorSibling(t *testing.T) {
	idx := newFileIndex()
	for _, n := range []idxNode{
		{id: "k", astKind: "keyed_element", startByte: 0, endByte: 20},
		{id: "k/literal_element_0", parentID: "k", astKind: "literal_element", startByte: 0, endByte: 5},
		{id: "k/literal_element_0/identifier", parentID: "k/literal_element_0", astKind: "identifier", record: "Key", startByte: 0, endByte: 3},
		{id: "k/literal_element_1", parentID: "k", astKind: "literal_element", startByte: 7, endByte: 12},
		{id: "k/literal_element_1/identifier", parentID: "k/literal_element_1", astKind: "identifier", record: "Value", startByte: 7, endByte: 12},
	} {
		idx.add(n)
	}
	p := CallPattern{OuterKind: "keyed_element", Ancestors: []string{"literal_element"}, LeafKind: "identifier", RequirePriorSibling: true}
	assert.Equal(t, []callRow{{token: "Value", leafID: "k/literal_element_1/identifier"}}, idx.callRows(p, false, ""))

	p.RequirePriorSibling = false
	assert.Len(t, idx.callRows(p, false, ""), 2, "without the constraint the key matches too")
}
