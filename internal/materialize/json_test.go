package materialize

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJSONMaterializer_Interface(t *testing.T) {
	var _ Materializer = (*JSONMaterializer)(nil)
}

func TestJSONMaterializer_CreatesValidJSON(t *testing.T) {
	srcDB := setupTestDB(t)
	outPath := filepath.Join(t.TempDir(), "out.json")

	m := &JSONMaterializer{}
	err := m.Materialize(srcDB, outPath)
	require.NoError(t, err)

	// Should be valid JSON.
	data, err := os.ReadFile(outPath)
	require.NoError(t, err)
	assert.True(t, json.Valid(data), "output should be valid JSON")
}

func TestJSONMaterializer_TreeStructure(t *testing.T) {
	srcDB := setupTestDB(t)
	outPath := filepath.Join(t.TempDir(), "out.json")

	m := &JSONMaterializer{}
	err := m.Materialize(srcDB, outPath)
	require.NoError(t, err)

	data, err := os.ReadFile(outPath)
	require.NoError(t, err)

	var entries []jsonEntry
	require.NoError(t, json.Unmarshal(data, &entries))

	// Root should have: _schema.json (file) + functions (directory).
	require.Len(t, entries, 2)

	// Find the functions directory and _schema.json file by name.
	var functionsDir, schemaFile *jsonEntry
	for i := range entries {
		switch entries[i].Name {
		case "functions":
			functionsDir = &entries[i]
		case "_schema.json":
			schemaFile = &entries[i]
		}
	}

	// Verify _schema.json is a file with content.
	require.NotNil(t, schemaFile, "_schema.json should exist")
	assert.Equal(t, "file", schemaFile.Type)
	require.NotNil(t, schemaFile.Content)
	assert.Equal(t, `{"version":"v1"}`, *schemaFile.Content)

	// Verify functions is a directory with 2 children.
	require.NotNil(t, functionsDir, "functions dir should exist")
	assert.Equal(t, "directory", functionsDir.Type)
	require.Len(t, functionsDir.Children, 2)
}

func TestJSONMaterializer_FileContent(t *testing.T) {
	srcDB := setupTestDB(t)
	outPath := filepath.Join(t.TempDir(), "out.json")

	m := &JSONMaterializer{}
	err := m.Materialize(srcDB, outPath)
	require.NoError(t, err)

	data, err := os.ReadFile(outPath)
	require.NoError(t, err)

	var entries []jsonEntry
	require.NoError(t, json.Unmarshal(data, &entries))

	// Navigate to functions/HandleRequest/source.
	var functionsDir *jsonEntry
	for i := range entries {
		if entries[i].Name == "functions" {
			functionsDir = &entries[i]
			break
		}
	}
	require.NotNil(t, functionsDir)

	var handleReq *jsonEntry
	for i := range functionsDir.Children {
		if functionsDir.Children[i].Name == "HandleRequest" {
			handleReq = functionsDir.Children[i]
			break
		}
	}
	require.NotNil(t, handleReq)
	require.Len(t, handleReq.Children, 1)

	source := handleReq.Children[0]
	assert.Equal(t, "source", source.Name)
	assert.Equal(t, "file", source.Type)
	require.NotNil(t, source.Content)
	assert.Equal(t, "func HandleRequest() {}", *source.Content)
	require.NotNil(t, source.Size)
	assert.Equal(t, int64(25), *source.Size)
}

func TestJSONMaterializer_DirectoryHasNoContentOrSize(t *testing.T) {
	srcDB := setupTestDB(t)
	outPath := filepath.Join(t.TempDir(), "out.json")

	m := &JSONMaterializer{}
	err := m.Materialize(srcDB, outPath)
	require.NoError(t, err)

	data, err := os.ReadFile(outPath)
	require.NoError(t, err)

	var entries []jsonEntry
	require.NoError(t, json.Unmarshal(data, &entries))

	for i := range entries {
		if entries[i].Name == "functions" {
			assert.Nil(t, entries[i].Size, "directory should not have size")
			assert.Nil(t, entries[i].Content, "directory should not have content")
			return
		}
	}
	t.Fatal("functions directory not found")
}

func TestJSONMaterializer_EmptyRootNode(t *testing.T) {
	// Same transparent-root pattern as BoltDB: empty-named root dir
	// should not appear — its children are promoted to the top level.
	srcDB := setupEmptyRootTestDB(t)
	outPath := filepath.Join(t.TempDir(), "out.json")

	m := &JSONMaterializer{}
	err := m.Materialize(srcDB, outPath)
	require.NoError(t, err)

	data, err := os.ReadFile(outPath)
	require.NoError(t, err)

	var entries []jsonEntry
	require.NoError(t, json.Unmarshal(data, &entries))

	// Should have "alpine 3.18" (dir) and "_schema.json" (file) at top level,
	// NOT a wrapper with name "".
	names := make(map[string]string)
	for _, e := range entries {
		names[e.Name] = e.Type
	}
	assert.Equal(t, "directory", names["alpine 3.18"])
	assert.Equal(t, "file", names["_schema.json"])
	_, hasEmpty := names[""]
	assert.False(t, hasEmpty, "empty-named root should be transparent")
}

func TestJSONMaterializer_InvalidSource(t *testing.T) {
	m := &JSONMaterializer{}
	err := m.Materialize("/nonexistent/path.db", filepath.Join(t.TempDir(), "out.json"))
	assert.Error(t, err)
}

func TestForFormat_JSON(t *testing.T) {
	m, err := ForFormat("json")
	require.NoError(t, err)
	assert.IsType(t, &JSONMaterializer{}, m)
}
