package projcfg

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMacheHomeDir_RefusesRealHomeUnderTest pins the hermeticity guard
// (mache-3e78d2): 106 of 118 entries in a developer's REAL
// ~/.mache/projects.json turned out to be test-run temp dirs, written by
// tests that exercised the registry without pointing HOME elsewhere. The
// guard turns that silent pollution into a loud, named failure.
func TestMacheHomeDir_RefusesRealHomeUnderTest(t *testing.T) {
	// This test binary started with the real HOME, so realHomeAtInit holds
	// it. Restore it for the failing direction.
	t.Setenv("HOME", realHomeAtInit)
	_, err := macheHomeDir()
	require.Error(t, err, "resolving the REAL home under `go test` must refuse")
	assert.Contains(t, err.Error(), "t.TempDir", "the error must name the fix")

	// The passing direction: a hermetic HOME resolves and creates ~/.mache.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	dir, err := macheHomeDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(tmp, ".mache"), dir)
	assert.DirExists(t, dir)
}

// TestRegisterProject_CannotTouchRealRegistry drives the guard through the
// public writer: with HOME unhermetic, RegisterProject must fail (not write);
// with HOME hermetic, it must succeed and the registry lands under the temp
// home — never the real one.
func TestRegisterProject_CannotTouchRealRegistry(t *testing.T) {
	t.Setenv("HOME", realHomeAtInit)
	_, err := RegisterProject(t.TempDir())
	require.Error(t, err, "an unhermetic registry write must be refused")

	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	token, err := RegisterProject("/some/project")
	require.NoError(t, err)
	require.NotEmpty(t, token)
	data, err := os.ReadFile(filepath.Join(tmp, ".mache", "projects.json"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "/some/project")
}
