package leyline

import (
	"context"
	"log"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

// resolvedLeyline records which leyline binary mache resolved for the
// auto-spawned daemon, from which tier, at what version. Populated by
// DiscoverOrStart's binary resolution; read by Provenance (surfaced via
// get_sheaf_status, mache-0dcd98). Zero value means mache hasn't resolved
// a binary this process — e.g. a pure-.db serve that never needs leyline,
// or a daemon started externally rather than auto-spawned.
var resolvedLeyline struct {
	mu      sync.Mutex
	path    string
	version string
	source  string // "PATH" | "cached" | "downloaded"
}

// LeylineProvenance is the observable provenance of the leyline binary
// mache resolved for the managed daemon. Surfaced so "which leyline is
// mache using?" is a log grep / MCP tool call instead of an archaeology
// dig (the failure mode that hid the stale-cached-0.4.1 skew, mache-0acdf6).
type LeylineProvenance struct {
	Path            string `json:"path"`
	Version         string `json:"version"`          // as reported by `<bin> --version` (best-effort)
	Source          string `json:"source"`           // PATH | cached | downloaded
	ExpectedVersion string `json:"expected_version"` // the compile-time pin (leylineBinaryVersion)
	VersionMatches  bool   `json:"version_matches"`  // reported version contains the pinned tag
}

// recordResolvedLeyline stores and logs the resolved binary's provenance.
func recordResolvedLeyline(path, source string) {
	version := queryBinaryVersion(path)
	resolvedLeyline.mu.Lock()
	resolvedLeyline.path = path
	resolvedLeyline.version = version
	resolvedLeyline.source = source
	resolvedLeyline.mu.Unlock()
	log.Printf("resolved leyline: %s (%s) [%s], expected %s", path, version, source, leylineBinaryVersion)
}

// Provenance returns the resolved leyline binary's provenance, or ok=false
// if mache hasn't resolved one this process. The returned ExpectedVersion
// is always populated (the compile-time pin) even when ok is false.
func Provenance() (LeylineProvenance, bool) {
	resolvedLeyline.mu.Lock()
	defer resolvedLeyline.mu.Unlock()
	if resolvedLeyline.path == "" {
		return LeylineProvenance{ExpectedVersion: leylineBinaryVersion}, false
	}
	tag := strings.TrimPrefix(leylineBinaryVersion, "v")
	return LeylineProvenance{
		Path:            resolvedLeyline.path,
		Version:         resolvedLeyline.version,
		Source:          resolvedLeyline.source,
		ExpectedVersion: leylineBinaryVersion,
		VersionMatches:  versionContainsTag(resolvedLeyline.version, tag),
	}, true
}

// versionContainsTag reports whether the pinned version tag appears as a
// whole dotted-number token in a `--version` line — NOT a raw substring,
// so the pin "0.5.1" does not false-match a "0.5.11" release. Tokenizes on
// any non-[0-9.] rune, so "leyline 0.5.1 (open)" → ["0.5.1"].
func versionContainsTag(version, tag string) bool {
	if version == "" || tag == "" {
		return false
	}
	return slices.Contains(strings.FieldsFunc(version, func(r rune) bool {
		return (r < '0' || r > '9') && r != '.'
	}), tag)
}

// probeTimeout bounds the `<bin> --version` probe so a hung binary can't
// block the spawn path. 2s is ample in prod (resolving a local binary's
// version); it's a package var only so tests can widen it — under
// `go test -race ./...` the fake-binary subprocess can be starved past 2s by
// cross-package contention, which is a test-environment artifact, not a prod
// concern (mache-3a0da5).
var probeTimeout = 2 * time.Second

// queryBinaryVersion runs `<bin> --version` and returns the trimmed output
// (best-effort, "" on any error). Bounded so a hung binary can't block the
// spawn path; provenance is informational, never load-bearing.
func queryBinaryVersion(bin string) string {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
