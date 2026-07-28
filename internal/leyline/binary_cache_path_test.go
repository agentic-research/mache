package leyline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The namespaced path must carry the pin, or two builds collide again.
func TestPinnedCachePath_IsNamespacedByPin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p, err := pinnedCachePath()
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(p, "leyline-"+leylineBinaryVersion),
		"cache path must be namespaced by the pin, got %s", p)

	legacy, err := legacyCachePath()
	require.NoError(t, err)
	assert.NotEqual(t, legacy, p, "namespaced and legacy paths must differ")
	assert.True(t, strings.HasSuffix(legacy, "leyline"))
}

// stubLeyline writes a fake binary reporting the given version.
func stubLeyline(t *testing.T, path, version string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path,
		[]byte("#!/bin/sh\necho 'leyline "+version+" (open)'\n"), 0o755))
}

// The core regression: a DIFFERENT build's cached binary must not satisfy — nor
// be disturbed by — this build. Before namespacing, an installed mache pinning
// v0.10.2 and a branch pinning v0.10.3 overwrote one file on every run.
func TestResolveCachedPinned_IgnoresAnotherPinsBinary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	other := filepath.Join(home, ".mache", "bin", "leyline-v0.0.1")
	stubLeyline(t, other, "0.0.1")

	assert.Empty(t, resolveCachedPinned(),
		"another pin's cached binary must not be resolved as ours")
	_, err := os.Stat(other)
	assert.NoError(t, err, "and must not be removed — it belongs to that build")
}

// Our own namespaced binary is used when it matches.
func TestResolveCachedPinned_UsesNamespacedHit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p, err := pinnedCachePath()
	require.NoError(t, err)
	stubLeyline(t, p, strings.TrimPrefix(leylineBinaryVersion, "v"))
	assert.Equal(t, p, resolveCachedPinned())
}

// Migration: a legacy unversioned binary that already IS the pin is reused
// rather than re-downloaded.
func TestResolveCachedPinned_ReusesLegacyWhenItMatchesPin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacy, err := legacyCachePath()
	require.NoError(t, err)
	stubLeyline(t, legacy, strings.TrimPrefix(leylineBinaryVersion, "v"))
	assert.Equal(t, legacy, resolveCachedPinned(), "no reason to re-download a correct legacy copy")
}

// A legacy binary at the WRONG version is simply ignored — not an error, not
// deleted. It belongs to whichever build wrote it; we use our own namespace.
// This is what ends the thrash.
func TestResolveCachedPinned_IgnoresStaleLegacyWithoutTouchingIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacy, err := legacyCachePath()
	require.NoError(t, err)
	stubLeyline(t, legacy, "0.0.2")

	assert.Empty(t, resolveCachedPinned())
	before, err := os.ReadFile(legacy)
	require.NoError(t, err)
	assert.Contains(t, string(before), "0.0.2", "a foreign build's cache must be left alone")
}

// Namespaced wins over a legacy copy at the same version — deterministic
// precedence so behaviour does not depend on filesystem ordering.
func TestResolveCachedPinned_NamespacedTakesPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	v := strings.TrimPrefix(leylineBinaryVersion, "v")
	p, err := pinnedCachePath()
	require.NoError(t, err)
	legacy, err := legacyCachePath()
	require.NoError(t, err)
	stubLeyline(t, p, v)
	stubLeyline(t, legacy, v)
	assert.Equal(t, p, resolveCachedPinned())
}
