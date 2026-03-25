package ingest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
