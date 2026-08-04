package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectToken_DeterministicGivenSameSaltAndPath(t *testing.T) {
	salt := []byte("0123456789abcdef0123456789abcdef")[:32]
	a := projectToken(salt, "/Users/x/repo")
	b := projectToken(salt, "/Users/x/repo")
	assert.Equal(t, a, b)
}

func TestProjectToken_DiffersByPath(t *testing.T) {
	salt := []byte("0123456789abcdef0123456789abcdef")[:32]
	a := projectToken(salt, "/Users/x/repo-one")
	b := projectToken(salt, "/Users/x/repo-two")
	assert.NotEqual(t, a, b)
}

func TestProjectToken_DiffersBySalt(t *testing.T) {
	saltA := []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	saltB := []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	a := projectToken(saltA, "/Users/x/repo")
	b := projectToken(saltB, "/Users/x/repo")
	assert.NotEqual(t, a, b, "an attacker who doesn't know the local salt must not be able to reproduce the token from the path alone")
}

func TestLoadOrCreateProjectSalt_PersistsAcrossCalls(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	a, err := loadOrCreateProjectSalt()
	require.NoError(t, err)
	require.Len(t, a, 32)

	b, err := loadOrCreateProjectSalt()
	require.NoError(t, err)
	assert.Equal(t, a, b, "a second call must reuse the persisted salt, not mint a new one")
}

func TestLoadOrCreateProjectSalt_FilePermissionsAreRestrictive(t *testing.T) {
	if os.Getenv("GOOS") == "windows" || filepath.Separator == '\\' {
		t.Skip("POSIX permission bits only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	_, err := loadOrCreateProjectSalt()
	require.NoError(t, err)

	info, err := os.Stat(filepath.Join(home, ".mache", projectSaltFile))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the salt must not be world- or group-readable")
}

func TestRegisterProject_RoundTripsThroughResolveProjectToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	absPath := "/Users/x/some-project"
	token, err := registerProject(absPath)
	require.NoError(t, err)
	require.NotEmpty(t, token)

	got, ok := resolveProjectToken(token)
	require.True(t, ok)
	assert.Equal(t, absPath, got)
}

func TestRegisterProject_IsIdempotentForTheSamePath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	absPath := "/Users/x/some-project"
	tokenA, err := registerProject(absPath)
	require.NoError(t, err)
	tokenB, err := registerProject(absPath)
	require.NoError(t, err)

	assert.Equal(t, tokenA, tokenB, "re-running mache init in the same directory must reproduce the same token, not orphan the URL already written into client configs")
}

func TestRegisterProject_PreservesOtherEntries(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	tokenA, err := registerProject("/Users/x/project-a")
	require.NoError(t, err)
	_, err = registerProject("/Users/x/project-b")
	require.NoError(t, err)

	// project-a must still resolve after project-b was registered.
	got, ok := resolveProjectToken(tokenA)
	require.True(t, ok)
	assert.Equal(t, "/Users/x/project-a", got)
}

func TestResolveProjectToken_UnknownTokenMisses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	_, ok := resolveProjectToken("not-a-real-token")
	assert.False(t, ok, "a guessed or stale token must miss, not fall back to any path")
}

func TestResolveProjectToken_EmptyRegistryMisses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// No registerProject call at all — the registry file doesn't exist yet.
	_, ok := resolveProjectToken("anything")
	assert.False(t, ok)
}
