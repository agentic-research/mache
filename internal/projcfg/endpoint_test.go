package projcfg

import (
	"testing"
	"time"

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

// TestEnvDurationOr pins the parse-or-fallback contract shared by every
// daemon-lifecycle tunable (settle, drain, breaker window). Junk must fall
// back rather than silently yielding a zero timeout, which would turn a
// bounded wait into an instant one.
func TestEnvDurationOr(t *testing.T) {
	const key = "MACHE_TEST_DURATION"
	fallback := 90 * time.Second

	t.Setenv(key, "45s")
	assert.Equal(t, 45*time.Second, EnvDurationOr(key, fallback))

	for _, bad := range []string{"", "not-a-duration", "0s", "-5s"} {
		t.Setenv(key, bad)
		assert.Equalf(t, fallback, EnvDurationOr(key, fallback),
			"%q must fall back, never produce a zero or negative wait", bad)
	}

	assert.Equal(t, fallback, EnvDurationOr("MACHE_TEST_DURATION_UNSET", fallback))
}

// TestEnvIntOr pins the same contract for counts (the crash-loop burst): a
// zero or negative burst would mean "trip immediately", turning the safety
// valve into a daemon that can never start.
func TestEnvIntOr(t *testing.T) {
	const key = "MACHE_TEST_INT"

	t.Setenv(key, "3")
	assert.Equal(t, 3, EnvIntOr(key, 5))

	for _, bad := range []string{"", "abc", "0", "-1", "1.5"} {
		t.Setenv(key, bad)
		assert.Equalf(t, 5, EnvIntOr(key, 5),
			"%q must fall back — a zero burst would trip the breaker on the first start", bad)
	}

	assert.Equal(t, 5, EnvIntOr("MACHE_TEST_INT_UNSET", 5))
}
