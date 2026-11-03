package leyline

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// expectedLeylineWireFormatMajor is the ley-line-open daemon wire-format major
// this mache build's schema-client speaks; it mirrors LLO's
// version::WIRE_FORMAT_MAJOR. A daemon advertising a different major means the
// capnp/JSON shapes have structurally diverged — the schema-client would decode
// garbage — so mache refuses rather than proceeding (mache-8kif / M3).
const expectedLeylineWireFormatMajor uint64 = 1

// VerifyReachableDaemonVersion is the startup handshake: if a leyline daemon
// is ALREADY listening (it never auto-starts one), it must be the pinned release
// and speak a wire mache decodes, or serve refuses to start (verifyDaemon). It
// returns nil only when nothing is listening.
func VerifyReachableDaemonVersion() error {
	sock, err := DiscoverSocket() // never auto-starts a daemon
	if err != nil {
		return nil // nothing running to check
	}
	if !isSocketAlive(sock) {
		return nil // stale socket file, no listener
	}
	return verifyDaemon(sock)
}

// ErrDaemonNotPinned is a reachable leyline daemon refused because it is not
// the release mache pins.
var ErrDaemonNotPinned = errors.New("leyline daemon is not the pinned release")

// pinDriftError refuses a daemon whose binary version is not the pin, or that
// does not report one. sock, and the daemon's pid when its owner record names
// one, go in the message so the user knows which process to stop.
//
// It used to warn and carry on. That closed the visibility hole that produced
// mache-608a3c twice, but a warned-about daemon still served: LLO changes the
// projection and the write wire between releases (v0.21.0 is breaking on both),
// so a daemon left over from an older mache would answer with the wrong shapes
// while every handshake passed. One daemon per repo is the authority; mache
// refuses a mismatched one, failing closed, and says what to restart
// (mache-a2edb2). A developer who set MACHE_LEYLINE_BINARY chose another
// binary on purpose, and is not refused.
func pinDriftError(resp map[string]any, sock string) error {
	if os.Getenv(BinaryOverrideEnv) != "" {
		return nil
	}
	where := "the leyline daemon at " + sock
	if rec, ok := ReadOwnerRecord(sock); ok && rec.DaemonPID > 0 {
		where += fmt.Sprintf(" (pid %d)", rec.DaemonPID)
	}
	got, ok := daemonBinaryVersion(resp)
	if !ok {
		return fmt.Errorf("%w: %s does not report its version, and mache needs %s — "+
			"stop it and mache will start the pinned one", ErrDaemonNotPinned, where, leylineBinaryVersion)
	}
	if parseSemverParts(got) == parseSemverParts(leylineBinaryVersion) {
		return nil
	}
	return fmt.Errorf("%w: %s is v%s, mache needs %s — stop it and mache will start the pinned one "+
		"(or set %s to use another leyline on purpose)",
		ErrDaemonNotPinned, where, strings.TrimPrefix(got, "v"), leylineBinaryVersion, BinaryOverrideEnv)
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

// verifyDaemon asks a daemon mache did NOT spawn which release it is, and
// refuses it unless it is the pin (pinDriftError) and its wire is one mache
// decodes (checkLeylineVersionCompat). Every DiscoverOrStart consumer adopts
// through here (mache-233902), and so does serve's startup handshake. It asks on every
// adoption: one socket round trip, and a user who restarts the daemon onto the
// right release is served at once rather than refused for the rest of the
// process. A daemon that cannot be asked is refused too, failing closed
// (mache-a2edb2): a daemon that predates the leyline_version op predates every
// release mache could pin.
func verifyDaemon(sock string) error {
	c, err := DialSocket(sock)
	if err != nil {
		return fmt.Errorf("%w: cannot ask the leyline daemon at %s its version: %v", ErrDaemonNotPinned, sock, err)
	}
	defer func() { _ = c.Close() }()
	resp, err := c.SendOp(map[string]any{"op": "leyline_version"})
	if err != nil {
		return fmt.Errorf("%w: the leyline daemon at %s does not answer leyline_version (mache needs %s): %v",
			ErrDaemonNotPinned, sock, leylineBinaryVersion, err)
	}
	if err := pinDriftError(resp, sock); err != nil {
		return err
	}
	return checkLeylineVersionCompat(resp, leylineBinaryVersion)
}

// AdoptedDaemonVersion reports the leyline version a reachable daemon claims,
// and whether it could be determined. Exported for `mache doctor`, which
// otherwise reports the RESOLVED BINARY's pin and says nothing about the daemon
// actually serving — accurate about what it measured, silent about what the
// reader cares about.
func AdoptedDaemonVersion() (string, bool) {
	sock, err := findExistingSocket()
	if err != nil || !isSocketAlive(sock) {
		return "", false
	}
	c, err := DialSocket(sock)
	if err != nil {
		return "", false
	}
	defer func() { _ = c.Close() }()
	resp, err := c.SendOp(map[string]any{"op": "leyline_version"})
	if err != nil {
		return "", false
	}
	return daemonBinaryVersion(resp)
}

// daemonBinaryVersion is the binary version a leyline_version reply reports,
// the only quantity the pin can be compared with.
//
// It is NOT schema_version. LLO versions the public schema-client contract
// independently of the binary (v0.19.1 and v0.20.1 both report schema 0.18.1),
// and mache used to read "version", a key the op never sends, then fall back to
// schema_version, so every daemon on the pinned binary looked stale (mache-d446a4). A
// daemon that does not report binary_version cannot be judged, so it is not.
func daemonBinaryVersion(resp map[string]any) (string, bool) {
	v, ok := resp["binary_version"].(string)
	return v, ok && v != ""
}
