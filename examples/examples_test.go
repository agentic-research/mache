package examples_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSchemasParse(t *testing.T) {
	schemas, err := filepath.Glob("*-schema.json")
	require.NoError(t, err)
	require.NotEmpty(t, schemas, "no schema files found")

	for _, sf := range schemas {
		t.Run(sf, func(t *testing.T) {
			data, err := os.ReadFile(sf)
			require.NoError(t, err)

			var schema api.Topology
			require.NoError(t, json.Unmarshal(data, &schema), "schema should parse as valid Topology")
			assert.Equal(t, "v1", schema.Version)
			assert.NotEmpty(t, schema.Nodes, "schema should have at least one node")
		})
	}
}

func TestMCPSchemaIngest(t *testing.T) {
	schemaBytes, err := os.ReadFile("mcp-schema.json")
	require.NoError(t, err)

	var schema api.Topology
	require.NoError(t, json.Unmarshal(schemaBytes, &schema))

	store := graph.NewMemoryStore()
	engine := ingest.NewEngine(&schema, store)

	absPath, err := filepath.Abs("mcp-sample-manifest.json")
	require.NoError(t, err)

	err = engine.Ingest(absPath)
	require.NoError(t, err, "MCP manifest ingestion failed")

	expectedNodes := []string{
		"tools",
		"tools/search-issues",
		"tools/create-issue",
		"tools/read-file",
		"resources",
		"resources/repository-readme",
		"resources/issue-detail",
		"prompts",
		"prompts/summarize-repo",
	}
	for _, path := range expectedNodes {
		_, err := store.GetNode(path)
		assert.NoError(t, err, "node %s not found", path)
	}

	// Verify tool leaf files exist
	toolNode, err := store.GetNode("tools/search-issues")
	require.NoError(t, err)
	assert.Contains(t, toolNode.Children, "tools/search-issues/description")
	assert.Contains(t, toolNode.Children, "tools/search-issues/input-schema.json")
	assert.Contains(t, toolNode.Children, "tools/search-issues/raw.json")

	// Verify input-schema.json content is valid JSON
	buf := make([]byte, 8192)
	n, err := store.ReadContent("tools/search-issues/input-schema.json", buf, 0)
	require.NoError(t, err)
	var inputSchema map[string]any
	require.NoError(t, json.Unmarshal(buf[:n], &inputSchema), "input-schema.json should be valid JSON")
	assert.Equal(t, "object", inputSchema["type"])

	// Verify resource leaf files
	resNode, err := store.GetNode("resources/repository-readme")
	require.NoError(t, err)
	assert.Contains(t, resNode.Children, "resources/repository-readme/uri")
}

func TestTreeSitterExamples(t *testing.T) {
	tests := []struct {
		name          string
		schemaFile    string
		sampleFile    string
		expectedNodes []string
	}{
		{
			name:       "Go",
			schemaFile: "go-schema.json",
			sampleFile: "testdata/go_sample.go",
			expectedNodes: []string{
				"main",
				"main/functions",
				"main/functions/Main",
				"main/functions/Main/source",
				"main/functions/Helper",
			},
		},
		{
			name:       "Python",
			schemaFile: "python-schema.json",
			sampleFile: "testdata/python_sample.py",
			expectedNodes: []string{
				"imports",
				"imports/os",
				"classes",
				"classes/MyClass",
				"classes/MyClass/source",
				"functions",
				"functions/hello",
			},
		},
		{
			name:       "SQL",
			schemaFile: "sql-schema.json",
			sampleFile: "testdata/sql_sample.sql",
			expectedNodes: []string{
				"tables",
				"tables/users",
				"views",
				"views/user_names",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			schemaBytes, err := os.ReadFile(tc.schemaFile)
			require.NoError(t, err)

			var schema api.Topology
			require.NoError(t, json.Unmarshal(schemaBytes, &schema))

			store := graph.NewMemoryStore()
			engine := ingest.NewEngine(&schema, store)

			absSamplePath, err := filepath.Abs(tc.sampleFile)
			require.NoError(t, err)

			err = engine.Ingest(absSamplePath)
			require.NoError(t, err, "ingestion failed")

			for _, path := range tc.expectedNodes {
				_, err := store.GetNode(path)
				assert.NoError(t, err, "node %s not found", path)
			}
		})
	}
}
