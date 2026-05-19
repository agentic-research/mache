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

// extractDocComments walks backward from a tree-sitter @scope capture to find
// contiguous preceding comment nodes. Returns the doc comment text (just the
// comments) and the extended byte range for write-back origin tracking.
func extractDocComments(match Match) (docText string, startByte, endByte uint32, hasScope bool) {
	sm, ok := match.(interface{ GetCaptureNode(string) *sitter.Node })
	if !ok {
		return docText, startByte, endByte, hasScope
	}
	scopeNode := sm.GetCaptureNode("scope")
	if scopeNode == nil {
		return docText, startByte, endByte, hasScope
	}
	hasScope = true
	startByte = scopeNode.StartByte()
	endByte = scopeNode.EndByte()

	// Walk backward to find contiguous comment siblings
	n := scopeNode
	prev := n.PrevSibling()
	for prev != nil && prev.Type() == "comment" {
		// Check adjacency: <= 2 bytes gap (allow \n or \n\n)
		if int(n.StartByte())-int(prev.EndByte()) <= 2 {
			startByte = prev.StartByte()
			n = prev
			prev = prev.PrevSibling()
		} else {
			break
		}
	}

	// Extract doc comment text (just the comments, not the scope body)
	if startByte < scopeNode.StartByte() {
		if root, ok := match.Context().(SitterRoot); ok {
			if scopeNode.StartByte() <= uint32(len(root.Source)) {
				docText = strings.TrimRight(
					string(root.Source[startByte:scopeNode.StartByte()]),
					"\n\r\t ",
				)
			}
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
