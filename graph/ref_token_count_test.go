package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/fixturedb"
)

// RefTokenCount is len(RefsMap()) without building the map (mache-a6c283), so
// it must drop exactly the rows RefsMap drops: a sentinel id, a token left
// with only sentinels, and a row naming no node, while keeping an occurrence
// in an injected language. RefsMap is the oracle, on every producer.
func TestRefTokenCount_IsTheLengthOfRefsMap(t *testing.T) {
	for _, p := range []fixturedb.Producer{fixturedb.Standalone, fixturedb.Leyline, fixturedb.LeylineV6} {
		t.Run(p.String(), func(t *testing.T) {
			b := fixturedb.New(t, p).
				Source("pkg/a.go", "go", "").
				Source("pkg/b.go", "go", "").
				Ref("Handler", "pkg/a.go/function_declaration", "", "").
				Ref("Handler", "pkg/b.go/function_declaration", "", "").
				Ref("Marshal", "pkg/b.go/function_declaration", "", "").
				InjectedRef("Embedded", "pkg/a.go/function_declaration", "code_span_0")
			if p != fixturedb.LeylineV6 {
				// Sentinels are written only by mache's engine, into path-keyed
				// dbs; LLO, the only producer of v6, writes none.
				b.Ref("Handler", FileLevelSentinelPrefix+"/abs/pkg/cmd.go", "", "").
					Ref("runE", FileLevelSentinelPrefix+"/abs/pkg/root.go", "", "")
			}
			path, f := b.
				Build()
			if p == fixturedb.LeylineV6 {
				// node_id is NOT NULL text on v4, so only v6 can dangle.
				fixturedb.AddDanglingV6Ref(t, f.DB(), "Dangling")
			}

			g, err := OpenSQLiteGraph(path, &api.Topology{}, nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = g.Close() })

			refs := g.RefsMap()
			require.Contains(t, refs, "Embedded", "an injected occurrence is a reference")
			require.NotContains(t, refs, "Dangling", "a row naming no node is not")
			require.NotContains(t, refs, "runE", "a token with only sentinel ids is not")
			require.Contains(t, refs, "Marshal")
			n, err := g.RefTokenCount()
			require.NoError(t, err)
			assert.Equal(t, len(refs), n)
		})
	}
}
