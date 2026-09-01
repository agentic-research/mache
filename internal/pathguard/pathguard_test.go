package pathguard_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/pathguard"
	"github.com/stretchr/testify/require"
)

func TestRequireContained(t *testing.T) {
	base := t.TempDir()
	require.NoError(t, pathguard.RequireContained(filepath.Join(base, "nested", "new.json"), base))
	require.ErrorContains(t,
		pathguard.RequireContained(filepath.Join(base, "..", "outside.json"), base),
		"escapes project directory")
}

func TestRequireContainedRejectsSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(base, "outside-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	require.ErrorContains(t,
		pathguard.RequireContained(filepath.Join(link, "schema.json"), base),
		"escapes project directory")
}
