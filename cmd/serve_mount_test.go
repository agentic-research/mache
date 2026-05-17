package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withServeMounts swaps the package-level serveMounts flag for the
// duration of the test and restores it on cleanup. Cobra binds the
// flag globally so direct mutation is the practical seam.
func withServeMounts(t *testing.T, mounts []string) {
	t.Helper()
	saved := serveMounts
	serveMounts = mounts
	t.Cleanup(func() { serveMounts = saved })
}

// writeSourceDir lays down a tiny Go source tree mache can ingest
// without going through ley-line — single file under the given dir
// with a known function name.
func writeSourceDir(t *testing.T, fnName string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	src := "package main\n\nfunc " + fnName + "() {}\n"
	require.NoError(t, os.WriteFile(path, []byte(src), 0o600))
	return dir
}

// TestBuildMaybeMultiGraph_NoMountsFallsThroughToSingleSource asserts
// the wrapper is a pass-through when no --mount flags are present.
// Behavior must be byte-identical to calling buildServeGraph directly.
func TestBuildMaybeMultiGraph_NoMountsFallsThroughToSingleSource(t *testing.T) {
	withServeMounts(t, nil)
	dir := writeSourceDir(t, "Foo")

	// Disable auto-leyline so the test exercises the in-process
	// MemoryStore path deterministically.
	t.Setenv("MACHE_NO_LEYLINE", "1")

	g, si, cleanup, err := buildMaybeMultiGraph(dir, &api.Topology{Version: api.SchemaVersion})
	require.NoError(t, err)
	defer cleanup()
	require.NotNil(t, g)
	// Pass-through case: invalidator propagates from buildServeGraph and
	// is non-nil for directory sources.
	require.NotNil(t, si, "no --mount + directory must propagate the SheafInvalidator")

	// We don't assert graph contents here — that would require a
	// non-empty topology and exercise the schema/ingest pipeline,
	// which has its own coverage. The point of this test is to
	// verify the dispatch path: no --mount → buildServeGraph fires
	// → returns a Graph + cleanup with no error.
	_, isComposite := g.(*graph.CompositeGraph)
	assert.False(t, isComposite, "no --mount must not wrap in CompositeGraph")
}

// TestBuildMaybeMultiGraph_MountsBuildComposite asserts that with
// two --mount flags, the result is a CompositeGraph whose virtual
// root lists both mount names.
func TestBuildMaybeMultiGraph_MountsBuildComposite(t *testing.T) {
	authDir := writeSourceDir(t, "Validate")
	billingDir := writeSourceDir(t, "Charge")
	withServeMounts(t, []string{
		"auth=" + authDir,
		"billing=" + billingDir,
	})
	t.Setenv("MACHE_NO_LEYLINE", "1")

	g, si, cleanup, err := buildMaybeMultiGraph("", &api.Topology{Version: api.SchemaVersion})
	require.NoError(t, err)
	defer cleanup()
	// Composite mount mode forfeits the unified cascade — see
	// buildMaybeMultiGraph for the design call.
	assert.Nil(t, si, "composite mounts must return nil *SheafInvalidator")

	root, err := g.GetNode("")
	require.NoError(t, err)
	assert.True(t, root.Mode.IsDir(), "composite root is a directory")

	// CompositeGraph populates the root's children via ListChildren,
	// not GetNode (intentional; keeps GetNode cheap and Mount/Unmount
	// from invalidating cached Node objects).
	children, err := g.ListChildren("")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"auth", "billing"}, children,
		"composite root must list both mount names via ListChildren")
}

// TestBuildMaybeMultiGraph_PositionalSourceWithMountErrors guards the
// "choose one" rule — if the user passes both a positional source
// and --mount, fail loudly rather than silently picking one.
func TestBuildMaybeMultiGraph_PositionalSourceWithMountErrors(t *testing.T) {
	dir := writeSourceDir(t, "Foo")
	withServeMounts(t, []string{"x=" + dir})

	_, _, _, err := buildMaybeMultiGraph(dir, &api.Topology{Version: api.SchemaVersion})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot use both")
}

// TestBuildMaybeMultiGraph_InvalidSpecErrors checks that --mount
// values without an '=' separator (or with empty NAME/PATH) produce
// a clear error and don't leak the partial composite.
func TestBuildMaybeMultiGraph_InvalidSpecErrors(t *testing.T) {
	cases := []struct {
		name string
		spec string
	}{
		{"no_equals", "noequalshere"},
		{"empty_name", "=/some/path"},
		{"empty_path", "name="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withServeMounts(t, []string{tc.spec})
			_, _, _, err := buildMaybeMultiGraph("", &api.Topology{Version: api.SchemaVersion})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "invalid --mount spec")
		})
	}
}
