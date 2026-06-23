package ingest

import (
	"strings"
	"sync"

	sitter "github.com/smacker/go-tree-sitter"
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

// --- Go package name extraction for qualified defs ---

var (
	goPackageQueryOnce sync.Once
	goPackageQueryObj  *sitter.Query
)

// extractGoPackageName uses tree-sitter to find the package name from a Go file root.
func extractGoPackageName(fileRoot *sitter.Node, source []byte, lang *sitter.Language) string {
	goPackageQueryOnce.Do(func() {
		goPackageQueryObj, _ = sitter.NewQuery([]byte(`(package_clause (package_identifier) @pkg)`), lang)
	})
	if goPackageQueryObj == nil { // coverage:ignore
		return "" // coverage:ignore
	} // coverage:ignore

	qc := sitter.NewQueryCursor()
	defer qc.Close()
	qc.Exec(goPackageQueryObj, fileRoot)

	m, ok := qc.NextMatch()
	if !ok || len(m.Captures) == 0 { // coverage:ignore
		return "" // coverage:ignore
	} // coverage:ignore

	c := m.Captures[0]
	start := c.Node.StartByte()
	end := c.Node.EndByte()
	if start < uint32(len(source)) && end <= uint32(len(source)) {
		return string(source[start:end])
	}
	return "" // coverage:ignore
}
