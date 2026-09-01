package mountmeta

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #8: mache unmount fails — meta.json not found
// unmount should work even without a sidecar meta.json file.

func TestUnmount_MissingMetaJSON(t *testing.T) {
	tmpDir := t.TempDir()
	mountPoint := filepath.Join(tmpDir, "test-mount")
	require.NoError(t, os.MkdirAll(mountPoint, 0o755))

	// No meta.json sidecar exists — LoadMountMetadata should fail
	_, err := LoadMountMetadata(mountPoint)
	assert.Error(t, err, "LoadMountMetadata should fail when sidecar missing")

	// The error should be identifiable as "not found" so unmount can fall back
	assert.True(t, os.IsNotExist(err),
		"error should be os.IsNotExist, got: %v", err)
}

func TestSidecarPath(t *testing.T) {
	assert.Equal(t, "/tmp/mache/test.meta.json", SidecarPath("/tmp/mache/test"))
}

func TestSaveThenLoadMetadata(t *testing.T) {
	tmpDir := t.TempDir()
	mountPoint := filepath.Join(tmpDir, "test-mount")
	require.NoError(t, os.MkdirAll(mountPoint, 0o755))

	meta := &MountMetadata{
		MountPoint: mountPoint,
		Source:     "/some/source",
		PID:        12345,
	}

	err := SaveMountMetadata(mountPoint, meta)
	require.NoError(t, err)

	loaded, err := LoadMountMetadata(mountPoint)
	require.NoError(t, err)
	assert.Equal(t, mountPoint, loaded.MountPoint)
	assert.Equal(t, "/some/source", loaded.Source)
	assert.Equal(t, 12345, loaded.PID)
}

// Issue #9: mache list doesn't detect NFS mounts without metadata
func TestListActiveMounts_WithSidecar(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a sidecar file directly
	meta := MountMetadata{
		MountPoint: filepath.Join(tmpDir, "my-mount"),
		Source:     "/some/source",
		PID:        os.Getpid(),
	}
	data, err := json.Marshal(meta)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tmpDir, "my-mount.meta.json"), data, 0o644)
	require.NoError(t, err)

	// Test sidecar parsing directly
	loaded, err := LoadMountMetadata(filepath.Join(tmpDir, "my-mount"))
	require.NoError(t, err)
	assert.Equal(t, "/some/source", loaded.Source)
}

// TestAgentMountsDir pins the contract ListActiveMounts depends on: the dir
// is created on demand and stable across calls (TMPDIR-scoped, so tests
// isolate via t.Setenv).
func TestAgentMountsDir(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	dir, err := AgentMountsDir()
	require.NoError(t, err)
	require.DirExists(t, dir)

	again, err := AgentMountsDir()
	require.NoError(t, err)
	assert.Equal(t, dir, again, "the mounts dir must be stable across calls")
}

// TestIsProcessRunning pins both directions: our own live pid, and a freshly
// reaped child whose pid is guaranteed dead — not a made-up number that some
// other process could be recycled onto.
func TestIsProcessRunning(t *testing.T) {
	assert.True(t, IsProcessRunning(os.Getpid()), "our own process is running")

	cmd := exec.Command("true")
	require.NoError(t, cmd.Run())
	assert.False(t, IsProcessRunning(cmd.Process.Pid),
		"a reaped child must read as not running")
}
