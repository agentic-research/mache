package fsutil

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDirectoryOrParent(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "data.json")
	require.NoError(t, os.WriteFile(file, []byte("[]"), 0o644))
	require.Equal(t, dir, DirectoryOrParent(file))
	require.Equal(t, dir, DirectoryOrParent(dir))

	missing := filepath.Join(dir, "missing")
	require.Equal(t, missing, DirectoryOrParent(missing))
}
