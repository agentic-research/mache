package mcpserve

import (
	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/agentic-research/mache/internal/smells"
)

// This test binary builds fixturedb fixtures (node_token_test.go,
// find_smells_parity_test.go), so it registers the canonical-view installer —
// B11's fourth leg. Legal direction: smells does not import mcpserve.
func init() {
	fixturedb.RegisterViewInstaller(func(qg fixturedb.RefsQuerier) error {
		return smells.EnsureCanonicalViews(qg)
	})
}
