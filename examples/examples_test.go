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
