package mcpserve

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/testutil"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resolveRefText extracts the JSON body from a resolve_ref tool result and
// unmarshals it — a small helper shared by the mount-pipeline tests below.
func resolveRefText(t *testing.T, result *mcp.CallToolResult) resolveRefResponse {
	t.Helper()
	tc, ok := result.Content[0].(mcp.TextContent)
	require.True(t, ok)
	var resp resolveRefResponse
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &resp))
	return resp
}

// Tests cover the resolveModScheme branch directly so we don't need an
// MCP transport spun up — the handler is a thin JSON wrapper.

func TestResolveModScheme_LocalDir(t *testing.T) {
	root := t.TempDir()
	moduleDir := filepath.Join(root, "modules", "vpc")
	require.NoError(t, os.MkdirAll(moduleDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(moduleDir, "main.tf"), []byte("resource \"aws_vpc\" \"this\" {}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(moduleDir, "outputs.tf"), []byte("output \"vpc_id\" {}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(moduleDir, "README.md"), []byte("# VPC"), 0o644))

	// base_path is a terraform file in the project root.
	basePath := filepath.Join(root, "main.tf")
	require.NoError(t, os.WriteFile(basePath, []byte("module \"vpc\" { source = \"./modules/vpc\" }"), 0o644))

	got := resolveModScheme("./modules/vpc", basePath)
	assert.Equal(t, "mod", got.Scheme)
	assert.Equal(t, "./modules/vpc", got.Locator)
	assert.True(t, got.Exists, "module dir should exist")
	assert.True(t, got.IsDir)
	assert.Empty(t, got.Error)
	assert.Empty(t, got.RemoteHint)

	resolved, err := filepath.EvalSymlinks(got.Resolved)
	require.NoError(t, err)
	expected, err := filepath.EvalSymlinks(moduleDir)
	require.NoError(t, err)
	assert.Equal(t, expected, resolved)

	// Files should include both .tf entries with terraform language
	// detected, plus the README.md without a lang tag.
	names := map[string]resolveRefEntry{}
	for _, f := range got.Files {
		names[f.Name] = f
	}
	require.Contains(t, names, "main.tf")
	require.Contains(t, names, "outputs.tf")
	require.Contains(t, names, "README.md")
	assert.Equal(t, "terraform", names["main.tf"].Lang)
	assert.Equal(t, "terraform", names["outputs.tf"].Lang)
}

func TestResolveModScheme_LocalDirRelativeToDir(t *testing.T) {
	// Same as above but base_path is a directory rather than a file —
	// the resolver should still anchor relative locators against it.
	root := t.TempDir()
	moduleDir := filepath.Join(root, "modules", "vpc")
	require.NoError(t, os.MkdirAll(moduleDir, 0o755))

	got := resolveModScheme("./modules/vpc", root)
	assert.True(t, got.Exists)
	assert.True(t, got.IsDir)
	assert.Empty(t, got.Error)
}

func TestResolveModScheme_RemoteLocatorReturnsHint(t *testing.T) {
	got := resolveModScheme("github.com/foo/bar", t.TempDir())
	assert.Equal(t, "github.com/foo/bar", got.Locator)
	assert.False(t, got.Exists)
	assert.NotEmpty(t, got.RemoteHint, "remote locator must surface a hint")
	assert.Empty(t, got.Resolved)
	assert.Empty(t, got.Files)
}

func TestResolveModScheme_LocalLocatorRequiresBasePath(t *testing.T) {
	got := resolveModScheme("./modules/vpc", "")
	assert.Empty(t, got.Resolved)
	assert.NotEmpty(t, got.Error)
	assert.Contains(t, got.Error, "base_path")
}

func TestResolveModScheme_TargetMissing(t *testing.T) {
	got := resolveModScheme("./does/not/exist", t.TempDir())
	assert.False(t, got.Exists)
	assert.NotEmpty(t, got.Error, "missing target must surface an error")
	assert.NotEmpty(t, got.Resolved, "Resolved should still be set so the caller can see what was tried")
}

func TestResolveModScheme_DotSlashFile(t *testing.T) {
	// When the locator points at a single file (not a directory), Files is
	// empty and IsDir is false.
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "shared.tf"), []byte("variable \"x\" {}"), 0o644))

	got := resolveModScheme("./shared.tf", root)
	assert.True(t, got.Exists)
	assert.False(t, got.IsDir)
	assert.Empty(t, got.Files)
}

func TestResolveResponseSerializesCleanly(t *testing.T) {
	// JSON roundtrip catches struct-tag typos and exported-field issues.
	resp := resolveRefResponse{
		Scheme:   "mod",
		Locator:  "./vpc",
		Resolved: "/abs/vpc",
		Exists:   true,
		IsDir:    true,
		Files: []resolveRefEntry{
			{Name: "main.tf", IsDir: false, Size: 42, Lang: "terraform"},
			{Name: "subdir", IsDir: true},
		},
	}
	b, err := json.Marshal(resp)
	require.NoError(t, err)

	var back resolveRefResponse
	require.NoError(t, json.Unmarshal(b, &back))
	assert.Equal(t, resp, back)
}

// --- mache-be0b9f: resolve_ref mounts a queryable sub-graph -------------
//
// Unlike the resolveModScheme tests above (pure filesystem metadata, no
// graph), these exercise the full pipeline through a real *lazyGraph:
// resolve_ref resolves the token AND mounts the resulting graph, returning
// graph_path, which list_directory/find_definition-style calls (lg itself
// implements graph.Graph) can then query directly.

// newResolveRefFixture creates a tempdir with a "modules/vpc" subdirectory
// containing one real Go source file mod_test.go leyline can parse, plus a
// referencing file at the root — mirroring newResolveRefFixture's
// Terraform-flavored callers elsewhere in this file, but using Go source so
// the resolved sub-graph actually has a definition to look up.
func newResolveRefFixture(t *testing.T) (root, basePath string) {
	t.Helper()
	root = t.TempDir()
	modDir := filepath.Join(root, "modules", "vpc")
	require.NoError(t, os.MkdirAll(modDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(modDir, "vpc.go"),
		[]byte("package vpc\n\nfunc New() string { return \"vpc\" }\n"), 0o644))
	basePath = filepath.Join(root, "main.tf")
	require.NoError(t, os.WriteFile(basePath, []byte("module \"vpc\" { source = \"./modules/vpc\" }"), 0o644))
	return root, basePath
}

func TestMakeResolveRefHandler_ModSchemeMountsQueryableGraph(t *testing.T) {
	root, basePath := newResolveRefFixture(t)
	lg := &lazyGraph{basePath: root}

	handler := makeResolveRefHandler(lg)
	result, err := handler(context.Background(), testutil.MakeRequest(map[string]any{
		"token":     "mod:./modules/vpc",
		"base_path": basePath,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError, "%+v", result)

	resp := resolveRefText(t, result)
	require.True(t, resp.Exists)
	require.Empty(t, resp.Error)
	require.NotEmpty(t, resp.GraphPath, "successful mod: resolution against a mounter must set graph_path")
	require.Contains(t, resp.GraphPath, "resolve/")

	// The mounted sub-graph is queryable through lg itself, exactly as
	// list_directory / find_definition would query it.
	children, err := lg.ListChildren(resp.GraphPath)
	require.NoError(t, err)
	assert.Contains(t, children, resp.GraphPath+"/vpc.go")

	defs := lg.LookupDef("New")
	require.NotEmpty(t, defs, "find_definition-style LookupDef must see the mounted sub-graph's own definitions")
}

func TestMakeResolveRefHandler_ModSchemeMountIsIdempotent(t *testing.T) {
	root, basePath := newResolveRefFixture(t)
	lg := &lazyGraph{basePath: root}
	handler := makeResolveRefHandler(lg)

	call := func() resolveRefResponse {
		result, err := handler(context.Background(), testutil.MakeRequest(map[string]any{
			"token":     "mod:./modules/vpc",
			"base_path": basePath,
		}))
		require.NoError(t, err)
		require.False(t, result.IsError)
		return resolveRefText(t, result)
	}

	first := call()
	second := call()
	assert.Equal(t, first.GraphPath, second.GraphPath, "repeated resolution of the same target must reuse the same mount")
}

func TestMakeResolveRefHandler_GomodSchemeMountsQueryableGraph(t *testing.T) {
	// gomod: resolves against the served root's own go.mod — mache's own
	// repo root already imports testify, so this exercises the real `go
	// list` + build.Parse + graph.Open pipeline against a real dependency, not a
	// synthetic fixture.
	//
	// repoRoot is derived from this source file's own compiled-in path
	// (runtime.Caller), NOT os.Getwd() — several sibling tests in this
	// package (cli_test.go, init_test.go) os.Chdir into a tempdir for their
	// duration; os.Getwd() here raced one of those and briefly observed
	// their already-deleted tempdir instead of the repo root.
	repoRoot := testutil.MacheRepoRoot(t) // hop-count-proof: hand-counted ".." broke on the stage-8 move
	lg := &lazyGraph{basePath: repoRoot}

	handler := makeResolveRefHandler(lg)
	result, err := handler(context.Background(), testutil.MakeRequest(map[string]any{
		"token": "gomod:github.com/stretchr/testify/require",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError, "%+v", result)

	resp := resolveRefText(t, result)
	require.True(t, resp.Exists, "resp.Error=%q resp.RemoteHint=%q repoRoot=%q", resp.Error, resp.RemoteHint, repoRoot)
	require.NotEmpty(t, resp.GraphPath)
	require.NotEmpty(t, resp.Files, "gomod scheme should populate Files for orientation, same as mod")

	// Query the mount directly via ListChildren (prefix-routed, doesn't
	// touch lg.get()/the base graph) rather than LookupDef's federated
	// path — federation itself is already covered by the mod-scheme test
	// above, and lg.get() here would trigger a full ingest of mache's own
	// ~800-file repo (lg.basePath == repoRoot) just to prove a fact this
	// test doesn't need restated.
	children, err := lg.ListChildren(resp.GraphPath)
	require.NoError(t, err)
	require.NotEmpty(t, children)
}

func TestMakeResolveRefHandler_UnmountableGraphDegradesGracefully(t *testing.T) {
	// makeResolveRefHandler is also called directly against plain
	// graph.Graph fixtures (see all_tools_e2e_test.go) that don't
	// implement resolveMounter. Must not panic or error — just skip the
	// graph_path enrichment.
	_, basePath := newResolveRefFixture(t)
	store := graph.NewMemoryStore()

	handler := makeResolveRefHandler(store)
	result, err := handler(context.Background(), testutil.MakeRequest(map[string]any{
		"token":     "mod:./modules/vpc",
		"base_path": basePath,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	resp := resolveRefText(t, result)
	assert.True(t, resp.Exists, "flat filesystem resolution must still succeed")
	assert.Empty(t, resp.GraphPath, "no mounter available, so graph_path must stay empty")
}
