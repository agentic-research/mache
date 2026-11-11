package ingest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// indexSource builds an index over src from nodes given by the text they span:
// each node is placed at the first occurrence of its text at or after the
// previous node's start, so a test reads as the source it describes.
func indexSource(t *testing.T, src string, nodes []idxNode, texts []string) *fileIndex {
	t.Helper()
	idx := newFileIndex()
	idx.source = []byte(src)
	from := 0
	for i, n := range nodes {
		at := strings.Index(src[from:], texts[i])
		require.GreaterOrEqual(t, at, 0, "%q not in the source after byte %d", texts[i], from)
		n.startByte, n.endByte = from+at, from+at+len(texts[i])
		from = n.startByte
		idx.add(n)
	}
	return idx
}

// TestFileIndex_DocExtendStart pins the doc-comment scan (mache-1e3d11): the
// run of comment siblings directly above a construct, with no blank line in
// the run or before the construct, of any comment kind, reaching over Rust
// attributes.
func TestFileIndex_DocExtendStart(t *testing.T) {
	const goSrc = "// A\n// B\nfunc Fn() {}\n\n// far\n\nfunc Gn() {\n\t// inner\n}\n"
	goIdx := indexSource(t, goSrc, []idxNode{
		{id: "sf", astKind: "source_file"},
		{id: "sf/comment_0", parentID: "sf", astKind: "comment"},
		{id: "sf/comment_1", parentID: "sf", astKind: "comment"},
		{id: "sf/function_declaration_0", parentID: "sf", astKind: "function_declaration"},
		{id: "sf/comment_2", parentID: "sf", astKind: "comment"},
		{id: "sf/function_declaration_1", parentID: "sf", astKind: "function_declaration"},
		{id: "sf/function_declaration_1/comment", parentID: "sf/function_declaration_1", astKind: "comment"},
	}, []string{goSrc, "// A", "// B", "func Fn() {}", "// far", "func Gn() {\n\t// inner\n}", "// inner"})
	fn, gn := uint32(strings.Index(goSrc, "func Fn")), uint32(strings.Index(goSrc, "func Gn"))
	assert.Equal(t, uint32(0), goIdx.docExtendStart("sf/function_declaration_0", fn),
		"both comments sit on the lines directly above Fn: extend to the first")
	assert.Equal(t, gn, goIdx.docExtendStart("sf/function_declaration_1", gn),
		"a comment with a blank line below it is not Gn's doc")
	assert.Equal(t, fn, goIdx.docExtendStart("no-such-node", fn),
		"a scope the file does not hold (the \"$\" grouping match) extends nothing")
	assert.NotContains(t, goIdx.byKind, "comment", "a comment is not a node of the index")
	assert.Len(t, goIdx.children["sf"], 2, "nor anyone's child: only the two functions are")

	// A blank line inside the run ends it at the comment below the blank.
	const runSrc = "// far\n\n// near\nfunc F() {}\n"
	runIdx := indexSource(t, runSrc, []idxNode{
		{id: "sf", astKind: "source_file"},
		{id: "sf/comment_0", parentID: "sf", astKind: "comment"},
		{id: "sf/comment_1", parentID: "sf", astKind: "comment"},
		{id: "sf/function_declaration", parentID: "sf", astKind: "function_declaration"},
	}, []string{runSrc, "// far", "// near", "func F() {}"})
	assert.Equal(t, uint32(strings.Index(runSrc, "// near")),
		runIdx.docExtendStart("sf/function_declaration", uint32(strings.Index(runSrc, "func F"))))

	// Rust: line_comment spans its newline, the doc sits above an attribute,
	// and a method's doc is indented in its impl.
	const rsSrc = "/// Doc.\n/// More.\n#[inline]\nfn f() {}\n\n#[test]\nfn g() {}\n\nimpl S {\n    /** Block. */\n    fn m() {}\n}\n"
	rsIdx := indexSource(t, rsSrc, []idxNode{
		{id: "sf", astKind: "source_file"},
		{id: "sf/line_comment_0", parentID: "sf", astKind: "line_comment"},
		{id: "sf/line_comment_1", parentID: "sf", astKind: "line_comment"},
		{id: "sf/attribute_item_0", parentID: "sf", astKind: "attribute_item"},
		{id: "sf/function_item_0", parentID: "sf", astKind: "function_item"},
		{id: "sf/attribute_item_1", parentID: "sf", astKind: "attribute_item"},
		{id: "sf/function_item_1", parentID: "sf", astKind: "function_item"},
		{id: "sf/impl_item", parentID: "sf", astKind: "impl_item"},
		{id: "sf/impl_item/declaration_list", parentID: "sf/impl_item", astKind: "declaration_list"},
		{id: "sf/impl_item/declaration_list/block_comment", parentID: "sf/impl_item/declaration_list", astKind: "block_comment"},
		{id: "sf/impl_item/declaration_list/function_item", parentID: "sf/impl_item/declaration_list", astKind: "function_item"},
	}, []string{rsSrc, "/// Doc.\n", "/// More.\n", "#[inline]", "fn f() {}", "#[test]", "fn g() {}", "impl S {", "{\n    /**", "/** Block. */", "fn m() {}"})
	assert.Equal(t, uint32(0), rsIdx.docExtendStart("sf/function_item_0", uint32(strings.Index(rsSrc, "fn f"))),
		"a line_comment run above an attribute is the item's doc")
	g := uint32(strings.Index(rsSrc, "fn g"))
	assert.Equal(t, g, rsIdx.docExtendStart("sf/function_item_1", g),
		"an attribute with no comment above it extends nothing")
	inner := indexSource(t, "//! Crate.\n/// Item.\nfn f() {}\n", []idxNode{
		{id: "sf", astKind: "source_file"},
		{id: "sf/line_comment_0", parentID: "sf", astKind: "line_comment"},
		{id: "sf/line_comment_1", parentID: "sf", astKind: "line_comment"},
		{id: "sf/function_item", parentID: "sf", astKind: "function_item"},
	}, []string{"//! Crate.\n/// Item.\nfn f() {}\n", "//! Crate.\n", "/// Item.\n", "fn f() {}"})
	assert.Equal(t, uint32(len("//! Crate.\n")), inner.docExtendStart("sf/function_item", uint32(len("//! Crate.\n/// Item.\n"))),
		"an inner doc comment documents what encloses it: it ends the run")
	assert.Equal(t, uint32(strings.Index(rsSrc, "/** Block")),
		rsIdx.docExtendStart("sf/impl_item/declaration_list/function_item", uint32(strings.Index(rsSrc, "fn m"))),
		"an indented block_comment directly above a method is its doc")

	goIdx.source = nil
	assert.Equal(t, fn, goIdx.docExtendStart("sf/function_declaration_0", fn),
		"without the file's source there is nothing to measure a gap in")
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
