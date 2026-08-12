package schema_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/schema"
	"github.com/stretchr/testify/require"
)

func TestResolvePreset(t *testing.T) {
	got, err := schema.Resolve("go", t.TempDir())
	require.NoError(t, err)
	require.NotEmpty(t, got.Topology.Nodes)
	require.Equal(t, []string{"go"}, got.Languages)
	require.Contains(t, schema.AvailablePresets(), "terraform")
}

func TestResolveContainedFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "custom.json"),
		[]byte(`{"version":"v1"}`),
		0o644,
	))

	got, err := schema.Resolve("custom.json", dir)
	require.NoError(t, err)
	require.Equal(t, "v1", got.Topology.Version)
	require.Empty(t, got.Languages)
}

func TestResolveRejectsEscape(t *testing.T) {
	_, err := schema.Resolve("../outside.json", t.TempDir())
	require.ErrorContains(t, err, "escapes")
}

func TestResolveUnknownPresetLikeNameReportsAvailablePresets(t *testing.T) {
	_, err := schema.LoadPreset("fortran")
	require.ErrorContains(t, err, `unknown preset schema "fortran"`)
	require.ErrorContains(t, err, "go")
}

func TestParseReportsInvalidJSON(t *testing.T) {
	_, err := schema.ParseTopology([]byte(`{"version":`))
	require.ErrorContains(t, err, "parse schema")
}
