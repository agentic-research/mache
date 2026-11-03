package smells

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/agentic-research/mache/internal/fixturedb"
)

// TestPopulateSnippets_ReadsTheParsedBytes is mache-46c59d. ley-line leaves
// _source.content NULL and keeps the bytes in source_blobs, so reading the
// content column alone left every snippet empty on a real leyline db, while
// the hand-built fixture (inline content) kept the test green.
func TestPopulateSnippets_ReadsTheParsedBytes(t *testing.T) {
	const src = "package main\n\nfunc f(x int) bool { return x > 42 }\n"
	for _, p := range []fixturedb.Producer{fixturedb.Leyline, fixturedb.LeylineV6, fixturedb.Standalone} {
		t.Run(p.String(), func(t *testing.T) {
			b := fixturedb.New(t, p)
			b.Source("main.go", "go", src)
			_, f := b.Build()
			findings := []smellFinding{{SourceID: "main.go", StartByte: 46, EndByte: 48}}

			populateSnippets(f, findings)

			assert.Contains(t, findings[0].Snippet, "x > 42")
		})
	}
}
