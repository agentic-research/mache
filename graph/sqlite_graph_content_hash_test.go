package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/agentic-research/mache/internal/lloschema"
)

// contentSpec is a ley-line projection with three constructs, three reference
// sites, one of them in an injected language (no _ast or nodes row, only its
// node_refs row carries a node_hash), built as projection-v4 or v6.
func contentSpec(t *testing.T, p fixturedb.Producer) *SQLiteGraph {
	t.Helper()
	b := fixturedb.New(t, p)
	b.Source("main.go", "go", "")
	b.Source("pkg/a.go", "go", "")
	b.ASTNode("main.go/function_declaration", "function_declaration", "main.go",
		fixturedb.Span{StartByte: 42, EndByte: 82, StartRow: 4, StartCol: 0, EndRow: 6, EndCol: 1})
	b.ASTNode("pkg/a.go/function_declaration_0", "function_declaration", "pkg/a.go",
		fixturedb.Span{StartByte: 40, EndByte: 110, StartRow: 3, StartCol: 0, EndRow: 8, EndCol: 1})
	b.ASTNode("pkg/a.go/function_declaration_1", "function_declaration", "pkg/a.go",
		fixturedb.Span{StartByte: 112, EndByte: 142, StartRow: 10, StartCol: 0, EndRow: 12, EndCol: 1})
	b.Def("Greet", "pkg/a.go/function_declaration_0", fixturedb.Function)
	b.Ref("Greet", "main.go/function_declaration", "main.go/function_declaration/call_expression", "pkg")
	b.Ref("Greet", "pkg/a.go/function_declaration_0", "pkg/a.go/function_declaration_0/call_expression", "")
	b.InjectedRef("Greet", "main.go/function_declaration", "code_span/identifier")
	path, _ := b.Build()
	g, err := OpenSQLiteGraph(path, &api.Topology{}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })
	return g
}

// TestSQLiteGraph_NodeHashes_EveryReferenceSiteHasOne: the ids community
// detection is built from (RefsMap's sites) each resolve to a 32-byte hash on
// both projections, the injected site through node_refs since it has no _ast
// row, and a directory, which has no content, resolves to none.
func TestSQLiteGraph_NodeHashes_EveryReferenceSiteHasOne(t *testing.T) {
	for _, p := range []fixturedb.Producer{fixturedb.Leyline, fixturedb.LeylineV6} {
		t.Run(p.String(), func(t *testing.T) {
			g := contentSpec(t, p)
			var sites []string
			for _, ids := range g.RefsMap() {
				sites = append(sites, ids...)
			}
			require.Len(t, sites, 3, "the three declared sites: %v", sites)

			ids := append([]string{"pkg", "main.go/function_declaration", "no/such/node"}, sites...)
			hashes, err := g.NodeHashes(ids)
			require.NoError(t, err)
			for _, s := range sites {
				assert.Contains(t, hashes, s, "a reference site has a node_hash")
			}
			assert.Contains(t, hashes, "main.go/function_declaration", "a construct with an _ast row has one")
			assert.NotContains(t, hashes, "pkg", "a directory has no content to hash")
			assert.NotContains(t, hashes, "no/such/node")
			assert.NotEqual(t, hashes[sites[0]], [32]byte{}, "a hash is bytes, not the zero value")

			distinct := map[[32]byte]bool{}
			for _, h := range hashes {
				distinct[h] = true
			}
			assert.Len(t, distinct, len(hashes), "the fixture gives every occurrence its own subtree")
		})
	}
}

// TestSQLiteGraph_NodeHashes_ReadsWhatTheProducerWrote compares one hash
// with the row behind it, so the lookup reads the hash and not another column.
func TestSQLiteGraph_NodeHashes_ReadsWhatTheProducerWrote(t *testing.T) {
	g := contentSpec(t, fixturedb.LeylineV6)
	const id = "main.go/function_declaration"
	hashes, err := g.NodeHashes([]string{id})
	require.NoError(t, err)
	got, ok := hashes[id]
	require.True(t, ok)

	nid, ok, err := g.key(id)
	require.NoError(t, err)
	require.True(t, ok)
	expr, has, err := lloschema.NodeHashExpr(g, "_ast", "a")
	require.NoError(t, err)
	require.True(t, has)
	var want []byte
	require.NoError(t, g.db.QueryRow("SELECT "+expr+" FROM _ast a WHERE a.nid = ?", nid).Scan(&want))
	assert.Equal(t, want, got[:])
}

// TestSQLiteGraph_ContentHasher_AbsentOnMachesOwnProjection: a standalone db
// records no parserId and no node_hash, and says so rather than inventing one.
func TestSQLiteGraph_ContentHasher_AbsentOnMachesOwnProjection(t *testing.T) {
	path, _ := fixturedb.New(t, fixturedb.Standalone).
		Source("main.go", "go", "package main").
		Construct("main.go/function_declaration").
		Ref("Greet", "main.go/function_declaration", "main.go/function_declaration/call_expression", "").
		Build()
	g, err := OpenSQLiteGraph(path, &api.Topology{}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })

	_, err = g.ParserID()
	require.ErrorIs(t, err, lloschema.ErrNoParserID)

	hashes, err := g.NodeHashes([]string{"main.go/function_declaration", "main.go/function_declaration/call_expression"})
	require.NoError(t, err)
	assert.Empty(t, hashes)
}
