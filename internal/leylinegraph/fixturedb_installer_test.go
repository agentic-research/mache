package leylinegraph

import (
	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/agentic-research/mache/internal/smells"
)

// This test binary builds fixturedb fixtures (leyline_derived_parent_test.go),
// so it must register the canonical-view installer — B11's third leg. Legal
// direction: smells does not import leylinegraph, which is why smells shipped
// in stage 4 before this package existed.
func init() {
	fixturedb.RegisterViewInstaller(func(qg fixturedb.RefsQuerier) error {
		return smells.EnsureCanonicalViews(qg)
	})
}
