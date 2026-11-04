package leyline_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/leyline"
	"github.com/agentic-research/mache/internal/lltest"
)

// TestWritableDaemon_WritesOnlyWhereTheDaemonSaysItServes: an adopted daemon
// (reached by LEYLINE_SOCKET, so not spawned by this process) is written
// through exactly when its status.source_root is the tree, reached directly
// or through a symlink. One serving another tree, or no tree, is refused, and
// the refusal names what it serves and where the write was going
// (ley-line-open-246526).
func TestWritableDaemon_WritesOnlyWhereTheDaemonSaysItServes(t *testing.T) {
	_, err := leyline.WritableDaemon("")
	require.ErrorIs(t, err, leyline.ErrNoWritableDaemon)

	tree := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tree, "a.go"), []byte(proposeBase), 0o644))
	link := filepath.Join(t.TempDir(), "via-link")
	require.NoError(t, os.Symlink(tree, link))
	other := t.TempDir()

	t.Setenv("LEYLINE_SOCKET", lltest.StartPinnedSourceDaemon(t, tree))
	for _, root := range []string{tree, link} {
		c, err := leyline.WritableDaemon(root)
		require.NoError(t, err, "the daemon serves %s", root)
		read, err := c.ReadSource("a.go")
		require.NoError(t, err)
		assert.Equal(t, proposeBase, read.Content, "and paths are relative to it")
		_ = c.Close()
	}

	_, err = leyline.WritableDaemon(other)
	require.ErrorIs(t, err, leyline.ErrNoWritableDaemon)
	assert.Contains(t, err.Error(), leyline.CanonicalSourceRoot(tree), "names what the daemon serves")
	assert.Contains(t, err.Error(), leyline.CanonicalSourceRoot(other), "and where the write was going")

	t.Setenv("LEYLINE_SOCKET", lltest.StartPinnedDaemon(t))
	_, err = leyline.WritableDaemon(tree)
	require.ErrorIs(t, err, leyline.ErrNoWritableDaemon)
	assert.Contains(t, err.Error(), "without --source")
}
