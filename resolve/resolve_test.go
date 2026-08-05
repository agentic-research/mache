package resolve_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/resolve"
	"github.com/stretchr/testify/require"
)

// TestPublicFacade_GoModResolver proves GoModResolver is usable by an
// external consumer that only imports the public resolve (and graph)
// packages — no internal/... import required. This is the fix for "the
// gomod resolver isn't accessible outside of internal": before this
// package existed, GoModResolver lived only in internal/resolve, which
// Go's internal/ visibility rule makes unimportable from outside this
// module.
func TestPublicFacade_GoModResolver(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/fakemod\n\ngo 1.26\n"), 0o644))
	pkgDir := filepath.Join(dir, "greet")
	require.NoError(t, os.MkdirAll(pkgDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "greet.go"),
		[]byte("package greet\n\nfunc Hello() string { return \"hi\" }\n"), 0o644))

	reg := resolve.NewRegistry()
	reg.Register("gomod", &resolve.GoModResolver{WorkDir: dir})

	g, err := reg.Resolve(context.Background(), "gomod", "example.com/fakemod/greet")
	require.NoError(t, err)
	require.NotNil(t, g)

	type lookupDefer interface{ LookupDef(string) []string }
	ld, ok := g.(lookupDefer)
	require.True(t, ok)
	require.NotEmpty(t, ld.LookupDef("Hello"))
}

// TestPublicFacade_LocalPathResolver mirrors the above for the mod scheme.
func TestPublicFacade_LocalPathResolver(t *testing.T) {
	anchor := t.TempDir()
	modDir := filepath.Join(anchor, "modules", "vpc")
	require.NoError(t, os.MkdirAll(modDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(modDir, "vpc.go"),
		[]byte("package vpc\n\nfunc New() string { return \"vpc\" }\n"), 0o644))

	reg := resolve.NewRegistry()
	reg.Register("mod", &resolve.LocalPathResolver{Anchor: anchor})

	g, err := reg.Resolve(context.Background(), "mod", "./modules/vpc")
	require.NoError(t, err)
	require.NotNil(t, g)
}

// TestPublicFacade_UnknownScheme proves the sentinel errors are reachable
// (and errors.Is-matchable) through the public alias too.
func TestPublicFacade_UnknownScheme(t *testing.T) {
	reg := resolve.NewRegistry()
	_, err := reg.Resolve(context.Background(), "npm", "react")
	require.Error(t, err)
	require.ErrorIs(t, err, resolve.ErrSchemeUnknown)
}
