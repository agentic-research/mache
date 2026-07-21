package ingest

import (
	"strings"
)

// byteOffsetToLine converts a byte offset to a 1-based line number in content.
func byteOffsetToLine(content []byte, offset uint32) int {
	line := 1
	for i := 0; i < int(offset) && i < len(content); i++ {
		if content[i] == '\n' {
			line++
		}
	}
	return line
}

// extractDocComments returns the doc-comment text immediately preceding a
// match's @scope node, plus the doc-extended byte range (for the `location`
// property and write-back origin tracking). It reads through the DocScope
// interface so it is walker-agnostic — sitterMatch walks tree-sitter siblings,
// astMatch queries the _ast comment rows; both yield identical output.
func extractDocComments(match Match) (docText string, startByte, endByte uint32, hasScope bool) {
	ds, ok := match.(DocScope)
	if !ok {
		return docText, startByte, endByte, hasScope
	}
	docStart, scopeStart, scopeEnd, ok := ds.DocRange()
	if !ok {
		return docText, startByte, endByte, hasScope
	}
	hasScope = true
	startByte = docStart
	endByte = scopeEnd

	// Extract doc comment text (just the comments, not the scope body).
	if docStart < scopeStart {
		src := ds.ScopeSource()
		if scopeStart <= uint32(len(src)) {
			docText = strings.TrimRight(string(src[docStart:scopeStart]), "\n\r\t ")
		}
	}
	return docText, startByte, endByte, hasScope
}
