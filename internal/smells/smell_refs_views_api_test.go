package smells

import (
	"testing"

	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestEnsureCanonicalViews_ExportedEntry pins the newly exported surface
// (stage 4): fixturedb registers this as its view installer (see
// fixturedb_test.go's init), so a fixture Build exercises the exported entry
// end to end — v_refs must exist and answer, and a second install over the
// same fixture must be idempotent, because the installer runs once per
// Build and manual re-runs must not error.
func TestEnsureCanonicalViews_ExportedEntry(t *testing.T) {
	_, f := fixturedb.New(t, fixturedb.Standalone).
		Construct("pkg/Only", fixturedb.Where{Source: "only.go"}).
		Def("Only", "pkg/Only", "").
		Build()

	var n int
	require.NoError(t, f.DB().QueryRow(`SELECT COUNT(*) FROM v_defs`).Scan(&n),
		"the registered installer must have created the canonical views")
	assert.Equal(t, 1, n)

	require.NoError(t, EnsureCanonicalViews(&sqlDBQuerier{db: f.DB()}),
		"re-install over an already-installed fixture must be idempotent")
}
