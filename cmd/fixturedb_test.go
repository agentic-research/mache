package cmd

import (
	"testing"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/agentic-research/mache/internal/testutil"
)

// Tests in this package build their .db fixtures through internal/fixturedb,
// which owns the producer DDL and forbids a test from stating a schema. See
// that package's doc comment for why (mache-7555da).
//
// The canonical views are installed by fixturedb itself, from the installer
// registered here — so no test can build a fixture and forget them, which is the
// failure the "26 tests that build fixtures by hand" note in smell_refs_views.go
// describes.
func init() {
	fixturedb.RegisterViewInstaller(func(qg fixturedb.RefsQuerier) error {
		return ensureCanonicalViews(qg)
	})
}

// newSmellFixture builds a producer-shaped fixture and wraps it as the graph
// backend the find_smells handlers take.
//
// p is not defaulted and never inferred: every fixture must SAY which producer
// it models, because ensureCanonicalViews emits a structurally different v_refs
// per producer and the choice decides which SQL the rule under test runs.
func newSmellFixture(t *testing.T, p fixturedb.Producer, seed func(*fixturedb.Builder)) *testutil.SmellTestGraph {
	t.Helper()
	b := fixturedb.New(t, p)
	seed(b)
	path, f := b.Build()
	return &testutil.SmellTestGraph{MemoryStore: graph.NewMemoryStore(), DB: f.DB(), Path: path}
}
