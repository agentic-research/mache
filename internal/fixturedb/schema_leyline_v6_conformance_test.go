package fixturedb

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/leyline"
)

// resolveV6Binary returns a path to a ley-line binary reporting
// leylineV6SchemaVersion, or "" with the reason it could not find one.
//
// Two sources, both read-only: MACHE_LEYLINE_BINARY when it happens to report
// that version, and the version-namespaced cache ~/.mache/bin/leyline-<v>
// which is where mache puts every binary it resolves. No download — a
// conformance test reports the world, it does not change it.
func resolveV6Binary(t *testing.T) (string, string) {
	t.Helper()

	reports := func(path string) bool {
		out, err := exec.Command(path, "--version").Output()
		return err == nil && strings.Contains(string(out), strings.TrimPrefix(leylineV6SchemaVersion, "v"))
	}

	if override := os.Getenv(leyline.BinaryOverrideEnv); override != "" && reports(override) {
		return override, ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "cannot determine home directory"
	}
	cached := filepath.Join(home, ".mache", "bin", "leyline-"+leylineV6SchemaVersion)
	if _, serr := os.Stat(cached); serr == nil && reports(cached) {
		return cached, ""
	}
	return "", "no " + leylineV6SchemaVersion + " binary at " + cached +
		" and " + leyline.BinaryOverrideEnv + " does not report one"
}

// TestLeylineV6Schema_MatchesBinary re-derives schema_leyline_v6.go from a real
// v0.20.0 binary and fails on any drift.
//
// IT SKIPS WHEN NO SUCH BINARY IS PRESENT, which is a compromise and is worth
// naming. The pin is still v0.19.1, so CI has no v0.20.0 binary to check
// against, and a gate that can never pass would be red forever rather than
// informative. The skip is time-boxed by construction:
// TestLeylineV6Schema_IsCheckedOnceThePinReachesIt fails the moment the pin
// reaches v0.20.0 with this mirror still sitting off to the side — at which
// point v6 becomes the pinned shape and the primary conformance test covers it
// unconditionally.
//
// Until then this is the arrangement mache-cc1a70 established for an LLO
// candidate: run it locally against the candidate and treat a diff as this
// file's re-derivation worklist, enumerated before either side ships.
func TestLeylineV6Schema_MatchesBinary(t *testing.T) {
	bin, why := resolveV6Binary(t)
	if bin == "" {
		t.Skipf("fixturedb: %s — this mirror is NOT verified in this run; "+
			"see TestLeylineV6Schema_IsCheckedOnceThePinReachesIt for the time box", why)
	}
	t.Logf("fixturedb: deriving against %s", bin)

	got := derivePinnedSchema(t, bin)

	modelled := map[string]string{}
	for _, m := range []map[string]string{leylineV6Tables, leylineV6Views, leylineV6Indexes} {
		maps.Copy(modelled, m)
	}

	// Only the objects this package models: a real db carries capnp blobs and
	// bookkeeping tables no fixture needs, and modelling them would only
	// create more surface to drift.
	for name, want := range modelled {
		g, ok := got[name]
		require.Truef(t, ok,
			"%s no longer exists in %s output (it has %v) — re-derive schema_leyline_v6.go",
			name, leylineV6SchemaVersion, slices.Sorted(maps.Keys(got)))
		assert.Equal(t, normalizeDDL(want), normalizeDDL(g),
			"%s drifted from %s — re-derive schema_leyline_v6.go "+
				"(leyline parse; SELECT sql FROM sqlite_master) rather than editing by hand",
			name, leylineV6SchemaVersion)
	}
}

// TestLeylineV6Schema_IsCheckedOnceThePinReachesIt is the time box on the skip
// above.
//
// While the pin trails v6 this mirror is unverified in CI, which is tolerable
// only because it cannot stay that way silently: the moment the pin reaches
// v0.20.0, v6 IS the pinned shape, and leaving schema_leyline.go as the
// checked mirror would mean the primary conformance test is comparing the
// pinned binary against a v4 model — it would fail anyway, but for a confusing
// reason. This fails first, and says what to do.
func TestLeylineV6Schema_IsCheckedOnceThePinReachesIt(t *testing.T) {
	if leyline.PinnedBinaryVersion() != leylineV6SchemaVersion {
		return
	}
	assert.Equal(t, leylineV6SchemaVersion, leylineSchemaVersion,
		"the pin has reached %s, so v6 is now the PINNED shape: schema_leyline_v6.go must "+
			"become schema_leyline.go (the mirror the primary conformance test checks), and the "+
			"v4 DDL kept only as a historical model for the pre-v6 artifacts lloschema still reads",
		leylineV6SchemaVersion)
}
