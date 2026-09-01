package lint

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// publicAPIExampleDir holds the executable proof that mache's public API is
// sufficient on its own — see its package comment.
const publicAPIExampleDir = "examples/publicapi"

// TestPublicAPIExample_ImportsNothingInternal is what gives that proof its
// teeth.
//
// Every other test in this repo may import internal/, so none of them can
// answer "could an external consumer do this?" — they all have access no
// consumer has. The publicapi example answers it only for as long as it is
// actually restricted to the public surface, and nothing about being in
// examples/ enforces that: Go happily lets an in-module package import
// internal/. One convenient import would silently turn the proof back into a
// test that proves nothing, and it would still pass.
//
// So the restriction lives here, as a rule rather than a convention.
//
// If this fails, the fix is NOT to add the import. It is to promote whatever
// the example needed into the public API — that need IS the finding.
func TestPublicAPIExample_ImportsNothingInternal(t *testing.T) {
	root := repoRootForBoundary(t)
	dir := filepath.Join(root, publicAPIExampleDir)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "the public-API example must exist; it is the only consumer-shaped test in the repo")

	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		require.NoError(t, err, "parse %s", path)

		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			require.False(t, strings.Contains(p, "/internal/"),
				"%s imports %s — the public-API example must reach ONLY the public surface, "+
					"because its entire purpose is proving that surface is sufficient. "+
					"Promote what it needs into a public package instead of importing internal.",
				filepath.Join(publicAPIExampleDir, e.Name()), p)
		}
		checked++
	}
	require.Positive(t, checked, "no Go files found in %s — the proof cannot be vacuous", publicAPIExampleDir)
}
