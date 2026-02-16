package ingest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEngine_IngestRawFile_MixedRepo(t *testing.T) {
	schema := loadGoSchema(t)

	tmpDir := t.TempDir()

	// 1. Create a Go file (known type)
	goFile := filepath.Join(tmpDir, "main.go")
	err := os.WriteFile(goFile, []byte(`package main
func Main() {}
`), 0o644)
	require.NoError(t, err)

	// 2. Create a Raw file (unknown type)
	txtFile := filepath.Join(tmpDir, "README.txt")
	txtContent := []byte("This is a readme")
	err = os.WriteFile(txtFile, txtContent, 0o644)
	require.NoError(t, err)

	// 3. Create a Raw file in subdir
	subDir := filepath.Join(tmpDir, "docs")
	require.NoError(t, os.Mkdir(subDir, 0o755))
	mdFile := filepath.Join(subDir, "manual.md")
	mdContent := []byte("# Manual")
	err = os.WriteFile(mdFile, mdContent, 0o644)
	require.NoError(t, err)

	// Sleep to ensure ModTime is distinct if needed, but os.Stat is enough
	// Touch files to ensure non-zero modtime? WriteFile sets it.

	store := graph.NewMemoryStore()
	engine := NewEngine(schema, store)
	require.NoError(t, engine.Ingest(tmpDir))

	// Verify Go file (Processed)
	_, err = store.GetNode("main/functions/Main/source")
	require.NoError(t, err, "Go file should be processed")

	// Verify README.txt (Raw) — routed to _project_files/ for tree-sitter schemas
	readmeNode, err := store.GetNode("_project_files/README.txt")
	require.NoError(t, err, "README.txt should be ingested under _project_files/")
	assert.Equal(t, txtContent, readmeNode.Data)
	assert.False(t, readmeNode.ModTime.IsZero(), "ModTime should be set")

	// Verify docs/manual.md (Raw in Subdir) — nested under _project_files/
	docsNode, err := store.GetNode("_project_files/docs")
	require.NoError(t, err, "_project_files/docs dir should be created")
	assert.True(t, docsNode.Mode.IsDir())
	assert.Contains(t, docsNode.Children, "_project_files/docs/manual.md")

	manualNode, err := store.GetNode("_project_files/docs/manual.md")
	require.NoError(t, err, "manual.md should be ingested under _project_files/")
	assert.Equal(t, mdContent, manualNode.Data)
}
