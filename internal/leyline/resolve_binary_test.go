package leyline

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pinnedVersionLine is the `leyline --version` line a stub must print to be
// accepted as the pinned version (mache-46af85).
func pinnedVersionLine() string {
	return "leyline " + strings.TrimPrefix(leylineBinaryVersion, "v") + " (open)"
}

// TestResolveBinary_FindsOnPath returns a PATH leyline WHEN it reports the
// pinned version, without downloading, even when download is allowed.
func TestResolveBinary_FindsOnPath(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "leyline")
	require.NoError(t, os.WriteFile(fake, []byte("#!/bin/sh\necho '"+pinnedVersionLine()+"'\n"), 0o755))
	t.Setenv("PATH", dir)
	t.Setenv("HOME", t.TempDir())

	got, err := ResolveBinary(true)
	require.NoError(t, err)
	assert.Equal(t, fake, got)
}

// TestResolveBinary_FindsBundled falls back to ~/.mache/bin/leyline (reporting
// the pinned version) when not on PATH, still without downloading.
func TestResolveBinary_FindsBundled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PATH", t.TempDir()) // no leyline on PATH
	t.Setenv("HOME", home)
	bundled := filepath.Join(home, ".mache", "bin", "leyline")
	require.NoError(t, os.MkdirAll(filepath.Dir(bundled), 0o755))
	require.NoError(t, os.WriteFile(bundled, []byte("#!/bin/sh\necho '"+pinnedVersionLine()+"'\n"), 0o755))

	got, err := ResolveBinary(true)
	require.NoError(t, err)
	assert.Equal(t, bundled, got)
}

// TestResolveBinary_NoDownloadWhenDisallowed errors (no network) when no pinned
// leyline is present and allowDownload is false; the error names the pin.
func TestResolveBinary_NoDownloadWhenDisallowed(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	_, err := ResolveBinary(false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), leylineBinaryVersion, "error should name the pinned version it needs")
}

// TestResolveBinary_NoLeylineEnvSkipsDownload honors MACHE_NO_LEYLINE.
func TestResolveBinary_NoLeylineEnvSkipsDownload(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MACHE_NO_LEYLINE", "1")

	_, err := ResolveBinary(true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MACHE_NO_LEYLINE")
}

// TestResolveBinary_DownloadsWhenMissing fetches the pinned binary when absent
// and allowed, and SHA-verifies the download. Hermetic: swaps the release URL
// for an httptest server AND injects the served content's SHA-256 into the pin
// map for this platform, so no network is touched and the supply-chain check
// exercises its accept path.
func TestResolveBinary_DownloadsWhenMissing(t *testing.T) {
	content := []byte("#!/bin/sh\necho fake-leyline\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	orig := leylineReleaseURLTemplate
	leylineReleaseURLTemplate = srv.URL + "/%s/%s"
	defer func() { leylineReleaseURLTemplate = orig }()

	pinLeylineSHA(t, content) // accept the served payload's SHA hermetically

	home := t.TempDir()
	t.Setenv("PATH", t.TempDir()) // no leyline on PATH
	t.Setenv("HOME", home)
	t.Setenv("MACHE_NO_LEYLINE", "") // ensure not opted out

	got, err := ResolveBinary(true)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".mache", "bin", "leyline"), got)

	fi, err := os.Stat(got)
	require.NoError(t, err, "downloaded leyline must exist")
	assert.NotZero(t, fi.Mode()&0o111, "downloaded leyline must be executable")
}

// TestResolveBinary_RejectsTamperedDownload proves the SHA-pin blocks a download
// whose bytes don't match the pinned release (supply-chain).
func TestResolveBinary_RejectsTamperedDownload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("tampered payload not matching the pinned SHA"))
	}))
	defer srv.Close()
	orig := leylineReleaseURLTemplate
	leylineReleaseURLTemplate = srv.URL + "/%s/%s"
	defer func() { leylineReleaseURLTemplate = orig }()

	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MACHE_NO_LEYLINE", "")

	_, err := ResolveBinary(true)
	require.Error(t, err, "a download not matching the pinned SHA-256 must be rejected")
	assert.Contains(t, err.Error(), "SHA-256")
}
