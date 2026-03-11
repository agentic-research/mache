package ingest

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test that Engine.ReIngestFile re-ingests a single file
// without changing RootPath (needed for live graph updates).

func TestEngine_ReIngestFile(t *testing.T) {
	schema := loadGoSchema(t)
	tmpDir := t.TempDir()

	// Create initial Go file
	goFile := filepath.Join(tmpDir, "main.go")
	err := os.WriteFile(goFile, []byte("package main\nfunc Hello() {}\n"), 0o644)
	require.NoError(t, err)

	store := graph.NewMemoryStore()
	engine := NewEngine(schema, store)
	require.NoError(t, engine.Ingest(tmpDir))

	// Verify initial state
	_, err = store.GetNode("main/functions/Hello/source")
	require.NoError(t, err, "Hello should exist after initial ingest")

	// Modify the file
	time.Sleep(10 * time.Millisecond)
	err = os.WriteFile(goFile, []byte("package main\nfunc Goodbye() {}\n"), 0o644)
	require.NoError(t, err)

	// ReIngestFile should update just this file
	err = engine.ReIngestFile(goFile)
	require.NoError(t, err)

	// Hello should be gone, Goodbye should exist
	_, err = store.GetNode("main/functions/Hello/source")
	assert.ErrorIs(t, err, graph.ErrNotFound, "Hello should be gone after re-ingest")

	_, err = store.GetNode("main/functions/Goodbye/source")
	require.NoError(t, err, "Goodbye should exist after re-ingest")
}

func TestEngine_ReIngestFile_PreservesRootPath(t *testing.T) {
	schema := loadGoSchema(t)
	tmpDir := t.TempDir()

	// Create a Go file in a subdirectory
	subDir := filepath.Join(tmpDir, "pkg")
	require.NoError(t, os.Mkdir(subDir, 0o755))
	goFile := filepath.Join(subDir, "util.go")
	err := os.WriteFile(goFile, []byte("package pkg\nfunc Util() {}\n"), 0o644)
	require.NoError(t, err)

	store := graph.NewMemoryStore()
	engine := NewEngine(schema, store)
	require.NoError(t, engine.Ingest(tmpDir))

	// Verify: node paths are relative to RootPath (tmpDir)
	_, err = store.GetNode("pkg/functions/Util/source")
	require.NoError(t, err)

	// Modify and re-ingest
	time.Sleep(10 * time.Millisecond)
	err = os.WriteFile(goFile, []byte("package pkg\nfunc Helper() {}\n"), 0o644)
	require.NoError(t, err)

	err = engine.ReIngestFile(goFile)
	require.NoError(t, err)

	// Node paths should still be relative to original RootPath
	_, err = store.GetNode("pkg/functions/Helper/source")
	require.NoError(t, err, "Helper should use paths relative to original RootPath")

	_, err = store.GetNode("pkg/functions/Util/source")
	assert.ErrorIs(t, err, graph.ErrNotFound, "Util should be gone")
}
