package testutil

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMacheRepoRoot_FindsTheModuleRoot pins the runtime.Caller hop count: the
// helper moved from cmd/ (one level deep) to internal/testutil/ (two levels),
// and a wrong ".." count silently resolves to a directory that exists but is
// not the repo — which every consumer would then treat as truth.
func TestMacheRepoRoot_FindsTheModuleRoot(t *testing.T) {
	root := MacheRepoRoot(t)
	require.FileExists(t, filepath.Join(root, "go.mod"))
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "module github.com/agentic-research/mache",
		"must resolve THIS module's root, not some enclosing directory with a go.mod")
}

// TestPresetFixturesDir_PointsAtTheHoistedTree pins the fixture hoist
// (cmd/testdata → internal/testutil/testdata): consumers in three future
// packages resolve fixtures through this one function, so a stale path here
// breaks preset coverage everywhere at once.
func TestPresetFixturesDir_PointsAtTheHoistedTree(t *testing.T) {
	dir := PresetFixturesDir(t)
	require.DirExists(t, dir)
	// The markdown fixture is deliberately NOT mdformat-clean; its presence
	// (and the hook excludes that keep it byte-stable) are part of the
	// contract this helper fronts.
	assert.FileExists(t, filepath.Join(dir, "markdown", "README.md"))
	assert.FileExists(t, filepath.Join(dir, "yaml", "config.yaml"))
}
