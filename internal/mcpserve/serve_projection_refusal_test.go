package mcpserve

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/agentic-research/mache/internal/mcpharness"
)

// A served .db that declares a projection mache has not been checked against
// is refused at open (graph.CheckProjection), and the refusal reaches the
// client as a tool error that names the shape and the way out. The server
// itself keeps answering: it neither crashes nor serves a partial graph, and
// a db of a known shape beside it is served as before (mache-86e215).
func TestServe_RefusesADBOfUnknownProjection(t *testing.T) {
	mcpharness.IsolatedHome(t)
	serve := func(t *testing.T, version string) *mcpharness.Session {
		t.Helper()
		path, f := fixturedb.New(t, fixturedb.Leyline).
			Def("F", "a.go/function_declaration_0", "function").
			Build()
		fixturedb.DeclareProjection(t, f.DB(), version)
		r := newGraphRegistry("", []string{path})
		t.Cleanup(r.Close)
		return mcpharness.InProcess(t, newMCPServer(r), mcpharness.Behavior{Roots: []string{t.TempDir()}})
	}

	s := serve(t, "projection-v14")
	assert.NotEmpty(t, s.Tools(), "the server is up and lists its tools")
	for _, tool := range []string{"get_overview", "list_directory", "search"} {
		text, isErr := s.Call(tool, map[string]any{"path": "", "pattern": "%F%"})
		require.True(t, isErr, "%s must report the refusal as a tool error, got: %s", tool, text)
		assert.Contains(t, text, `"projection-v14"`, "%s names the shape found", tool)
		assert.Contains(t, text, "projection-v13", "%s names what this mache reads", tool)
		assert.Contains(t, text, "mache leyline path", "%s says how to rebuild", tool)
	}

	known := serve(t, "projection-v13")
	assert.Contains(t, known.Text("find_definition", map[string]any{"symbol": "F"}), "function_declaration_0",
		"the pinned shape is served as before")
}
