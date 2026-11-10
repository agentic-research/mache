package machehome

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDir_IsHomeDotMacheByDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(Env, "")
	dir, err := Dir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".mache"), dir)
	bin, err := BinPath("leyline-v1")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".mache", "bin", "leyline-v1"), bin)
}

// MACHE_HOME moves every state path and the binary cache together.
func TestDir_FollowsMacheHome(t *testing.T) {
	over := t.TempDir()
	t.Setenv(Env, over)
	dir, err := Dir()
	require.NoError(t, err)
	assert.Equal(t, over, dir)
	bin, err := BinPath("")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(over, "bin"), bin)
}

func TestDir_RefusesARelativeMacheHome(t *testing.T) {
	t.Setenv(Env, "rel/mache")
	_, err := Dir()
	assert.ErrorContains(t, err, "must be an absolute path")
}

// Under `go test`, the developer's real ~/.mache is refused for state, both
// through HOME and through MACHE_HOME pointing at it, while the shared binary
// cache stays readable (mache-6d5e62).
func TestDir_RefusesTheRealMacheUnderTest(t *testing.T) {
	realHome, err := os.UserHomeDir()
	require.NoError(t, err)
	require.NotEmpty(t, realDefault, "this test needs a home directory")

	t.Setenv("HOME", realHome)
	t.Setenv(Env, "")
	_, err = Dir()
	assert.ErrorContains(t, err, "t.TempDir")
	_, err = BinPath("")
	assert.NoError(t, err, "the binary cache is not guarded")

	t.Setenv(Env, realDefault)
	_, err = Dir()
	assert.ErrorContains(t, err, "t.TempDir", "MACHE_HOME naming the real directory is the real directory")
}
