package ingest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEngine_IngestDataTree_SharesProjectSkipRules pins that a data-schema
// directory ingest walks through the same walkProjectFiles as the source-tree
// ingest: hidden directories, .gitignore matches and binary files are skipped
// by ONE set of rules, parsed extensions are projected and everything else
// lands as a raw file. Until mache-95a33d the data walk was a second copy of
// those rules, and it was untested.
func TestEngine_IngestDataTree_SharesProjectSkipRules(t *testing.T) {
	schema := &api.Topology{
		Nodes: []api.Node{{
			Name:     "users",
			Selector: "$",
			Children: []api.Node{{
				Name:     "{{.name}}",
				Selector: "users[*]",
				Files:    []api.Leaf{{Name: "role", ContentTemplate: "{{.role}}"}},
			}},
		}},
	}
	root := t.TempDir()
	write := func(rel string, data []byte) {
		p := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, data, 0o644))
	}
	write("data.json", []byte(`{"users":[{"name":"Alice","role":"admin"}]}`))
	write("notes/readme.txt", []byte("plain text lands as a raw file\n"))
	write(".hidden/trap.json", []byte(`{"users":[{"name":"Trap","role":"x"}]}`))
	write("ignored.json", []byte(`{"users":[{"name":"Ignored","role":"x"}]}`))
	write(".gitignore", []byte("ignored.json\n"))
	write("blob.dat", []byte("\x00\x01\x02binary"))

	store := graph.NewMemoryStore()
	engine := NewEngine(schema, store)
	require.NoError(t, engine.Ingest(root))

	node, err := store.GetNode("users/Alice/role")
	require.NoError(t, err, "data.json is parsed by the schema")
	assert.Equal(t, "admin", string(node.Data))

	_, err = store.GetNode("notes/readme.txt")
	require.NoError(t, err, "a non-parsed, non-binary file lands as a raw file")

	for _, absent := range []string{"users/Trap", "users/Ignored", "blob.dat"} {
		_, err = store.GetNode(absent)
		assert.ErrorIs(t, err, graph.ErrNotFound, "%s must be skipped", absent)
	}
}
