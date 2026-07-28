package leyline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestProvenance_RecordAndReport verifies the resolved-leyline provenance
// round-trips and that version_matches reflects whether the reported
// version contains the pinned tag — the at-a-glance skew signal that the
// stale-0.4.1 incident lacked (mache-0dcd98 / mache-0acdf6).
func TestProvenance_RecordAndReport(t *testing.T) {
	// Reset shared state so the test is order-independent, and restore the
	// zero value on exit so we don't leak stale provenance to later tests.
	resetResolved := func() {
		resolvedLeyline.mu.Lock()
		resolvedLeyline.path, resolvedLeyline.version, resolvedLeyline.source = "", "", ""
		resolvedLeyline.mu.Unlock()
	}
	resetResolved()
	t.Cleanup(resetResolved)

	// The 2s prod probe timeout is too tight for this test's fake-binary
	// subprocess under full-suite -race contention (mache-3a0da5); widen it so
	// the test still exercises the real exec+parse path without deadline
	// flakiness. Package tests are sequential (no t.Parallel), so overriding
	// the package var is race-safe.
	origTimeout := probeTimeout
	probeTimeout = 30 * time.Second
	t.Cleanup(func() { probeTimeout = origTimeout })

	// Before any resolution: ok=false, but expected version still reported.
	if p, ok := Provenance(); ok || p.ExpectedVersion != leylineBinaryVersion {
		t.Fatalf("pre-resolution: got ok=%v expected=%q, want ok=false expected=%q", ok, p.ExpectedVersion, leylineBinaryVersion)
	}

	// A fake binary whose --version prints the PINNED tag → version_matches true.
	matching := writeFakeLeyline(t, "leyline "+leylineBinaryVersion+" (open)")
	recordResolvedLeyline(matching, "PATH")
	p, ok := Provenance()
	if !ok {
		t.Fatal("post-resolution: ok=false")
	}
	if p.Path != matching || p.Source != "PATH" {
		t.Errorf("path/source: got %q/%q", p.Path, p.Source)
	}
	if !p.VersionMatches {
		t.Errorf("version %q should match pinned %q", p.Version, leylineBinaryVersion)
	}

	// A binary reporting a DIFFERENT version → version_matches false (the
	// exact stale-cached-binary signal).
	stale := writeFakeLeyline(t, "leyline 0.4.1 (open)")
	recordResolvedLeyline(stale, "cached")
	p, _ = Provenance()
	if p.Source != "cached" {
		t.Errorf("source: got %q want cached", p.Source)
	}
	if p.VersionMatches {
		t.Errorf("stale version %q must NOT match pinned %q", p.Version, leylineBinaryVersion)
	}

	// Substring false-positive guard: a "<pin>1" release (e.g. 0.5.11 vs pin
	// 0.5.1) must NOT match — version_matches is whole-token, not substring.
	superstr := writeFakeLeyline(t, "leyline "+strings.TrimPrefix(leylineBinaryVersion, "v")+"1 (open)")
	recordResolvedLeyline(superstr, "PATH")
	if p, _ := Provenance(); p.VersionMatches {
		t.Errorf("version %q must NOT match pinned %q (substring false-positive)", p.Version, leylineBinaryVersion)
	}
}

// writeFakeLeyline writes an executable shell script that prints versionLine
// on `--version`, returning its path.
func writeFakeLeyline(t *testing.T, versionLine string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "leyline")
	script := "#!/bin/sh\necho '" + versionLine + "'\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake leyline: %v", err)
	}
	return p
}

// TestQueryBinaryVersion_HonorsProbeTimeout proves probeTimeout is load-bearing:
// a binary that only prints its version after sleeping past a tiny deadline
// yields "" (deadline fired), while a generous deadline resolves the same
// binary. Falsifiable — if the probe ignored probeTimeout, the tiny-deadline
// case would still return the version and this test would fail (mache-3a0da5).
func TestQueryBinaryVersion_HonorsProbeTimeout(t *testing.T) {
	orig := probeTimeout
	t.Cleanup(func() { probeTimeout = orig })

	slow := filepath.Join(t.TempDir(), "leyline")
	script := "#!/bin/sh\nsleep 1\necho 'leyline " + leylineBinaryVersion + " (open)'\n"
	if err := os.WriteFile(slow, []byte(script), 0o755); err != nil {
		t.Fatalf("write slow fake leyline: %v", err)
	}

	probeTimeout = 50 * time.Millisecond
	if v := queryBinaryVersion(slow); v != "" {
		t.Errorf("deadline should fire: got %q, want empty", v)
	}

	probeTimeout = 5 * time.Second
	if v := queryBinaryVersion(slow); v == "" {
		t.Error("generous deadline should resolve the version, got empty")
	}
}

// RecordResolved is the public entry point for non-daemon callers (mache
// build's autoInvokeLeylineParse). Before it existed, Provenance() was
// populated only by DiscoverOrStart, so `mache build` resolved a binary, ran
// it, and still reported "no binary resolved this process" — leaving
// _mache_meta with nothing to stamp and .db artifacts whose producing leyline
// was unknowable (mache-438104).
//
// Declared in this file, after TestProvenance, deliberately: that test asserts
// the PRE-resolution state (ok=false), so it must run before anything records.
// Go runs a package's tests in declaration order across filename-sorted files,
// and a separate provenance_record_test.go would sort ahead of provenance_test.go
// and break it.
func TestRecordResolved_PublishesPathSourceAndVersion(t *testing.T) {
	origTimeout := probeTimeout
	probeTimeout = 30 * time.Second
	t.Cleanup(func() { probeTimeout = origTimeout })

	bin := writeFakeLeyline(t, "leyline "+leylineBinaryVersion+" (open)")
	RecordResolved(bin, "resolved")

	p, ok := Provenance()
	if !ok {
		t.Fatal("after RecordResolved, Provenance must report a resolved binary")
	}
	if p.Path != bin {
		t.Errorf("path: got %q want %q", p.Path, bin)
	}
	if p.Source != "resolved" {
		t.Errorf("source: got %q want %q", p.Source, "resolved")
	}
	if p.Version == "" {
		t.Error("version must be queried from the binary, not left empty — " +
			"an empty version is what writeBuildMetadata records as 'unresolved'")
	}
	if !p.VersionMatches {
		t.Errorf("fake reports the pin %q but VersionMatches is false (got %q)",
			leylineBinaryVersion, p.Version)
	}
}

// An empty path must not clobber an existing record. The guard matters because
// callers pass the result of a resolution that may have failed; overwriting a
// good record with nothing would make the artifact stamp WORSE than not calling
// it at all.
func TestRecordResolved_EmptyPathIsANoOp(t *testing.T) {
	origTimeout := probeTimeout
	probeTimeout = 30 * time.Second
	t.Cleanup(func() { probeTimeout = origTimeout })

	bin := writeFakeLeyline(t, "leyline "+leylineBinaryVersion+" (open)")
	RecordResolved(bin, "resolved")
	before, _ := Provenance()

	RecordResolved("", "should-be-ignored")

	after, ok := Provenance()
	if !ok {
		t.Fatal("an empty-path call must not un-resolve the record")
	}
	if after.Path != before.Path || after.Source != before.Source {
		t.Errorf("empty path overwrote the record: %q/%q -> %q/%q",
			before.Path, before.Source, after.Path, after.Source)
	}
}
