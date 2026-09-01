package projcfg

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectToken(t *testing.T) {
	saltA := []byte("0123456789abcdef0123456789abcdef")[:32]
	saltB := []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	cases := []struct {
		name       string
		saltX      []byte
		pathX      string
		saltY      []byte
		pathY      string
		wantEqual  bool
		ineqReason string
	}{
		{
			name: "same salt and path are deterministic", saltX: saltA, pathX: "/Users/x/repo",
			saltY: saltA, pathY: "/Users/x/repo", wantEqual: true,
		},
		{
			name: "differs by path", saltX: saltA, pathX: "/Users/x/repo-one",
			saltY: saltA, pathY: "/Users/x/repo-two", wantEqual: false,
		},
		{
			name: "differs by salt", saltX: saltA, pathX: "/Users/x/repo",
			saltY: saltB, pathY: "/Users/x/repo", wantEqual: false,
			ineqReason: "an attacker who doesn't know the local salt must not be able to reproduce the token from the path alone",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := projectToken(tc.saltX, tc.pathX)
			b := projectToken(tc.saltY, tc.pathY)
			if tc.wantEqual {
				assert.Equal(t, a, b)
			} else {
				assert.NotEqual(t, a, b, tc.ineqReason)
			}
		})
	}
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
	token, err := RegisterProject(absPath)
	require.NoError(t, err)
	require.NotEmpty(t, token)

	got, ok := ResolveProjectToken(token)
	require.True(t, ok)
	assert.Equal(t, absPath, got)
}

func TestRegisterProject_IsIdempotentForTheSamePath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	absPath := "/Users/x/some-project"
	tokenA, err := RegisterProject(absPath)
	require.NoError(t, err)
	tokenB, err := RegisterProject(absPath)
	require.NoError(t, err)

	assert.Equal(t, tokenA, tokenB, "re-running mache init in the same directory must reproduce the same token, not orphan the URL already written into client configs")
}

func TestRegisterProject_PreservesOtherEntries(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	tokenA, err := RegisterProject("/Users/x/project-a")
	require.NoError(t, err)
	_, err = RegisterProject("/Users/x/project-b")
	require.NoError(t, err)

	// project-a must still resolve after project-b was registered.
	got, ok := ResolveProjectToken(tokenA)
	require.True(t, ok)
	assert.Equal(t, "/Users/x/project-a", got)
}

func TestResolveProjectToken_UnknownTokenMisses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	_, ok := ResolveProjectToken("not-a-real-token")
	assert.False(t, ok, "a guessed or stale token must miss, not fall back to any path")
}

func TestResolveProjectToken_EmptyRegistryMisses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// No RegisterProject call at all — the registry file doesn't exist yet.
	_, ok := ResolveProjectToken("anything")
	assert.False(t, ok)
}
