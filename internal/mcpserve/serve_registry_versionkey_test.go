package mcpserve

import (
	"strings"
	"testing"

	"github.com/agentic-research/mache/internal/leyline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGraphCacheKey_BindsFullProvenance pins mache-6c9e1d's core claim: a
// graph's freshness is a TRIPLE — (source state, producer version, parser pin)
// — and the registry key must carry all three.
//
// It carried only the first, and the gap was MASKED: upgrades restart the
// daemon, and the restart flushes the in-process registry as a side effect.
// That accident is load-bearing — mache-956488 (decoupling sessions from
// daemon restarts) removes it, so this binding is that work's dependency-
// blocker, recorded as a `blocks` edge. Without it, a decoupled daemon serves
// a graph built by an older mache indefinitely, with nothing to notice.
//
// Asserted on the KEY rather than through a full registry round-trip: the
// registry's load/evict machinery is exercised elsewhere; what this must pin
// is that the key is a pure function of all three provenance inputs, so a
// change to ANY of them misses the cache.
func TestGraphCacheKey_BindsFullProvenance(t *testing.T) {
	// The version term is INJECTED (SetVersion from cmd/register.go, B23):
	// asserting against cmd.Version from here would be an import cycle, so
	// the test injects a sentinel and asserts the key carries THAT — proving
	// the key is a pure function of whatever the wiring supplies.
	prev := producerVersion
	t.Cleanup(func() { producerVersion = prev })
	SetVersion("9.9.9-versionkey-test")

	r := &graphRegistry{}
	root := t.TempDir() // no git repo: exercises the no-commit key shape too

	lg := r.getOrCreateGraph(root)
	require.NotNil(t, lg)

	found := ""
	r.graphs.Range(func(k, _ any) bool {
		if strings.HasPrefix(k.(string), root) {
			found = k.(string)
		}
		return true
	})
	require.NotEmpty(t, found, "the graph must be registered under its root")

	assert.Contains(t, found, "9.9.9-versionkey-test",
		"the key must carry the PRODUCER version — buildcache already stamps "+
			"ProducerVersion() into the OCI cache for exactly this reason, and "+
			"the serve path must not be the one surface that forgets who built it")
	assert.Contains(t, found, leyline.BinaryVersion,
		"and the PARSER pin — LLO ships _ast schema changes in patch releases, "+
			"so the same source under a different pin is a different graph")
}
