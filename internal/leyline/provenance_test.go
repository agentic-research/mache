package leyline

import (
	"os"
	"path/filepath"
	"testing"
)

// TestProvenance_RecordAndReport verifies the resolved-leyline provenance
// round-trips and that version_matches reflects whether the reported
// version contains the pinned tag — the at-a-glance skew signal that the
// stale-0.4.1 incident lacked (mache-0dcd98 / mache-0acdf6).
func TestProvenance_RecordAndReport(t *testing.T) {
	// Reset shared state so the test is order-independent.
	resolvedLeyline.mu.Lock()
	resolvedLeyline.path, resolvedLeyline.version, resolvedLeyline.source = "", "", ""
	resolvedLeyline.mu.Unlock()

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
