package mcpserve

import "github.com/agentic-research/mache/internal/buildinfo"

// producerVersion is the mache version this server announces (MCP serverInfo)
// and binds into graph cache keys. Defaults to the committed release base and
// is overridden by cmd/register.go with the ldflags-stamped build version —
// the same pull-based injection pattern as internal/buildcache (B24,
// mache-96c378): reads happen at call time, so init order between this
// package and cmd cannot produce a stale or empty identity.
var producerVersion = buildinfo.Version

// SetVersion overrides the announced/bound version. Called once from cmd
// wiring; empty values are refused — a graph cache key with an empty version
// term would silently collide across releases, the exact defect
// full-provenance keys exist to prevent.
func SetVersion(v string) {
	if v != "" {
		producerVersion = v
	}
}

// serverVersion returns the identity announced to MCP clients and bound into
// cache keys.
func serverVersion() string { return producerVersion }
