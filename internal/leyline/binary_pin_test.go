package leyline

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pinLeylineSHA pins content's SHA-256 as the leyline release pin for the
// current platform for the duration of the test (restored on cleanup). Lets
// download tests serve arbitrary bytes and still pass verifyLeylineSHA256.
func pinLeylineSHA(t *testing.T, content []byte) {
	t.Helper()
	sum := sha256.Sum256(content)
	key := runtime.GOOS + "-" + runtime.GOARCH
	orig, had := leylinePinnedSHA256[key]
	leylinePinnedSHA256[key] = hex.EncodeToString(sum[:])
	t.Cleanup(func() {
		if had {
			leylinePinnedSHA256[key] = orig
		} else {
			delete(leylinePinnedSHA256, key)
		}
	})
}

// mache-46af85 — mache must use ONLY the pinned leyline version. A wrong-version
// leyline on PATH (the recurring "0.5.7 shadows the pin" trap, or a raw-main
// build) produces different _ast output than the pinned release and silently
// diverges local runs from CI. These tests capture that: ResolveBinary rejects a
// non-pinned PATH binary, and accepts a pinned one.

// writeLeylineStub writes an executable stub that reports versionLine on
// `--version`, and returns its path.
func writeLeylineStub(t *testing.T, dir, versionLine string) string {
	t.Helper()
	p := filepath.Join(dir, "leyline")
	// #!/bin/sh is an absolute interpreter path, so it runs even with PATH
	// stripped to just `dir`.
	require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\necho '"+versionLine+"'\n"), 0o755))
	return p
}

// hermeticEnv points HOME at an empty temp dir (so ~/.mache/bin/leyline can't
// leak in) and PATH at only pathDir.
func hermeticEnv(t *testing.T, pathDir string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", pathDir)
}

func TestExtractSemver(t *testing.T) {
	cases := map[string]string{
		"leyline 0.7.0 (open)": "0.7.0",
		"leyline v0.7.0":       "0.7.0",
		"leyline 0.5.7 (open)": "0.5.7",
		"0.6.0":                "0.6.0",
		"no version here":      "",
		"":                     "",
	}
	for in, want := range cases {
		assert.Equalf(t, want, extractSemver(in), "extractSemver(%q)", in)
	}
}

func TestPinnedBinaryReleaseMatchesAdoptedContract(t *testing.T) {
	assert.Equal(t, "v0.18.2", leylineBinaryVersion)
}

func TestPinnedBinarySHA256CoversSupportedPlatforms(t *testing.T) {
	platforms := []string{"darwin-amd64", "darwin-arm64", "linux-amd64", "linux-arm64"}
	require.Len(t, leylinePinnedSHA256, len(platforms))
	for _, platform := range platforms {
		digest, ok := leylinePinnedSHA256[platform]
		require.Truef(t, ok, "missing SHA-256 pin for %s", platform)
		raw, err := hex.DecodeString(digest)
		require.NoErrorf(t, err, "invalid SHA-256 pin for %s", platform)
		require.Lenf(t, raw, sha256.Size, "wrong SHA-256 length for %s", platform)
	}
}

func TestLivePinnedReleaseDownload(t *testing.T) {
	if os.Getenv("MACHE_LIVE_LEYLINE_RELEASE") != "1" {
		t.Skip("set MACHE_LIVE_LEYLINE_RELEASE=1 or run task leyline:verify-release")
	}

	dest := filepath.Join(t.TempDir(), "leyline")
	got, err := downloadLeyline(dest)
	require.NoError(t, err)
	assert.Equal(t, dest, got)
	assert.True(t, leylineVersionMatchesPin(got), "downloaded release must report the pinned version")
}

func TestLeylineVersionMatchesPin(t *testing.T) {
	pin := strings.TrimPrefix(leylineBinaryVersion, "v") // e.g. "0.7.0"
	dir := t.TempDir()

	match := writeLeylineStub(t, filepath.Join(mustMkdir(t, dir, "ok")), "leyline "+pin+" (open)")
	assert.True(t, leylineVersionMatchesPin(match), "the pinned version must match")

	wrong := writeLeylineStub(t, filepath.Join(mustMkdir(t, dir, "old")), "leyline 0.5.7 (open)")
	assert.False(t, leylineVersionMatchesPin(wrong), "a wrong version must NOT match")

	// Patch drift within the pinned major.minor must NOT match either: LLO
	// patch releases change the emitted _ast schema (v0.7.4 added
	// container_node_id, v0.7.5 added canonical_kind), so a floating patch
	// silently diverges local builds from CI's exact pinned download
	// (mache-608a3c).
	pinParts := parseSemverParts(leylineBinaryVersion)
	patchDrift := fmt.Sprintf("%d.%d.%d", pinParts[0], pinParts[1], pinParts[2]+1)
	drift := writeLeylineStub(t, filepath.Join(mustMkdir(t, dir, "drift")), "leyline "+patchDrift+" (open)")
	assert.False(t, leylineVersionMatchesPin(drift),
		"a patch-drifted version within the pinned major.minor must NOT match")

	garbage := writeLeylineStub(t, filepath.Join(mustMkdir(t, dir, "junk")), "not a version")
	assert.False(t, leylineVersionMatchesPin(garbage), "unparseable output must NOT match")

	assert.False(t, leylineVersionMatchesPin(filepath.Join(dir, "does-not-exist")),
		"a non-existent binary must NOT match")
}

func TestResolveBinary_RejectsWrongVersionOnPath(t *testing.T) {
	dir := t.TempDir()
	writeLeylineStub(t, dir, "leyline 0.5.7 (open)") // wrong version on PATH
	hermeticEnv(t, dir)

	got, err := ResolveBinary(false) // no download
	require.Error(t, err, "a wrong-version PATH leyline must be rejected, not returned")
	assert.NotEqual(t, filepath.Join(dir, "leyline"), got, "must not return the wrong-version binary")
	assert.Contains(t, err.Error(), leylineBinaryVersion, "error should name the pinned version")
}

func TestResolveBinary_AcceptsPinnedVersionOnPath(t *testing.T) {
	dir := t.TempDir()
	pin := strings.TrimPrefix(leylineBinaryVersion, "v")
	writeLeylineStub(t, dir, "leyline "+pin+" (open)") // pinned version on PATH
	hermeticEnv(t, dir)

	got, err := ResolveBinary(false)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "leyline"), got, "the pinned PATH leyline must be used")
}

func TestVerifyLeylineSHA256_RejectsBadContent(t *testing.T) {
	f := filepath.Join(t.TempDir(), "leyline")
	require.NoError(t, os.WriteFile(f, []byte("not the published leyline binary"), 0o755))
	// On a pinned platform this is a hash mismatch; on an unknown platform it's a
	// missing-pin error. Either way, an unverifiable binary must be refused.
	require.Error(t, verifyLeylineSHA256(f))
}

func mustMkdir(t *testing.T, base, name string) string {
	t.Helper()
	p := filepath.Join(base, name)
	require.NoError(t, os.MkdirAll(p, 0o755))
	return p
}
