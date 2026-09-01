//go:build unix

package lltest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/leyline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLeyline writes an executable that answers --version with the given line,
// so resolution can be exercised without a real leyline (which these tests must
// never download).
func fakeLeyline(t *testing.T, versionLine string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "leyline")
	require.NoError(t, os.WriteFile(path,
		[]byte("#!/bin/sh\necho '"+versionLine+"'\n"), 0o755))
	return path
}

// TestDecideBinary_HonoursTheProductionOverride is the point of mache-cc1a70.
//
// mache already had exactly one override, MACHE_LEYLINE_BINARY, which
// production's ResolveBinary consults before every pinned tier. The gated tests
// called CachedPinnedBinary directly and ignored it, so pointing mache at a
// release candidate moved `mache build` onto the candidate while the
// conformance and parity gates went on testing the pin — or skipped. Same knob
// now drives both.
func TestDecideBinary_HonoursTheProductionOverride(t *testing.T) {
	bin := fakeLeyline(t, "leyline 0.19.0-rc.1 (open)")
	t.Setenv(leyline.BinaryOverrideEnv, bin)

	d := decideBinary()

	require.Empty(t, d.reason, "a usable override must resolve, not skip")
	assert.True(t, d.bin.Override, "resolution came from the override, not the cache")
	assert.Equal(t, bin, d.bin.Path)
	assert.Equal(t, "0.19.0-rc.1", d.bin.Version,
		"the version reported must be the override's own, not the pin's")
	assert.NotEqual(t, leyline.PinnedBinaryVersion(), "v"+d.bin.Version,
		"sanity: this fixture is deliberately not the pinned version")
}

// TestDecideBinary_BrokenOverrideIsFatalNotASkip guards the distinction that
// makes the override safe. Skipping is right for the DEFAULT path — no cached
// pin means there is nothing to test — but naming a binary states an intent,
// and silently testing nothing would report success for a validation that never
// ran.
//
// Asserted on the decision rather than through a *testing.T, whose Fatalf would
// terminate the test making the assertion.
func TestDecideBinary_BrokenOverrideIsFatalNotASkip(t *testing.T) {
	notExecutable := filepath.Join(t.TempDir(), "leyline")
	require.NoError(t, os.WriteFile(notExecutable, []byte("not executable"), 0o644))

	for _, tc := range []struct{ name, path string }{
		{"nonexistent path", filepath.Join(t.TempDir(), "no-such-leyline")},
		{"present but not executable", notExecutable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(leyline.BinaryOverrideEnv, tc.path)

			d := decideBinary()

			require.NotEmpty(t, d.reason, "an unusable override must not resolve")
			assert.True(t, d.fatal,
				"a named-but-unusable override must FAIL; a skip would report a validation that never ran")
			assert.Contains(t, d.reason, tc.path,
				"the reason must name the binary that was asked for")
		})
	}
}

// TestDecideBinary_UnsetOverrideIsUnchanged pins that wiring the override in did
// not disturb the default: still the pinned cache, still never downloading, and
// still a SKIP (never a failure) when nothing is cached.
func TestDecideBinary_UnsetOverrideIsUnchanged(t *testing.T) {
	t.Setenv(leyline.BinaryOverrideEnv, "")

	d := decideBinary()

	assert.False(t, d.bin.Override, "no override set, so this must be the pinned path")
	assert.False(t, d.fatal, "the default path never fails; a missing pin is a skip")

	if cached := leyline.CachedPinnedBinary(); cached == "" {
		assert.NotEmpty(t, d.reason, "nothing cached must produce a skip reason")
		return
	} else {
		assert.Equal(t, cached, d.bin.Path)
	}
	assert.Equal(t, leyline.PinnedBinaryVersion(), "v"+d.bin.Version,
		"the pinned path reports the pin")
}

// TestDecideBinary_UnparseableOverrideVersionStillReports covers a fork or a
// dev build whose --version mache cannot parse. The operator chose that binary
// by hand, so the answer must be shown rather than blanked — a run reporting
// version "" gives a reader nothing to check the result against.
func TestDecideBinary_UnparseableOverrideVersionStillReports(t *testing.T) {
	t.Setenv(leyline.BinaryOverrideEnv, fakeLeyline(t, "some-fork build abc123"))

	d := decideBinary()

	require.Empty(t, d.reason)
	assert.Equal(t, "some-fork build abc123", d.bin.Version)
}
