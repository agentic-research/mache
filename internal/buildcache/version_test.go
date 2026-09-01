package buildcache

import (
	"testing"

	"github.com/agentic-research/mache/internal/buildinfo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProducerVersionInjection pins the pull-based identity contract (B3/B24):
// default is the committed release base, a set overrides it, and an empty set
// is REFUSED — the failure mode this design exists to prevent is a silent ""
// stamped into every lockfile when ldflags resolution goes wrong.
func TestProducerVersionInjection(t *testing.T) {
	orig := producerVersion
	t.Cleanup(func() { producerVersion = orig })

	producerVersion = buildinfo.Version
	assert.Equal(t, buildinfo.Version, ProducerVersion(), "default is the release base")

	SetProducerVersion("9.9.9-test")
	assert.Equal(t, "9.9.9-test", ProducerVersion(), "an injected identity wins")

	SetProducerVersion("")
	assert.Equal(t, "9.9.9-test", ProducerVersion(),
		"empty injection must be refused, never stamped")
}

// TestCacheCmd_ReturnsTheRegisteredCommand pins registration hook #2: what
// cmd/register.go wires must be the live command group with its flag set.
func TestCacheCmd_ReturnsTheRegisteredCommand(t *testing.T) {
	c := CacheCmd()
	require.NotNil(t, c)
	assert.Equal(t, "cache", c.Use)
	assert.Same(t, cacheCmd, c, "must be the live command, not a clone")
	require.NotEmpty(t, c.Commands(), "push/pull/verify subcommands must ride along")
}
