package buildcache

import "github.com/agentic-research/mache/internal/buildinfo"

// producerVersion is the mache version recorded in lockfiles and OCI
// metadata. It defaults to the committed release base and is OVERRIDDEN by
// cmd/register.go with the ldflags-stamped build version (B3/B24,
// mache-96c378): reads are pull-based at call time — never copied at package
// init — so registration order between this package and cmd cannot produce a
// stale or empty identity.
var producerVersion = buildinfo.Version

// SetProducerVersion overrides the recorded producer identity. Called once,
// from cmd wiring, with the ldflags-resolved build version.
func SetProducerVersion(v string) {
	if v != "" {
		producerVersion = v
	}
}

// ProducerVersion returns the identity stamped into lockfiles and cache
// metadata.
func ProducerVersion() string { return producerVersion }
