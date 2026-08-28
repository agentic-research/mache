package projcfg

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestEnvOr pins the override contract the daemon lifecycle and the hermetic
// launchd E2E both depend on: set means used, unset OR EMPTY means fallback —
// an empty exported var would silently produce ":7532"-less URLs.
func TestEnvOr(t *testing.T) {
	t.Setenv("MACHE_TEST_ENVOR", "custom")
	assert.Equal(t, "custom", EnvOr("MACHE_TEST_ENVOR", "fallback"))

	t.Setenv("MACHE_TEST_ENVOR", "")
	assert.Equal(t, "fallback", EnvOr("MACHE_TEST_ENVOR", "fallback"),
		"empty must mean fallback, not an empty endpoint")

	assert.Equal(t, "fallback", EnvOr("MACHE_TEST_ENVOR_UNSET", "fallback"))
}

// TestEndpointShape pins the derived URL: MacheHTTPURL is always the /mcp
// path on MacheHTTPListen — the one answer to "where does mache listen" that
// onboarding (this package) and the daemon lifecycle (cmd) both read.
func TestEndpointShape(t *testing.T) {
	assert.Equal(t, "http://"+MacheHTTPListen+"/mcp", MacheHTTPURL)
	assert.NotEmpty(t, MacheHTTPListen)
}
