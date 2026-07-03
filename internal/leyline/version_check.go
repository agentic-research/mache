package leyline

import (
	"fmt"
	"strconv"
	"strings"
)

// expectedLeylineWireFormatMajor is the ley-line-open daemon wire-format major
// this mache build's schema-client speaks; it mirrors LLO's
// version::WIRE_FORMAT_MAJOR. A daemon advertising a different major means the
// capnp/JSON shapes have structurally diverged — the schema-client would decode
// garbage — so mache refuses rather than proceeding (mache-8kif / M3).
const expectedLeylineWireFormatMajor uint64 = 1

// VerifyReachableDaemonVersion is the startup wire-compat handshake: if a
// leyline daemon is ALREADY reachable (it never auto-starts one), it queries the
// leyline_version op and refuses to proceed on a structural mismatch. It returns
// nil when no daemon is reachable, when the daemon predates the leyline_version
// op, or when the versions are compatible — a version probe must never be
// stricter than the actual wire decode.
func VerifyReachableDaemonVersion() error {
	sock, err := DiscoverSocket() // never auto-starts a daemon
	if err != nil {
		return nil // nothing running to check
	}
	if !isSocketAlive(sock) {
		return nil // stale socket file, no listener
	}
	c, err := DialSocket(sock)
	if err != nil {
		return nil // transient dial failure — don't block startup on a probe
	}
	defer func() { _ = c.Close() }()
	return c.VerifyVersion(leylineBinaryVersion)
}

// VerifyVersion queries the daemon's leyline_version op and checks wire-format
// and schema compatibility against clientVersion (this build's leyline
// schema-client version). A daemon that predates the op (SendOp errors) is not
// an error — older daemons simply can't be verified, and we must not refuse a
// daemon we might still decode fine.
func (c *SocketClient) VerifyVersion(clientVersion string) error {
	resp, err := c.SendOp(map[string]any{"op": "leyline_version"})
	if err != nil {
		return nil
	}
	return checkLeylineVersionCompat(resp, clientVersion)
}

// checkLeylineVersionCompat compares a leyline_version response against this
// build's client version. It returns a non-nil, remediation-carrying error on a
// genuine incompatibility, and nil when compatible OR when the relevant fields
// are absent (an older daemon that omits them can't be refused on an unknown).
// Pure — unit-tested directly (mache-8kif / external-review M3).
func checkLeylineVersionCompat(resp map[string]any, clientVersion string) error {
	// 1. Wire-format major: a mismatch is a structural break — the schema-client
	//    decodes a different shape. Only present-and-different fails.
	if major, ok := versionUint(resp["wire_format_major"]); ok && major != expectedLeylineWireFormatMajor {
		return fmt.Errorf(
			"leyline daemon wire-format is v%d but this mache build speaks v%d (daemon schema %v) — "+
				"upgrade ley-line-open to a build whose wire_format_major is %d, or rebuild mache against the daemon's schema",
			major, expectedLeylineWireFormatMajor, resp["schema_version"], expectedLeylineWireFormatMajor)
	}

	// 2. compat_min: the daemon refuses clients older than this schema version.
	//    If mache's schema-client is older, fail fast with the daemon's implied
	//    remediation rather than letting a later op fail cryptically.
	if compatMin, ok := resp["compat_min"].(string); ok && compatMin != "" {
		if compareSemver(clientVersion, compatMin) < 0 {
			return fmt.Errorf(
				"this mache build's leyline schema-client is %s but the daemon requires >= %s — "+
					"upgrade mache (bump the go.mod leyline-schema pin and leylineBinaryVersion) to at least %s",
				strings.TrimPrefix(clientVersion, "v"), compatMin, compatMin)
		}
	}
	return nil
}

// versionUint extracts an unsigned integer from a JSON-decoded value that may be
// a number (float64) or a string — the leyline capnp-json codec emits some
// integers as JSON strings (see SocketClient.SendOpInto).
func versionUint(v any) (uint64, bool) {
	switch n := v.(type) {
	case float64:
		if n < 0 {
			return 0, false
		}
		return uint64(n), true
	case string:
		u, err := strconv.ParseUint(strings.TrimSpace(n), 10, 64)
		if err != nil {
			return 0, false
		}
		return u, true
	default:
		return 0, false
	}
}

// compareSemver compares two dotted versions (optional leading 'v'; pre-release
// / build suffixes ignored) by major, then minor, then patch. Returns -1, 0, or
// +1; missing components count as 0.
func compareSemver(a, b string) int {
	pa, pb := parseSemverParts(a), parseSemverParts(b)
	for i := range 3 {
		switch {
		case pa[i] < pb[i]:
			return -1
		case pa[i] > pb[i]:
			return 1
		}
	}
	return 0
}

func parseSemverParts(v string) [3]uint64 {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out [3]uint64
	for i, p := range strings.SplitN(v, ".", 3) {
		if i >= 3 {
			break
		}
		out[i], _ = strconv.ParseUint(p, 10, 64)
	}
	return out
}
