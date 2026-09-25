package smells

import (
	"testing"

	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every built-in rule's Requires must RESOLVE against a producer-shaped
// fixture (mache-be17ce).
//
// This exists because the failure it catches is SILENT. Since the rules moved
// onto v_ast / v_nodes / v_defs / v_refs, Requires names TEMP views — and on
// the `--rule '*'` path a rule whose Requires does not resolve is SKIPPED, not
// failed. A name that goes stale therefore turns the rule off while the gate
// still reports success and the ratchet still finds no NEW findings.
//
// SCOPE, stated because the first version of this test overclaimed: it does
// NOT prove the install ORDERING, because fixturedb installs the views for
// every fixture by construction (see the RegisterViewInstaller in
// fixturedb_test.go), so a fixture with them missing cannot be built here.
// Removing the install call below changes nothing, which is how the
// overclaim was caught. The ordering bug — the MCP handler checking Requires
// before installing the views — is held by
// TestFindSmells_MCPAndCLI_ByteForByteParity, which did catch it.
//
// What this holds is the stale-name half, verified by pointing a rule's
// Requires at something that does not exist and watching it fail.
func TestBuiltinRules_RequiresAllResolve(t *testing.T) {
	rules := mustLoadEmbeddedRules()
	require.NotEmpty(t, rules, "no built-in rules — the guard would pass vacuously")

	g := newSmellFixture(t, fixturedb.Leyline, func(b *fixturedb.Builder) {
		b.Def("Alpha", "pkg/a.go/function_declaration_0", fixturedb.Function)
	})

	for i := range rules {
		rule := &rules[i]
		t.Run(rule.ID, func(t *testing.T) {
			require.NotEmpty(t, rule.Requires,
				"a rule with no Requires can never be skipped, but also declares nothing")
			missing, err := missingTables(g, rule.Requires)
			require.NoError(t, err)
			assert.Empty(t, missing,
				"rule %q requires %v, and %v do not exist on a leyline-shaped fixture with the "+
					"canonical views installed. On `--rule '*'` this SKIPS the rule silently rather "+
					"than failing it, so the gate would keep reporting success while enforcing nothing.",
				rule.ID, rule.Requires, missing)
		})
	}
}
