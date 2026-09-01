package testutil

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// MacheRepoRoot returns the absolute path of mache's source root.
// Derived from runtime.Caller so the test works regardless of which
// checkout the developer has cd'd into (~/remotes/art/mache vs
// ~/github/art/mache).
func MacheRepoRoot(t testing.TB) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller(0) must succeed")
	// internal/testutil/repo.go → ../../ → repo root
	root := filepath.Clean(filepath.Join(filepath.Dir(here), "..", ".."))
	require.FileExists(t, filepath.Join(root, "go.mod"), "repo root must contain go.mod")
	return root
}

// PresetFixturesDir returns the absolute path of the shared per-language
// preset fixture tree (one tiny idiomatic source file per supported
// language). Hoisted out of cmd/testdata because its consumers end up in
// three different packages after the decomposition, and testdata/ is
// package-dir-relative.
func PresetFixturesDir(t testing.TB) string {
	t.Helper()
	return filepath.Join(MacheRepoRoot(t), "internal", "testutil", "testdata", "preset_fixtures")
}
