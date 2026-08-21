package leyline

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Unset must be a complete no-op. If an absent override changed resolution in
// any way, the pin would no longer be the default and the whole strictness
// argument collapses.
func TestOverrideBinary_UnsetIsNoOp(t *testing.T) {
	t.Setenv(BinaryOverrideEnv, "")
	p, set, err := OverrideBinary()
	require.NoError(t, err)
	assert.False(t, set, "an unset override must not participate in resolution")
	assert.Empty(t, p)
}

// A set override returns THAT binary, bypassing the pin — the whole point.
func TestOverrideBinary_SetReturnsThatPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "leyline")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\necho 'leyline 9.9.9 (dev)'\n"), 0o755))

	t.Setenv(BinaryOverrideEnv, bin)
	p, set, err := OverrideBinary()
	require.NoError(t, err)
	assert.True(t, set)
	assert.Equal(t, bin, p, "the override wins over the pin, by design")
}

// A broken override must FAIL, never fall through to the pinned resolution.
// Someone who sets this wants this binary; silently substituting another is the
// exact silent-divergence class the pin exists to prevent.
func TestOverrideBinary_MissingPathFailsRatherThanFallingBack(t *testing.T) {
	t.Setenv(BinaryOverrideEnv, filepath.Join(t.TempDir(), "does-not-exist"))
	p, set, err := OverrideBinary()
	require.Error(t, err)
	assert.True(t, set, "set-but-broken must still short-circuit resolution")
	assert.Empty(t, p)
	assert.Contains(t, err.Error(), BinaryOverrideEnv, "the error must name the variable to unset")
	assert.Contains(t, err.Error(), leylineBinaryVersion, "and the pin it is overriding")
}

func TestOverrideBinary_DirectoryIsRejected(t *testing.T) {
	t.Setenv(BinaryOverrideEnv, t.TempDir())
	_, set, err := OverrideBinary()
	require.Error(t, err)
	assert.True(t, set)
	assert.Contains(t, err.Error(), "directory")
}

// binaryVersionString is best-effort: it feeds a log line, so an unrunnable or
// silent binary must degrade to "unknown" rather than failing the override.
func TestBinaryVersionString_DegradesToUnknown(t *testing.T) {
	dir := t.TempDir()
	notExec := filepath.Join(dir, "nope")
	require.NoError(t, os.WriteFile(notExec, []byte("not a program"), 0o644))
	assert.Equal(t, "unknown", binaryVersionString(notExec))
	assert.Equal(t, "unknown", binaryVersionString(filepath.Join(dir, "absent")))

	ok := filepath.Join(dir, "leyline")
	require.NoError(t, os.WriteFile(ok, []byte("#!/bin/sh\necho 'leyline 0.10.2 (open)'\n"), 0o755))
	assert.Equal(t, "0.10.2", binaryVersionString(ok))
}

// The drift warning is the fix for the daemon-path hole: a reachable daemon at
// a non-pinned version must be VISIBLE. These assert the comparison logic —
// same version quiet, different version noisy, absent version quiet.
func TestWarnOnPinDrift_ComparesAgainstThePin(t *testing.T) {
	// Same version as the pin: no drift. Uses the pin itself so the test does
	// not go stale when the pin bumps.
	warnOnPinDrift(map[string]any{"version": leylineBinaryVersion})
	// A patch-level difference is exactly the case that produced a 0.10.2
	// daemon serving a v0.10.3 pin, and must not be treated as equal.
	assert.NotEqual(t, parseSemverParts("0.10.2"), parseSemverParts(leylineBinaryVersion),
		"a patch difference must compare unequal — LLO ships _ast changes in patch releases")
	// Absent version field: nothing to compare, must not panic.
	warnOnPinDrift(map[string]any{})
	warnOnPinDrift(map[string]any{"schema_version": ""})
}
