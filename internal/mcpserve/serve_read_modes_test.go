package mcpserve

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/agentic-research/mache/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// modesFixture builds a construct directory shaped like a source schema:
// pkg/functions/<Name>/source, mirroring examples/go-schema.json.
func modesFixture(t *testing.T) graph.Graph {
	t.Helper()
	s := graph.NewMemoryStore()
	dir := func(id string, kids ...string) *graph.Node {
		return &graph.Node{ID: id, Mode: fs.ModeDir, Children: kids}
	}
	s.AddRoot(dir("pkg", "functions"))
	names := []string{"Alpha", "Beta", "Gamma"}
	s.AddNode(dir("pkg/functions", names...))
	bodies := map[string]string{
		"Alpha": "func Alpha(ctx context.Context) error {\n\tdoWork()\n\treturn nil\n}",
		"Beta":  "\n// leading blank line and a comment are skipped\nfunc Beta() {\n\tx := 1\n}",
		"Gamma": "func Gamma(a, b int) (int, error) { return a + b, nil }",
	}
	for _, name := range names {
		s.AddNode(dir("pkg/functions/"+name, "source"))
		s.AddNode(&graph.Node{ID: "pkg/functions/" + name + "/source", Data: []byte(bodies[name])})
	}
	return s
}

// signatures returns one declaration line per construct, sorted, in a single
// call — the property the whole mode exists for. Compare against reading the
// three bodies in full.
func TestReadProjected_SignaturesReturnsOneLinePerConstruct(t *testing.T) {
	g := modesFixture(t)
	out, ok, err := readProjected(g, "pkg/functions", readModeSignatures)
	require.NoError(t, err)
	require.True(t, ok)

	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 3, "one line per construct, no bodies")
	assert.Equal(t, "func Alpha(ctx context.Context) error {", lines[0])
	assert.Equal(t, "// leading blank line and a comment are skipped", lines[1],
		"first NON-BLANK line — a leading newline must not yield an empty signature")
	assert.Equal(t, "func Gamma(a, b int) (int, error) { return a + b, nil }", lines[2])
	assert.NotContains(t, out, "doWork()", "bodies must not appear")
}

// The measured claim: signatures is materially smaller than reading the same
// constructs in full. If this ever inverts, the mode is pointless.
func TestReadProjected_SignaturesIsSmallerThanFullBodies(t *testing.T) {
	g := modesFixture(t)
	sigs, _, err := readProjected(g, "pkg/functions", readModeSignatures)
	require.NoError(t, err)

	var full int
	for _, n := range []string{"Alpha", "Beta", "Gamma"} {
		node, err := g.GetNode("pkg/functions/" + n + "/source")
		require.NoError(t, err)
		full += int(node.ContentSize())
	}
	assert.Less(t, len(sigs), full, "signatures must be smaller than the bodies it summarises")
}

// map is names only — no content at all.
func TestReadProjected_MapIsNamesOnly(t *testing.T) {
	g := modesFixture(t)
	out, ok, err := readProjected(g, "pkg/functions", readModeMap)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "Alpha\nBeta\nGamma\n", out)
	assert.NotContains(t, out, "func ")
}

// Output must be deterministic so a caller can diff two reads of one node.
func TestReadProjected_DeterministicOrder(t *testing.T) {
	g := modesFixture(t)
	first, _, err := readProjected(g, "pkg/functions", readModeSignatures)
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		again, _, err := readProjected(g, "pkg/functions", readModeSignatures)
		require.NoError(t, err)
		assert.Equal(t, first, again, "map iteration must not leak into output order")
	}
}

// full (and empty) must fall through to the existing read path untouched —
// ok=false — so this feature cannot change default behaviour.
func TestReadProjected_FullFallsThrough(t *testing.T) {
	g := modesFixture(t)
	for _, mode := range []string{"", readModeFull} {
		_, ok, err := readProjected(g, "pkg/functions", mode)
		require.NoError(t, err)
		assert.False(t, ok, "mode %q must not intercept the normal read path", mode)
	}
}

// An unknown mode is an ERROR, never a silent fallback to full. Returning 9KB
// when the caller asked for 528 B is exactly the failure this mode prevents.
func TestReadProjected_UnknownModeErrorsRatherThanFallingBack(t *testing.T) {
	g := modesFixture(t)
	_, ok, err := readProjected(g, "pkg/functions", "signature") // near-miss typo
	require.Error(t, err)
	assert.False(t, ok)
	assert.Contains(t, err.Error(), "unknown mode")
	assert.Contains(t, err.Error(), "signatures", "the error must name the valid modes")
}

// A leaf node is well-defined under both modes rather than rejected.
func TestReadProjected_LeafNodeHandled(t *testing.T) {
	g := modesFixture(t)
	sig, ok, err := readProjected(g, "pkg/functions/Alpha/source", readModeSignatures)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "func Alpha(ctx context.Context) error {", sig)

	name, ok, err := readProjected(g, "pkg/functions/Alpha/source", readModeMap)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "source", name)
}

func TestDeclarationLine_SkipsBlankLines(t *testing.T) {
	assert.Equal(t, "func F()", declarationLine("\n\n   \nfunc F()\n\tbody"))
	assert.Empty(t, declarationLine("\n\n   \n"))
	assert.Empty(t, declarationLine(""))
}

func TestReadProjected_MissingNode(t *testing.T) {
	g := modesFixture(t)
	_, _, err := readProjected(g, "pkg/nope", readModeSignatures)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}
