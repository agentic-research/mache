package fixturedb

import (
	"fmt"
	"testing"
)

// WideDeepID is a construct every Wide corpus holds, whatever its width, at
// the same depth: the block inside the second function of the first file.
const WideDeepID = "pkg/f0000.go/function_declaration_1/block"

// Wide builds a corpus of files source files under pkg/, each with three
// functions (so the kind repeats and is suffixed) and a block inside the
// second. Every file has the same shape, so two Wide corpora differ only in
// size: a lookup of WideDeepID is the same lookup in both, which is what a
// gate on how cost GROWS needs.
func Wide(t testing.TB, p Producer, files int) *FixtureDB {
	b := New(t, p)
	for i := range files {
		src := SourceID(fmt.Sprintf("pkg/f%04d.go", i))
		b.Source(src, "go", "")
		for j := range 3 {
			b.ASTNode(fmt.Sprintf("%s/function_declaration_%d", src, j), "function_declaration", src,
				Bytes(10*j, 10*j+9))
		}
		b.ASTNode(string(src)+"/function_declaration_1/block", "block", src, Bytes(12, 18))
	}
	_, f := b.Build()
	return f
}
