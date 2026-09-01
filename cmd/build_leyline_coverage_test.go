package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunBuildViaLeylineSchema_UnparseableLanguageErrors pins the CLI's
// delegation to the public build coverage guard. An explicit schema must not
// emit a hollow database when the pinned leyline has no grammar for its source.
func TestRunBuildViaLeylineSchema_UnparseableLanguageErrors(t *testing.T) {
	testutil.RequirePinnedLeyline(t)
	saved := saveBuildFlags()
	defer saved.restore()

	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, "conf.cue"),
		[]byte("package conf\n\nx: 1\n"), 0o644))
	output := filepath.Join(t.TempDir(), "out.db")

	schemaPath = "cue"
	err := runBuildViaLeyline(source, output)
	require.Error(t, err, "preset-ref hollow projection must fail")
	assert.Contains(t, err.Error(), "cue")
	assert.Contains(t, err.Error(), "no fallback parser")
	assert.NotContains(t, err.Error(), "--backend=tree-sitter")

	work := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(work, "schema.json"),
		[]byte(`{"version":"v1","nodes":[{"name":"fields","selector":"(field) @f","language":"cue"}]}`), 0o644))
	t.Chdir(work)
	schemaPath = "schema.json"
	err = runBuildViaLeyline(source, output)
	require.Error(t, err, "hint-based hollow projection must fail")
	assert.Contains(t, err.Error(), "cue")
}
