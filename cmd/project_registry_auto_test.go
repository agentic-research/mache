package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
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

// TestEnsureProjectRegistered_ConcurrentRootsAllSurvive is the regression for
// the defect a cold review found in this feature's first cut.
//
// The registry is a read-modify-write over one JSON file, and a shared HTTP
// daemon resolves sessions concurrently — one goroutine per request. Without
// serialization every caller writes back a map it read BEFORE the others'
// inserts, so all but the last are silently dropped. Measured on the unlocked
// version: 50 concurrent roots left 1-3 registered, a 94-98% loss.
//
// Nothing crashed and nothing was corrupted — writeFileAtomic renames, so the
// file is never torn. It just quietly lost almost everything, which defeats
// the entire point: the token a later ?project= lookup needs was never
// written. That is why this test asserts an EXACT count rather than
// "non-empty" — a lost-update bug passes any weaker assertion.
func TestEnsureProjectRegistered_ConcurrentRootsAllSurvive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".mache"), 0o755))

	const n = 50
	roots := make([]string, n)
	for i := range roots {
		roots[i] = filepath.Join(home, fmt.Sprintf("proj%02d", i))
		require.NoError(t, os.MkdirAll(roots[i], 0o755))
	}

	var wg sync.WaitGroup
	for _, r := range roots {
		wg.Add(1)
		go func(p string) { defer wg.Done(); ensureProjectRegistered(p) }(r)
	}
	wg.Wait()

	reg, err := loadProjectRegistry()
	require.NoError(t, err)
	assert.Len(t, reg, n,
		"every concurrently-registered root must survive; a read-modify-write race silently drops all but the last")

	// Each token must still resolve to its own root — a race could also leave
	// the file self-consistent but with entries pointing at the wrong paths.
	for tok, path := range reg {
		got, ok := resolveProjectToken(tok)
		require.True(t, ok)
		assert.Equal(t, path, got)
	}
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

	// Assert the CONTRACT, not merely the absence of a panic. NotPanics alone
	// would also pass if the write had silently succeeded — on a filesystem
	// that ignores permission bits, as root, or after a refactor that created
	// the directory earlier. The claim is "degrades to not-registered", so
	// pin both halves: the return value AND that nothing was written.
	root := t.TempDir()
	assert.False(t, ensureProjectRegistered(root),
		"an unwritable registry must report that it did NOT register")

	entries, err := os.ReadDir(macheDir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), "projects.json",
			"nothing may be written to an unwritable registry directory")
	}
}

// TestEnsureProjectRegistered_IgnoresEmptyRoot guards the degenerate input: a
// path-less shared daemon has no root to register, and must not write one.
func TestEnsureProjectRegistered_IgnoresEmptyRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	assert.False(t, ensureProjectRegistered(""))
}
