package projtest_test

import (
	"testing"

	"github.com/agentic-research/mache/internal/projtest"
	"github.com/agentic-research/mache/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLeylineSource: the source projector parses a directory with the pinned
// leyline and projects it, so a function in the sample lands where the
// schema says. Skips when the pinned leyline is not cached (lltest).
func TestLeylineSource(t *testing.T) {
	topo, err := schema.ParseTopology([]byte(`{"version":"v1","nodes":[{"name":"{{.pkg}}","selector":"(source_file (package_clause (package_identifier) @pkg)) @scope","children":[{"name":"functions","selector":"$","children":[{"name":"{{.name}}","selector":"(function_declaration name: (identifier) @name) @scope","files":[{"name":"source","content_template":"{{.scope}}"}]}]}]}]}`))
	require.NoError(t, err)
	dir := projtest.WriteFiles(t, map[string]string{"a.go": "package demo\n\nfunc Hello() string { return \"hi\" }\n"})

	g := projtest.LeylineSource(t, topo, dir)

	dirs, leaves, problems := projtest.Walk(t, g, nil)
	assert.Empty(t, problems)
	assert.Equal(t, []string{"demo", "demo/functions", "demo/functions/Hello"}, dirs)
	assert.Equal(t, []string{"demo/functions/Hello/source"}, leaves)
	node, err := g.GetNode("demo/functions/Hello/source")
	require.NoError(t, err)
	assert.Equal(t, `func Hello() string { return "hi" }`, projtest.Read(t, g, node))
}

// TestWalk: the walker reports a name or content rendered from a missing
// field and an empty leaf, and lets a nullable leaf render empty.
func TestWalk(t *testing.T) {
	topo, err := schema.ParseTopology([]byte(`{"version":"v1","nodes":[{"name":"{{.missing}}","selector":"$","files":[{"name":"a","content_template":"{{.absent}}"},{"name":"b","content_template":" "},{"name":"c","content_template":" "}]}]}`))
	require.NoError(t, err)
	dir := projtest.WriteFiles(t, map[string]string{"d.json": `{"present": 1}`})

	_, leaves, problems := projtest.Walk(t, projtest.JSONDocument(t, topo, dir+"/d.json"), []string{"c"})

	assert.Len(t, leaves, 3)
	assert.ElementsMatch(t, []string{
		"<no value>: named from a missing field",
		"<no value>/a: content rendered from a missing field",
		"<no value>/b: renders empty",
	}, problems)
}

// TestVenturiResults: records land in the named table, keyed by identifier,
// and the direct backend reads them back through a schema.
func TestVenturiResults(t *testing.T) {
	records := []byte(`[{"schema":"s","identifier":"one","item":{"k":"v1"}},{"schema":"s","identifier":"two","item":{"k":"v2"}}]`)
	topo, err := schema.ParseTopology([]byte(`{"version":"v1","table":"rows","nodes":[{"name":"r","children":[{"name":"{{.identifier}}","files":[{"name":"k","content_template":"{{.item.k}}"}]}]}]}`))
	require.NoError(t, err)

	g := projtest.VenturiDirect(t, topo, projtest.VenturiResults(t, records, "rows"))

	_, leaves, problems := projtest.Walk(t, g, nil)
	assert.Empty(t, problems)
	assert.Equal(t, []string{"r/one/k", "r/two/k"}, leaves)
}

// TestVenturiEngine: the engine stream over a results db ignores the root
// selector and runs child selectors against `[record]`, so `$[*]` is the
// record.
func TestVenturiEngine(t *testing.T) {
	records := []byte(`[{"schema":"s","identifier":"one","item":{"k":"v1"}},{"schema":"s","identifier":"two","item":{"k":"v2"}}]`)
	topo, err := schema.ParseTopology([]byte(`{"version":"v1","nodes":[{"name":"r","selector":"$","children":[{"name":"{{.identifier}}","selector":"$[*]","files":[{"name":"k","content_template":"{{.item.k}}"}]}]}]}`))
	require.NoError(t, err)

	g := projtest.VenturiEngine(t, topo, projtest.VenturiResults(t, records, ""))

	_, leaves, problems := projtest.Walk(t, g, nil)
	assert.Empty(t, problems)
	assert.Equal(t, []string{"r/one/k", "r/two/k"}, leaves)
	node, err := g.GetNode("r/two/k")
	require.NoError(t, err)
	assert.Equal(t, "v2", projtest.Read(t, g, node))
}
