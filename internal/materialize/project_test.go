package materialize

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/stretchr/testify/require"
)

func TestExportProjectionMissingSource(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "missing.json")
	target := filepath.Join(dir, "out.json")
	require.ErrorIs(t, ExportProjection(source, target, "json", &api.Topology{}), os.ErrNotExist)
	require.NoFileExists(t, target)
}
