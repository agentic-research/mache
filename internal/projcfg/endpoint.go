package projcfg

import (
	"os"
	"strconv"
	"time"
)

// Canonical MCP transport endpoint. mache serves Streamable HTTP on this
// address, and onboarding registers clients against it. Re-homed here from
// cmd/daemon_agent.go (R2, mache-96c378): editor/MCP-config generation needs
// the endpoint, and reaching upward into cmd for it was the other would-be
// import cycle. The daemon lifecycle code in cmd reads these same values, so
// there is exactly one answer to "where does mache listen".
//
// Overridable via environment so a second, isolated daemon can coexist with
// the canonical one (the hermetic launchd E2E, or an operator's canary).
var (
	MacheHTTPListen = EnvOr("MACHE_DAEMON_LISTEN", "localhost:7532")
	MacheHTTPURL    = "http://" + MacheHTTPListen + "/mcp"
)

// EnvOr returns the environment value for key, or fallback when unset/empty.
func EnvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// EnvDurationOr parses key as a Go duration, or returns fallback when unset,
// unparseable, or non-positive. Lives here beside EnvOr because the daemon
// lifecycle's tunables (settle, drain, breaker window) are read from three
// different packages, and three copies of this parser is exactly what the
// duplicate-code gate exists to prevent.
func EnvDurationOr(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return fallback
}

// EnvIntOr parses key as a positive integer, or returns fallback.
func EnvIntOr(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}
