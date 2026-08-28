package projcfg

import "os"

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
