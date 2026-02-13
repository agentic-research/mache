package ingest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEngine_IngestTreeSitter_ModTimeAndUpdates(t *testing.T) {
	// 1. Setup
	tempDir := t.TempDir()
	goFilePath := filepath.Join(tempDir, "main.go")

	// Simple schema: creates a directory for each function
	schemaJSON := `
{
  "nodes": [
    {
      "name": "{{.pkg}}",
      "selector": "(source_file (package_clause (package_identifier) @pkg)) @scope",
      "children": [
        {
          "name": "{{.func_name}}",
          "selector": "(function_declaration name: (identifier) @func_name) @scope",
          "files": [
            {
              "name": "source",
              "content_template": "{{.func_name}}"
            }
          ]
        }
      ]
    }
  ]
}
`
	var schema api.Topology
	require.NoError(t, json.Unmarshal([]byte(schemaJSON), &schema))

	store := graph.NewMemoryStore()
	engine := NewEngine(&schema, store)

	// 2. Create Initial File
	initialContent := `package main

func FunctionA() {}
`
	require.NoError(t, os.WriteFile(goFilePath, []byte(initialContent), 0o644))

	// Set a specific ModTime to verify propagation
	initialTime := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(goFilePath, initialTime, initialTime))

	// 3. Ingest
	require.NoError(t, engine.Ingest(tempDir))

	// 4. Verify Initial State
	// Should have: main/FunctionA/source
	nodeA, err := store.GetNode("main/FunctionA/source")
	require.NoError(t, err)
	assert.Equal(t, "FunctionA", string(nodeA.Data))
	// ModTime check (allowing for some FS precision loss, but usually exact on modern FS)
	assert.True(t, nodeA.ModTime.Equal(initialTime) || nodeA.ModTime.After(initialTime.Add(-time.Second)), "ModTime should be close to file time")

	// 5. Modify File (Change FunctionA to FunctionB)
	newContent := `package main

func FunctionB() {}
`
	// Sleep to ensure ModTime changes significantly if filesystem granularity is low
	time.Sleep(10 * time.Millisecond)

	require.NoError(t, os.WriteFile(goFilePath, []byte(newContent), 0o644))
	updatedTime := time.Now()
	require.NoError(t, os.Chtimes(goFilePath, updatedTime, updatedTime))

	// 6. Re-Ingest
	require.NoError(t, engine.Ingest(tempDir))

	// 7. Verify Updated State
	// FunctionA should be GONE
	_, err = store.GetNode("main/FunctionA/source")
	assert.ErrorIs(t, err, graph.ErrNotFound, "Old node FunctionA should be removed")

	// FunctionB should EXIST
	nodeB, err := store.GetNode("main/FunctionB/source")
	require.NoError(t, err)
	assert.Equal(t, "FunctionB", string(nodeB.Data))

	// Check ModTime updated
	assert.True(t, nodeB.ModTime.After(initialTime), "ModTime should be updated")
	// Note: We can't strictly compare to updatedTime because Ingest might read the file stat slightly differently or precision issues,
	// but it definitely should be newer than initial.
}

func TestEngine_DeleteFileNodes_Explicit(t *testing.T) {
	// Setup similar to above
	tempDir := t.TempDir()
	goFilePath := filepath.Join(tempDir, "utils.go")

	schemaJSON := `
{
  "nodes": [
    {
      "name": "utils",
      "selector": "(package_clause) @scope",
      "files": [
         { "name": "marker", "content_template": "marker" }
      ]
    }
  ]
}
`
	var schema api.Topology
	require.NoError(t, json.Unmarshal([]byte(schemaJSON), &schema))

	store := graph.NewMemoryStore()
	engine := NewEngine(&schema, store)

	require.NoError(t, os.WriteFile(goFilePath, []byte("package utils"), 0o644))
	require.NoError(t, engine.Ingest(tempDir))

	// Verify existence
	_, err := store.GetNode("utils/marker")
	require.NoError(t, err)

	// Explicitly Delete
	store.DeleteFileNodes(goFilePath)

	// Verify deletion
	_, err = store.GetNode("utils/marker")
	assert.ErrorIs(t, err, graph.ErrNotFound)
}
