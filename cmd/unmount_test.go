package cmd

import (
	"encoding/json"
	"os"
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

	// No meta.json sidecar exists — loadMountMetadata should fail
	_, err := loadMountMetadata(mountPoint)
	assert.Error(t, err, "loadMountMetadata should fail when sidecar missing")

	// The error should be identifiable as "not found" so unmount can fall back
	assert.True(t, os.IsNotExist(err),
		"error should be os.IsNotExist, got: %v", err)
}

func TestSidecarPath(t *testing.T) {
	assert.Equal(t, "/tmp/mache/test.meta.json", sidecarPath("/tmp/mache/test"))
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

	err := saveMountMetadata(mountPoint, meta)
	require.NoError(t, err)

	loaded, err := loadMountMetadata(mountPoint)
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
	loaded, err := loadMountMetadata(filepath.Join(tmpDir, "my-mount"))
	require.NoError(t, err)
	assert.Equal(t, "/some/source", loaded.Source)
}
