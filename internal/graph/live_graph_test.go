package graph

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Live graph: MemoryStore detects source file changes and triggers re-ingestion.

func TestMemoryStore_TracksFileMtimes(t *testing.T) {
	store := NewMemoryStore()

	tmpDir := t.TempDir()
	srcFile := filepath.Join(tmpDir, "main.go")
	require.NoError(t, os.WriteFile(srcFile, []byte("func A() {}"), 0o644))

	info, _ := os.Stat(srcFile)
	mtime := info.ModTime()

	// Add a node with Origin pointing to the source file
	node := &Node{
		ID:      "main/functions/A/source",
		Mode:    0o444,
		ModTime: mtime,
		Data:    []byte("func A() {}"),
		Origin: &SourceOrigin{
			FilePath:  srcFile,
			StartByte: 0,
			EndByte:   12,
		},
	}
	store.AddNode(node)

	// Store should track the mtime
	tracked := store.FileMtime(srcFile)
	assert.Equal(t, mtime, tracked, "store should track the file's mtime")
}

func TestMemoryStore_DetectsStaleness(t *testing.T) {
	store := NewMemoryStore()

	tmpDir := t.TempDir()
	srcFile := filepath.Join(tmpDir, "main.go")
	require.NoError(t, os.WriteFile(srcFile, []byte("func A() {}"), 0o644))

	info, _ := os.Stat(srcFile)
	mtime := info.ModTime()

	node := &Node{
		ID:      "main/functions/A/source",
		Mode:    0o444,
		ModTime: mtime,
		Data:    []byte("func A() {}"),
		Origin: &SourceOrigin{
			FilePath:  srcFile,
			StartByte: 0,
			EndByte:   12,
		},
	}
	store.AddNode(node)

	// File hasn't changed — not stale
	assert.False(t, store.IsFileStale(srcFile), "file should not be stale initially")

	// Modify the file
	time.Sleep(10 * time.Millisecond) // ensure mtime changes
	require.NoError(t, os.WriteFile(srcFile, []byte("func B() {}"), 0o644))

	// Now it should be stale
	assert.True(t, store.IsFileStale(srcFile), "file should be stale after modification")
}

func TestMemoryStore_RefresherCalledOnStaleRead(t *testing.T) {
	store := NewMemoryStore()

	tmpDir := t.TempDir()
	srcFile := filepath.Join(tmpDir, "main.go")
	require.NoError(t, os.WriteFile(srcFile, []byte("func A() {}"), 0o644))

	info, _ := os.Stat(srcFile)
	mtime := info.ModTime()

	node := &Node{
		ID:      "main/functions/A/source",
		Mode:    0o444,
		ModTime: mtime,
		Data:    []byte("func A() {}"),
		Origin: &SourceOrigin{
			FilePath:  srcFile,
			StartByte: 0,
			EndByte:   12,
		},
	}
	store.AddNode(node)

	// Track refresher calls
	var refreshedFiles []string
	store.SetRefresher(func(filePath string) error {
		refreshedFiles = append(refreshedFiles, filePath)
		// Simulate re-ingestion: update the node content and mtime
		newInfo, _ := os.Stat(filePath)
		newData, _ := os.ReadFile(filePath)
		store.ReplaceFileNodes(filePath, []*Node{
			{
				ID:      "main/functions/A/source",
				Mode:    0o444,
				ModTime: newInfo.ModTime(),
				Data:    newData,
				Origin: &SourceOrigin{
					FilePath:  filePath,
					StartByte: 0,
					EndByte:   uint32(len(newData)),
				},
			},
		})
		store.RecordFileMtime(filePath, newInfo.ModTime())
		return nil
	})

	// Modify the file
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(srcFile, []byte("func B() {}"), 0o644))

	// Read content — should trigger refresh
	buf := make([]byte, 100)
	n, err := store.ReadContent("main/functions/A/source", buf, 0)
	require.NoError(t, err)

	// Refresher should have been called
	assert.Len(t, refreshedFiles, 1, "refresher should be called once")
	assert.Equal(t, srcFile, refreshedFiles[0])

	// Content should be updated
	assert.Equal(t, "func B() {}", string(buf[:n]))
}

func TestMemoryStore_NoRefreshWhenNotStale(t *testing.T) {
	store := NewMemoryStore()

	tmpDir := t.TempDir()
	srcFile := filepath.Join(tmpDir, "main.go")
	require.NoError(t, os.WriteFile(srcFile, []byte("func A() {}"), 0o644))

	info, _ := os.Stat(srcFile)

	node := &Node{
		ID:      "main/functions/A/source",
		Mode:    0o444,
		ModTime: info.ModTime(),
		Data:    []byte("func A() {}"),
		Origin: &SourceOrigin{
			FilePath:  srcFile,
			StartByte: 0,
			EndByte:   12,
		},
	}
	store.AddNode(node)

	refreshCalled := false
	store.SetRefresher(func(filePath string) error {
		refreshCalled = true
		return nil
	})

	// Read without modifying — refresher should NOT be called
	buf := make([]byte, 100)
	_, err := store.ReadContent("main/functions/A/source", buf, 0)
	require.NoError(t, err)

	assert.False(t, refreshCalled, "refresher should not be called when file is fresh")
}

func TestMemoryStore_RefresherNotCalledForVirtualNodes(t *testing.T) {
	store := NewMemoryStore()

	// Virtual node (no Origin — e.g., _schema.json, context)
	node := &Node{
		ID:   "virtual/file",
		Mode: 0o444,
		Data: []byte("virtual content"),
	}
	store.AddNode(node)

	refreshCalled := false
	store.SetRefresher(func(filePath string) error {
		refreshCalled = true
		return nil
	})

	buf := make([]byte, 100)
	_, err := store.ReadContent("virtual/file", buf, 0)
	require.NoError(t, err)

	assert.False(t, refreshCalled, "refresher should not be called for virtual nodes")
}
