package resolve

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/graph"
	"github.com/stretchr/testify/require"
)

// newFakeAnchor creates a tempdir with a "modules/vpc" subdirectory
// containing one real Go source file, so LocalPathResolver has something
// leyline can actually parse. LocalPathResolver itself is language-agnostic
// (it just leyline-parses whatever's in the resolved directory) — Go is used
// here purely because it's the fixture already proven reliable elsewhere in
// this package (gomod_resolver_test.go).
func newFakeAnchor(t *testing.T) (anchor string) {
	t.Helper()
	anchor = t.TempDir()
	modDir := filepath.Join(anchor, "modules", "vpc")
	require.NoError(t, os.MkdirAll(modDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(modDir, "vpc.go"),
		[]byte("package vpc\n\nfunc New() string { return \"vpc\" }\n"), 0o644))
	return anchor
}

func TestLocalPathResolver_ResolvesRelativeLocator(t *testing.T) {
	anchor := newFakeAnchor(t)
	r := &LocalPathResolver{Anchor: anchor}

	g, err := r.Resolve(context.Background(), "./modules/vpc")
	require.NoError(t, err)
	require.NotNil(t, g)

	ld, ok := g.(graph.DefsLookuper)
	require.True(t, ok, "resolved graph must satisfy graph.DefsLookuper")
	require.NotEmpty(t, ld.LookupDef("New"), "must find the function actually defined in the resolved directory")
}

func TestLocalPathResolver_ResolvesAbsoluteLocator(t *testing.T) {
	anchor := newFakeAnchor(t)
	r := &LocalPathResolver{Anchor: anchor}

	g, err := r.Resolve(context.Background(), filepath.Join(anchor, "modules", "vpc"))
	require.NoError(t, err)
	require.NotNil(t, g)
}

func TestLocalPathResolver_NonLocalLocatorIsNotResolvable(t *testing.T) {
	r := &LocalPathResolver{Anchor: t.TempDir()}

	_, err := r.Resolve(context.Background(), "github.com/foo/bar")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNotResolvable)
}

func TestLocalPathResolver_EscapingLocatorIsRejected(t *testing.T) {
	anchor := newFakeAnchor(t)
	r := &LocalPathResolver{Anchor: filepath.Join(anchor, "modules")}

	_, err := r.Resolve(context.Background(), "../../../../../../etc")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNotResolvable)
}

func TestLocalPathResolver_MissingAnchorErrorsOnRelativeLocator(t *testing.T) {
	r := &LocalPathResolver{}
	_, err := r.Resolve(context.Background(), "./modules/vpc")
	require.Error(t, err)
}

func TestLocalPathResolver_CachesRepeatedResolution(t *testing.T) {
	anchor := newFakeAnchor(t)
	r := &LocalPathResolver{Anchor: anchor}

	assertResolveIsCached(t, func() (graph.Graph, error) {
		return r.Resolve(context.Background(), "./modules/vpc")
	})
}

func TestIsLocalRelativeLocator(t *testing.T) {
	cases := map[string]bool{
		"./modules/vpc":    true,
		"../shared":        true,
		"github.com/x/y":   false,
		"foo/bar":          false,
		"":                 false,
		"git::https://...": false,
		"./":               true,
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			require.Equal(t, want, IsLocalRelativeLocator(in))
		})
	}
}

func TestLocalPathResolver_ViaRegistry(t *testing.T) {
	anchor := newFakeAnchor(t)
	reg := NewRegistry()
	reg.Register("mod", &LocalPathResolver{Anchor: anchor})

	g, err := reg.Resolve(context.Background(), "mod", "./modules/vpc")
	require.NoError(t, err)
	require.NotNil(t, g)
}
