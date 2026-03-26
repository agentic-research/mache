package ingest

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestEngine_ParentContext_JSONArrayFanout verifies that when a child selector
// fans out a nested array, each child match can access the parent's fields
// via the _parent key.
//
// JSON structure:
//
//	{ "name": "alpine", "version": "3.18", "packages": [{"pkg": "curl"}, {"pkg": "git"}] }
//
// Schema: root → {{.name}} → fan out packages[*] → {{._parent.name}}_{{.pkg}}
// Expected: alpine/alpine_curl, alpine/alpine_git
func TestEngine_ParentContext_JSONArrayFanout(t *testing.T) {
	schema := &api.Topology{
		Nodes: []api.Node{
			{
				Name:     "distros",
				Selector: "$",
				Children: []api.Node{
					{
						Name:     "{{.name}}",
						Selector: "$.distros[*]",
						Children: []api.Node{
							{
								Name:     "{{._parent.name}}_{{.pkg}}",
								Selector: "packages[*]",
								Files: []api.Leaf{
									{
										Name:            "info",
										ContentTemplate: "distro={{._parent.name}} version={{._parent.version}} pkg={{.pkg}}",
									},
								},
							},
						},
					},
				},
			},
		},
	}

	tmpDir := t.TempDir()
	dataFile := filepath.Join(tmpDir, "data.json")
	err := os.WriteFile(dataFile, []byte(`{
  "distros": [
    {
      "name": "alpine",
      "version": "3.18",
      "packages": [
        {"pkg": "curl"},
        {"pkg": "git"}
      ]
    },
    {
      "name": "debian",
      "version": "12",
      "packages": [
        {"pkg": "openssl"}
      ]
    }
  ]
}`), 0o644)
	require.NoError(t, err)

	store := graph.NewMemoryStore()
	engine := NewEngine(schema, store)

	err = engine.Ingest(dataFile)
	require.NoError(t, err)

	// alpine_curl should exist under alpine/
	node, err := store.GetNode("distros/alpine/alpine_curl/info")
	require.NoError(t, err, "alpine_curl/info should exist")
	assert.Equal(t, "distro=alpine version=3.18 pkg=curl", string(node.Data))

	// alpine_git should exist
	node, err = store.GetNode("distros/alpine/alpine_git/info")
	require.NoError(t, err, "alpine_git/info should exist")
	assert.Equal(t, "distro=alpine version=3.18 pkg=git", string(node.Data))

	// debian_openssl should exist under debian/
	node, err = store.GetNode("distros/debian/debian_openssl/info")
	require.NoError(t, err, "debian_openssl/info should exist")
	assert.Equal(t, "distro=debian version=12 pkg=openssl", string(node.Data))
}

// TestEngine_ParentContext_NestedTwoLevels verifies _parent chains: a
// grandchild can access _parent._parent.
func TestEngine_ParentContext_NestedTwoLevels(t *testing.T) {
	schema := &api.Topology{
		Nodes: []api.Node{
			{
				Name:     "root",
				Selector: "$",
				Children: []api.Node{
					{
						Name:     "{{.org}}",
						Selector: "$.orgs[*]",
						Children: []api.Node{
							{
								Name:     "{{.team}}",
								Selector: "teams[*]",
								Children: []api.Node{
									{
										Name:     "{{.member}}",
										Selector: "members[*]",
										Files: []api.Leaf{
											{
												Name:            "path",
												ContentTemplate: "{{._parent._parent.org}}/{{._parent.team}}/{{.member}}",
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	tmpDir := t.TempDir()
	dataFile := filepath.Join(tmpDir, "data.json")
	err := os.WriteFile(dataFile, []byte(`{
  "orgs": [
    {
      "org": "acme",
      "teams": [
        {
          "team": "eng",
          "members": [{"member": "alice"}, {"member": "bob"}]
        }
      ]
    }
  ]
}`), 0o644)
	require.NoError(t, err)

	store := graph.NewMemoryStore()
	engine := NewEngine(schema, store)

	err = engine.Ingest(dataFile)
	require.NoError(t, err)

	node, err := store.GetNode("root/acme/eng/alice/path")
	require.NoError(t, err, "alice/path should exist")
	assert.Equal(t, "acme/eng/alice", string(node.Data))

	node, err = store.GetNode("root/acme/eng/bob/path")
	require.NoError(t, err, "bob/path should exist")
	assert.Equal(t, "acme/eng/bob", string(node.Data))
}

// TestEngine_ParentContext_NoParentAtRoot verifies that top-level matches
// don't have a _parent key (no panic, no error).
func TestEngine_ParentContext_NoParentAtRoot(t *testing.T) {
	schema := &api.Topology{
		Nodes: []api.Node{
			{
				Name:     "items",
				Selector: "$",
				Children: []api.Node{
					{
						Name:     "{{.name}}",
						Selector: "$.items[*]",
						Files: []api.Leaf{
							{
								Name:            "value",
								ContentTemplate: "{{.name}}",
							},
						},
					},
				},
			},
		},
	}

	tmpDir := t.TempDir()
	dataFile := filepath.Join(tmpDir, "data.json")
	err := os.WriteFile(dataFile, []byte(`{"items": [{"name": "foo"}]}`), 0o644)
	require.NoError(t, err)

	store := graph.NewMemoryStore()
	engine := NewEngine(schema, store)

	err = engine.Ingest(dataFile)
	require.NoError(t, err)

	node, err := store.GetNode("items/foo/value")
	require.NoError(t, err)
	assert.Equal(t, "foo", string(node.Data))
}

// TestEngine_ParentContext_SQLiteStreamingPath exercises the collectNodes path
// (used for .db ingestion) to verify _parent works in the SQLite streaming
// pipeline, not just the MemoryStore/JSON file path.
func TestEngine_ParentContext_SQLiteStreamingPath(t *testing.T) {
	schema := &api.Topology{
		Version: "v1",
		Nodes: []api.Node{
			{
				Name:     "vulns",
				Selector: "$",
				Children: []api.Node{
					{
						Name:     "{{.item.name}}",
						Selector: "$[*]",
						Children: []api.Node{
							{
								Name:     "{{.pkg}}",
								Selector: "$.item.affected[*]",
								Files: []api.Leaf{
									{
										Name:            "detail",
										ContentTemplate: "vuln={{._parent.item.name}} pkg={{.pkg}}",
									},
								},
							},
						},
					},
				},
			},
		},
	}

	// Build a test SQLite DB with records containing nested arrays.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = db.Exec("CREATE TABLE results (id TEXT PRIMARY KEY, record TEXT NOT NULL)")
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO results VALUES ('a', '{"schema":"vuln","identifier":"CVE-1","item":{"name":"CVE-2024-001","affected":[{"pkg":"curl"},{"pkg":"wget"}]}}')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	store := graph.NewMemoryStore()
	engine := NewEngine(schema, store)

	err = engine.Ingest(dbPath)
	require.NoError(t, err)

	// Verify _parent worked in the collectNodes path
	node, err := store.GetNode("vulns/CVE-2024-001/curl/detail")
	require.NoError(t, err, "curl/detail should exist via SQLite streaming path")
	assert.Equal(t, "vuln=CVE-2024-001 pkg=curl", string(node.Data))

	node, err = store.GetNode("vulns/CVE-2024-001/wget/detail")
	require.NoError(t, err, "wget/detail should exist via SQLite streaming path")
	assert.Equal(t, "vuln=CVE-2024-001 pkg=wget", string(node.Data))
}
