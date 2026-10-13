package fixturedb

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-research/mache/internal/leyline"
)

// resolveVersionedBinary returns a path to a ley-line binary reporting
// version, or "" with the reason it could not find one.
//
// Two sources, both read-only: MACHE_LEYLINE_BINARY when it happens to report
// that version, and the version-namespaced cache ~/.mache/bin/leyline-<v>
// which is where mache puts every binary it resolves. No download — a
// conformance test reports the world, it does not change it.
func resolveVersionedBinary(t *testing.T, version string) (string, string) {
	t.Helper()

	reports := func(path string) bool {
		out, err := exec.Command(path, "--version").Output()
		return err == nil && strings.Contains(string(out), strings.TrimPrefix(version, "v"))
	}

	if override := os.Getenv(leyline.BinaryOverrideEnv); override != "" && reports(override) {
		return override, ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "cannot determine home directory"
	}
	cached := filepath.Join(home, ".mache", "bin", "leyline-"+version)
	if _, serr := os.Stat(cached); serr == nil && reports(cached) {
		return cached, ""
	}
	return "", "no " + version + " binary at " + cached +
		" and " + leyline.BinaryOverrideEnv + " does not report one"
}

// TestLeylineV4Schema_MatchesItsBinary re-derives schema_leyline.go, the
// projection-v4 mirror, from a real binary at leylineSchemaVersion.
//
// The pin has moved to projection-v6, which the primary conformance test
// checks unconditionally. This mirror stays because mache still reads the
// artifacts the previous pin built, and the producers that test those reads
// (fixturedb.Leyline) must keep matching what that leyline really wrote. It
// skips when no such binary is cached: CI no longer resolves it, so this is a
// local check, run whenever the v4 mirror is edited.
func TestLeylineV4Schema_MatchesItsBinary(t *testing.T) {
	bin, why := resolveVersionedBinary(t, leylineSchemaVersion)
	if bin == "" {
		t.Skipf("fixturedb: %s — the v4 mirror is NOT verified in this run", why)
	}
	t.Logf("fixturedb: deriving against %s", bin)
	assertMirror(t, bin, leylineSchemaVersion, "schema_leyline.go", leylineTables, leylineIndexes)
}
