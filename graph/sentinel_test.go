package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/fixturedb"
	_ "modernc.org/sqlite"
)

// TestRefsMap_StripsSentinelsOnEveryBackend pins the contract that made this a
// bug rather than a wart: RefsMap is a CONSUMER-facing aggregation feeding
// community detection, architecture layering and context packing, and it used
// to return different data depending on which producer wrote the .db —
// leylinegraph filtered the sentinels in SQL while SQLiteGraph and MemoryStore
// did not.
//
// Measured on a schema-projected build of mache's cmd/, leaving them in
// inflated `mache pack`'s headline count from 629 real reference tokens to
// 1,026, i.e. 39% of what it reported was bookkeeping, and pushed real module
// dependencies out of the top-refs list in favour of sentinel-backed tokens.
func TestRefsMap_StripsSentinelsOnEveryBackend(t *testing.T) {
	want := map[string][]string{
		"Handler": {"pkg/functions/Serve"},
		"Marshal": {"pkg/functions/Encode"},
	}

	t.Run("SQLiteGraph", func(t *testing.T) {
		// Standalone is the producer that writes node_refs.node_id = the
		// ENCLOSING construct, which is where the engine puts the sentinel.
		// Named rather than hand-written, because the DDL a fixture happens to
		// type decides which canonical view the code under test runs against
		// (mache-7555da).
		path, _ := fixturedb.New(t, fixturedb.Standalone).
			Ref("Handler", "pkg/functions/Serve", "", "").
			Ref("Handler", FileLevelSentinelPrefix+"/abs/path/pkg/cmd.go", "", "").
			Ref("runE", FileLevelSentinelPrefix+"/abs/path/pkg/root.go", "", "").
			Ref("Marshal", "pkg/functions/Encode", "", "").
			Build()

		g, err := OpenSQLiteGraph(path, &api.Topology{}, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = g.Close() })
		assert.Equal(t, want, g.RefsMap())
	})

	t.Run("MemoryStore", func(t *testing.T) {
		s := NewMemoryStore()
		require.NoError(t, s.AddRef("Handler", "pkg/functions/Serve"))
		require.NoError(t, s.AddRef("Handler", "_file_level:/abs/path/pkg/cmd.go"))
		require.NoError(t, s.AddRef("runE", "_file_level:/abs/path/pkg/root.go"))
		require.NoError(t, s.AddRef("Marshal", "pkg/functions/Encode"))
		assert.Equal(t, want, s.RefsMap())

		// Memoized: the second call must return the same filtered snapshot,
		// not an unfiltered rebuild.
		assert.Equal(t, want, s.RefsMap())
	})
}

// TestFilterSentinelRefs_DropsTokensLeftEmpty pins the part that is easy to get
// half-right. Removing sentinel ids but keeping the token would leave a
// real-looking node with no edges, and callers count len(refs) as "reference
// tokens" — which is precisely the number that was 39% wrong.
func TestFilterSentinelRefs_DropsTokensLeftEmpty(t *testing.T) {
	in := map[string][]string{
		"mixed":        {"pkg/functions/A", FileLevelSentinelPrefix + "/abs/f.go"},
		"onlySentinel": {FileLevelSentinelPrefix + "/abs/g.go"},
		"clean":        {"pkg/functions/B"},
	}
	got := filterSentinelRefs(in)

	assert.Equal(t, map[string][]string{
		"mixed": {"pkg/functions/A"},
		"clean": {"pkg/functions/B"},
	}, got)
	assert.NotContains(t, got, "onlySentinel",
		"a token whose only refs were bookkeeping is not a reference token")

	assert.Len(t, in["mixed"], 2, "the input must not be mutated — MemoryStore hands out a shared snapshot")
}

// TestIsFileLevelSentinel_MatchesOnlyThePrefix guards against a filter that is
// too eager: a construct legitimately named with a leading underscore, or a
// file whose path merely contains the marker, is content.
func TestIsFileLevelSentinel_MatchesOnlyThePrefix(t *testing.T) {
	assert.True(t, IsFileLevelSentinel("_file_level:/abs/path/main.go"))
	assert.False(t, IsFileLevelSentinel("pkg/functions/_file_level_helper"))
	assert.False(t, IsFileLevelSentinel("pkg/_file_level:/nested"),
		"the marker is a PREFIX, not a substring — a construct is not bookkeeping")
	assert.False(t, IsFileLevelSentinel(""))
}
