package ingest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSegmentEscape pins the two properties a node-ID segment needs: it holds
// no '/', and the encoding is injective — `a/b` and `a%2Fb` are different
// names and must stay different IDs (mache-94f571).
func TestSegmentEscape(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`"fmt"`, `"fmt"`},
		{"Store.Add", "Store.Add"},
		{`"golden/store"`, `"golden%2Fstore"`},
		{"encoding/json", "encoding%2Fjson"},
		{"a/b/c", "a%2Fb%2Fc"},
		{"100%", "100%25"},
		{"a%2Fb", "a%252Fb"},
	} {
		assert.Equal(t, tc.want, segmentEscape(tc.in), "segmentEscape(%q)", tc.in)
		assert.NotContains(t, segmentEscape(tc.in), "/", "segmentEscape(%q) left a separator", tc.in)
	}

	// Injective: the two names that collide under a '/'-only escape.
	assert.NotEqual(t, segmentEscape("a/b"), segmentEscape("a%2Fb"),
		"escaping '/' without escaping '%' collapses two distinct names onto one ID")
}

// TestJoinSegment_KeepsNameInOneSegment is the property the ID depends on:
// whatever the name renders, the joined path gains exactly one segment.
func TestJoinSegment_KeepsNameInOneSegment(t *testing.T) {
	assert.Equal(t, `main/imports/"golden%2Fstore"`, joinSegment("main/imports", `"golden/store"`))
	assert.Equal(t, "main/imports", filepath.Dir(joinSegment("main/imports", `"golden/store"`)),
		"the parent of the joined ID must be the path it was joined onto")
}

// TestEngine_SlashedNameIsOneNode projects a Go file importing a slashed path
// through an imports schema shaped like the go preset's. Before mache-94f571
// the import's ID was `main/imports/"golden/store"`, whose parent
// `main/imports/"golden` is not a node — SQLiteWriter.AddNode split the ID at
// its last '/', so ListChildren never returned it.
func TestEngine_SlashedNameIsOneNode(t *testing.T) {
	dir := t.TempDir()
	goFile := filepath.Join(dir, "main.go")
	src := "package main\n\nimport (\n\t\"fmt\"\n\t\"golden/store\"\n)\n\nfunc main() { fmt.Println(store.N) }\n"
	require.NoError(t, os.WriteFile(goFile, []byte(src), 0o644))

	schema := &api.Topology{Version: "1", Nodes: []api.Node{{
		Name:     "{{.pkg}}",
		Selector: "(source_file (package_clause (package_identifier) @pkg)) @scope",
		Children: []api.Node{{
			Name:     "imports",
			Selector: "$",
			Children: []api.Node{{
				Name:     "{{.path}}",
				Selector: "(import_spec path: (interpreted_string_literal) @path) @scope",
				Files:    []api.Leaf{{Name: "source", ContentTemplate: "{{.scope}}"}},
			}},
		}},
	}}}

	store := graph.NewMemoryStore()
	engine := NewEngine(schema, store)
	attachLeylineAST(t, engine, goFile)
	require.NoError(t, engine.Ingest(goFile))

	kids, err := store.ListChildren("main/imports")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{`main/imports/"fmt"`, `main/imports/"golden%2Fstore"`}, kids,
		"a slashed import path is one child of imports/, not a node nobody can reach")

	const id = `main/imports/"golden%2Fstore"`
	_, err = store.GetNode(id)
	require.NoError(t, err, "%s must exist", id)
	leaves, err := store.ListChildren(id)
	require.NoError(t, err)
	assert.Equal(t, []string{id + "/source"}, leaves)

	// The ID is escaped; the DEFINITION TOKEN is not. `search` and the smell
	// rules match the import as it is written in the source.
	assert.Equal(t, []string{id}, store.LookupDef(`"golden/store"`),
		"the def token keeps the source spelling")
	assert.Empty(t, store.LookupDef(`"golden%2Fstore"`), "the escaped form is an ID, not a token")
}
