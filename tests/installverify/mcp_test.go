package installverify

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Gate (c): the installed binary's commands RUN AND RETURN CORRECT RESULTS.
//
// Exit codes are not evidence. `mache serve` exits 0 while serving an empty
// graph; a projection that lost every symbol answers find_definition with a
// well-formed "no definition found". Both are green to a smoke test that only
// checks status. So every assertion below names a specific expected VALUE
// derived from the fixture — a node id, a symbol count, a rule id — and the
// fixture is small enough that those values are stated, not computed.

// fixtureChecksumDef is the projection address of ComputeChecksum, the
// fixture's first top-level declaration. Stating the exact id (rather than
// asserting non-emptiness) is what makes this an assertion about CORRECTNESS:
// a backend that returns the right count of wrong addresses fails here.
const fixtureChecksumDef = "pkg/checksum.go/function_declaration_0"

// buildFixtureDB projects the fixture with the INSTALLED binary and returns
// the .db path. This is the `mache build` half of the install: a binary that
// cannot reach a working leyline fails here rather than at serve time.
func buildFixtureDB(t *testing.T, bin string) string {
	t.Helper()
	db := filepath.Join(t.TempDir(), "fixture.db")
	res := runner{}.mustRun(t, bin, "build", fixtureDir(t), db)
	t.Logf("mache build: %s", strings.TrimSpace(res.combined()))

	info := requireFileExists(t, db, "projection built by `mache build`")
	require.NotZero(t, info.Size(), "`mache build` produced an EMPTY .db — exit 0 is not evidence")
	return db
}

// TestInstalledMacheServesCorrectResults drives the core MCP tools against a
// projection the installed binary itself produced, and asserts on their
// content. One server for all of them: the first tool call of a session pays a
// one-off root-resolution timeout, and paying it per assertion would buy
// nothing.
func TestInstalledMacheServesCorrectResults(t *testing.T) {
	bin := macheBinary(t)
	db := buildFixtureDB(t, bin)
	client := startServe(t, bin, db, fixtureDir(t))
	ctx := t.Context()

	t.Run("find_definition returns the expected node id", func(t *testing.T) {
		assertFindDefinition(t, ctx, client)
	})
	t.Run("find_definition discriminates — an absent symbol is not found", func(t *testing.T) {
		assertFindDefinitionMiss(t, ctx, client)
	})
	t.Run("get_overview returns non-empty structure", func(t *testing.T) {
		assertOverview(t, ctx, client)
	})
	t.Run("list_directory returns the projected tree", func(t *testing.T) {
		assertListDirectory(t, ctx, client)
	})
	t.Run("find_smells returns the rule registry", func(t *testing.T) {
		assertSmellRegistry(t, ctx, client)
	})
	t.Run("an unknown tool is rejected rather than silently accepted", func(t *testing.T) {
		_, err := client.callTool(ctx, "no_such_tool", nil)
		assert.Error(t, err, "the server must reject a tool it does not implement")
	})
}

func assertFindDefinition(t *testing.T, ctx context.Context, client *mcpClient) {
	t.Helper()
	body, err := client.callTool(ctx, "find_definition", map[string]any{"symbol": "ComputeChecksum"})
	require.NoError(t, err)

	got := decodeJSON[struct {
		Symbol      string   `json:"symbol"`
		Definitions []string `json:"definitions"`
	}](t, "find_definition", body)

	assert.Equal(t, "ComputeChecksum", got.Symbol)
	assert.Equal(t, []string{fixtureChecksumDef}, got.Definitions,
		"the projection must address the fixture's first declaration exactly")
}

// assertFindDefinitionMiss is the falsifier for assertFindDefinition: if
// find_definition answered every query with the same non-empty list, that
// assertion would pass for the wrong reason.
func assertFindDefinitionMiss(t *testing.T, ctx context.Context, client *mcpClient) {
	t.Helper()
	body, err := client.callTool(ctx, "find_definition",
		map[string]any{"symbol": "NoSuchSymbolInTheFixture"})
	require.NoError(t, err, "a miss is a result, not a transport failure")
	assert.NotContains(t, body, fixtureChecksumDef,
		"a symbol that does not exist must not resolve to one that does")
}

func assertOverview(t *testing.T, ctx context.Context, client *mcpClient) {
	t.Helper()
	body, err := client.callTool(ctx, "get_overview", nil)
	require.NoError(t, err)

	got := decodeJSON[struct {
		TopLevel []struct {
			Name     string `json:"name"`
			Path     string `json:"path"`
			Children int    `json:"children"`
		} `json:"top_level"`
		TotalDirs  int `json:"total_dirs"`
		TotalFiles int `json:"total_files"`
		DefTokens  int `json:"def_tokens"`
	}](t, "get_overview", body)

	assert.NotEmpty(t, got.TopLevel, "overview must describe the projected tree")
	assert.Positive(t, got.TotalFiles, "a projection with no files is a silent ingestion failure")
	assert.GreaterOrEqual(t, got.DefTokens, 2,
		"the fixture defines ComputeChecksum and VerifyChecksum; fewer means definitions were lost")

	names := make([]string, 0, len(got.TopLevel))
	for _, d := range got.TopLevel {
		names = append(names, d.Name)
	}
	assert.Contains(t, names, "pkg", "the fixture's only package directory must appear at top level")
}

func assertListDirectory(t *testing.T, ctx context.Context, client *mcpClient) {
	t.Helper()
	body, err := client.callTool(ctx, "list_directory", map[string]any{"path": ""})
	require.NoError(t, err)

	entries := decodeJSON[[]struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Type string `json:"type"`
	}](t, "list_directory", body)
	require.NotEmpty(t, entries, "the root of a built projection is never empty")

	var sawPkgDir bool
	for _, e := range entries {
		if e.Name == "pkg" && e.Type == "dir" {
			sawPkgDir = true
		}
	}
	assert.True(t, sawPkgDir, "expected a `pkg` directory entry, got %s", body)
}

// assertSmellRegistry calls find_smells with no `rule`, which answers with the
// rule registry — a known-shape result that does not depend on how much debt
// the tiny fixture happens to contain, so the assertion is about the tool
// working rather than about the fixture's contents.
func assertSmellRegistry(t *testing.T, ctx context.Context, client *mcpClient) {
	t.Helper()
	body, err := client.callTool(ctx, "find_smells", nil)
	require.NoError(t, err)

	got := decodeJSON[struct {
		Help  string `json:"help"`
		Rules []struct {
			ID       string `json:"id"`
			Severity string `json:"severity"`
		} `json:"rules"`
	}](t, "find_smells", body)

	assert.NotEmpty(t, got.Help)
	require.NotEmpty(t, got.Rules, "the built-in rule set is never empty")
	ids := make([]string, 0, len(got.Rules))
	for _, r := range got.Rules {
		assert.NotEmpty(t, r.ID, "every rule must carry an id")
		assert.NotEmpty(t, r.Severity, "severity is always emitted, defaulting to warn")
		ids = append(ids, r.ID)
	}
	assert.Contains(t, ids, "duplicate_definitions",
		"a built-in rule went missing from the shipped binary's registry")
}
