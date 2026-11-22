package mcpserve

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/lltest"
	"github.com/agentic-research/mache/internal/mcpharness"
)

// positionalServer is a serve-mode MCP server whose registry was given args
// as its positional source, as `mache serve <args>` builds it.
func positionalServer(t *testing.T, args ...string) *graphRegistry {
	t.Helper()
	r := newGraphRegistry("", args)
	t.Cleanup(r.Close)
	return r
}

// #818: `mache serve --http <dir>` served a rootless client nothing — the
// positional directory was honoured only under --stdio. It is explicit
// operator input in every transport, so the HTTP daemon serves it too.
func TestRootlessHTTPPositional_RegistryRoutesToTheSource(t *testing.T) {
	require.False(t, serveStdio, "this test is about the HTTP transport")
	source := t.TempDir()
	r := positionalServer(t, source)

	g := r.resolveSession(context.Background(), &failingRootsSession{id: "rootless-http"})
	assert.Equal(t, source, g.basePath)
	assert.Nil(t, g.err, "a rootless session given a positional source must not get the error graph")
}

// End to end over Streamable HTTP, through mcp-go's own client: a client
// that declares no roots and sends no ?project= token is served the
// positional directory.
func TestRootlessHTTPPositional_ServesTheDirectoryOverHTTP(t *testing.T) {
	lltest.ResolveBinaryOrSkip(t)
	mcpharness.IsolatedHome(t)
	root := matrixTree(t)

	s := mcpharness.Served(t, newHTTPServer(newMCPServer(positionalServer(t, root))).Handler, "/mcp", mcpharness.Behavior{})
	assert.Equal(t, 4, overviewFiles(t, s), "total_files")
}

// A server without an explicit source still refuses rootless sessions.
func TestRootlessHTTPPositional_NoArgsStillRefused(t *testing.T) {
	r := positionalServer(t)
	g := r.resolveSession(context.Background(), &failingRootsSession{id: "no-source"})
	_, err := g.ListChildren("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "workspace root unavailable")
}

// jsonFixture writes a schema and a JSON data file side by side and returns
// their directory.
func jsonFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "schema.json"), []byte(`{
		"version": "v1",
		"nodes": [{
			"name": "items",
			"selector": "$",
			"children": [{
				"name": "{{.name}}",
				"selector": "$[*]",
				"files": [{"name": "value", "content_template": "{{.value}}"}]
			}]
		}]
	}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "data.json"),
		[]byte(`[{"name":"foo","value":"bar"},{"name":"baz","value":"qux"}]`), 0o644))
	return dir
}

// #820: `mache serve --stdio --schema schema.json data.json`, run from the
// directory holding both, resolved the schema to "data.json/schema.json".
// The working directory takes precedence over the source directory.
func TestServeSchemaRelative_ResolvesAgainstTheWorkingDirectory(t *testing.T) {
	dir := jsonFixture(t)
	t.Chdir(dir)
	previous := serveSchema
	serveSchema = "schema.json"
	defer func() { serveSchema = previous }()
	require.NoError(t, os.Mkdir("records", 0o755))
	data := filepath.Join("records", "data.json")
	require.NoError(t, os.Rename("data.json", data))
	require.NoError(t, os.WriteFile(filepath.Join("records", "schema.json"), []byte("{}"), 0o644))

	r := positionalServer(t, data)
	g := r.resolveSession(context.Background(), &failingRootsSession{id: "rootless-json"})
	children, err := g.ListChildren("items")
	require.NoError(t, err)
	assert.Len(t, children, 2)
}

// The base a relative schema joins onto is never the data file itself, even
// when the flag was not anchored (a schema that is not in the working
// directory): a file's parent is the nearest directory that can hold one.
func TestServeSchemaRelative_FileBaseUsesItsParent(t *testing.T) {
	dir := jsonFixture(t)
	t.Chdir(t.TempDir()) // schema.json is not here, so it is not anchored
	previous := serveSchema
	serveSchema = "schema.json"
	defer func() { serveSchema = previous }()

	data := filepath.Join(dir, "data.json")
	lg := &lazyGraph{args: []string{data}, basePath: data}
	_, schema, err := lg.resolveArgSource()
	require.NoError(t, err)
	require.NotNil(t, schema)
	assert.Equal(t, "items", schema.Nodes[0].Name)
}

// Preset names and absolute paths are never rewritten, and a preset name
// wins even over a same-named file in the working directory, as in Resolve.
func TestServeSchemaRelative_AnchorLeavesPresetsAndAbsolutePaths(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go"), []byte("{}"), 0o644))
	assert.Equal(t, "go", anchorSchemaFlag("go"))
	assert.Equal(t, "/abs/schema.json", anchorSchemaFlag("/abs/schema.json"))
	assert.Equal(t, "", anchorSchemaFlag(""))
	assert.Equal(t, ".mache/schema.json", anchorSchemaFlag(".mache/schema.json"))
}
