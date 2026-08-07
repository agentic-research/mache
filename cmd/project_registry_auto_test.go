package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEnsureProjectRegistered_RegistersAnUnknownRoot is the behaviour that
// makes registration a byproduct of serving.
//
// Before this, registerProject had exactly one caller — `mache init` — so a
// daemon could serve a project indefinitely with ~/.mache/projects.json absent
// entirely, and every ?project= lookup necessarily missed. Found live: a
// shared daemon had been serving this repo for days with no registry at all.
func TestEnsureProjectRegistered_RegistersAnUnknownRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()

	require.True(t, ensureProjectRegistered(root), "an unknown root must be registered")

	reg, err := loadProjectRegistry()
	require.NoError(t, err)
	found := false
	for _, p := range reg {
		if p == root {
			found = true
		}
	}
	assert.True(t, found, "the registry must now resolve a token to %s", root)
}

// TestEnsureProjectRegistered_IsReadOnlyWhenAlreadyKnown keeps the steady
// state cheap. This runs on session resolution, which happens per tool call
// on an unmapped session — rewriting the registry every time would be constant
// pointless file churn.
func TestEnsureProjectRegistered_IsReadOnlyWhenAlreadyKnown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()

	require.True(t, ensureProjectRegistered(root))
	assert.False(t, ensureProjectRegistered(root),
		"a root already in the registry must not be rewritten")
}

// TestEnsureProjectRegistered_TokenResolvesBackToTheRoot closes the loop: the
// point of registering is that a LATER client — one that cannot answer
// roots/list — can resolve this project by token.
func TestEnsureProjectRegistered_TokenResolvesBackToTheRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	require.True(t, ensureProjectRegistered(root))

	reg, err := loadProjectRegistry()
	require.NoError(t, err)
	require.Len(t, reg, 1)
	var token string
	for tok := range reg {
		token = tok
	}

	got, ok := resolveProjectToken(token)
	require.True(t, ok, "the token the daemon just registered must resolve")
	assert.Equal(t, root, got)
}

// TestEnsureProjectRegistered_SurvivesAnUnwritableRegistry pins the
// non-fatal contract. Registration is an optimization for the NEXT client;
// serving this one is the actual job, so a read-only HOME must degrade to
// "not registered" rather than failing the session.
func TestEnsureProjectRegistered_SurvivesAnUnwritableRegistry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	macheDir := filepath.Join(home, ".mache")
	require.NoError(t, os.MkdirAll(macheDir, 0o755))
	require.NoError(t, os.Chmod(macheDir, 0o500)) // read+execute, no write
	t.Cleanup(func() { _ = os.Chmod(macheDir, 0o755) })

	assert.NotPanics(t, func() { ensureProjectRegistered(t.TempDir()) },
		"an unwritable registry must not take down a session that is otherwise fine")
}

// TestEnsureProjectRegistered_IgnoresEmptyRoot guards the degenerate input: a
// path-less shared daemon has no root to register, and must not write one.
func TestEnsureProjectRegistered_IgnoresEmptyRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	assert.False(t, ensureProjectRegistered(""))
}
