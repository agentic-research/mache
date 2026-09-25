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
// TestEnvBytesOr covers what makes it a separate parser from EnvIntOr: a size
// budget is int64 and ZERO is a legitimate value, where EnvIntOr's
// positive-only rule would reject it.
func TestEnvBytesOr(t *testing.T) {
	const key = "MACHE_TEST_BYTES"
	const fallback int64 = 10 << 30

	t.Setenv(key, "4096")
	assert.Equal(t, int64(4096), EnvBytesOr(key, fallback))

	// 0 means "keep nothing", a real instruction, not an error.
	t.Setenv(key, "0")
	assert.Zero(t, EnvBytesOr(key, fallback), "zero is a valid budget, not a parse failure")

	// Beyond int32, because a byte budget routinely is.
	t.Setenv(key, "21474836480")
	assert.Equal(t, int64(21474836480), EnvBytesOr(key, fallback), "must not truncate to 32 bits")

	for _, bad := range []string{"", "abc", "-1", "1.5", "10GiB"} {
		t.Setenv(key, bad)
		assert.Equalf(t, fallback, EnvBytesOr(key, fallback),
			"%q must fall back — a misparsed budget silently changes what gets deleted", bad)
	}

	assert.Equal(t, fallback, EnvBytesOr("MACHE_TEST_BYTES_UNSET", fallback))
}

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
