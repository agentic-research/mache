package mcpserve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-research/mache/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCIRunsEveryE2EBackend pins the fix for mache-f06138 where it can
// actually rot: the large-tier CI step now runs ONE go test invocation per
// backend, so each gets a -timeout budget sized to its own work rather than
// to the sum of both.
//
// Two things can silently break that. A backend added to allE2EBackends()
// would run locally and never run in CI — the gate would look green while
// covering less. And collapsing the loop back into a single invocation would
// restore the budget-versus-sum mismatch that made a 22m53s run fail against
// a 25m limit on a re-run of an already-passing commit.
//
// Reading the workflow is the only way to check either: the coupling between
// the Go backend list and the CI invocation is real but invisible to the
// compiler.
func TestCIRunsEveryE2EBackend(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(testutil.MacheRepoRoot(t),
		".github", "workflows", "integration.yml"))
	require.NoError(t, err)
	wf := string(raw)

	sentinel := "TestE2E_MacheOnMache"
	require.Contains(t, wf, sentinel, "the sentinel step must exist")

	// The invocation must select a subtest, not run the whole test — that is
	// what splits the budget per backend.
	assert.Contains(t, wf, sentinel+`\$/$backend`,
		"the sentinel must run one invocation per backend; a bare -run '^"+sentinel+"$' "+
			"puts both backends under one timeout, which is the defect this closed")

	// Every backend the code declares must appear in the CI loop.
	loop := wf[strings.Index(wf, "for backend in"):]
	loop = loop[:strings.Index(loop, "\n")]
	for _, b := range allE2EBackends() {
		assert.Containsf(t, loop, b.name,
			"backend %q runs locally but is missing from the CI loop %q — "+
				"the gate would be green while covering less than it appears to", b.name, loop)
	}
}
