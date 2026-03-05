package nfsmount

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildMountOpts_Darwin_ReadOnly(t *testing.T) {
	opts, err := BuildMountOpts("darwin", 12345, false, "")
	require.NoError(t, err)
	assert.Contains(t, opts, "port=12345")
	assert.Contains(t, opts, "mountport=12345")
	assert.Contains(t, opts, "noac")
	assert.Contains(t, opts, "rdonly")
	// No trailing comma
	assert.False(t, strings.HasSuffix(opts, ","))
}

func TestBuildMountOpts_Darwin_Writable(t *testing.T) {
	opts, err := BuildMountOpts("darwin", 9999, true, "")
	require.NoError(t, err)
	assert.NotContains(t, opts, "rdonly")
	assert.Contains(t, opts, "port=9999")
}

func TestBuildMountOpts_Linux_ReadOnly(t *testing.T) {
	opts, err := BuildMountOpts("linux", 8888, false, "")
	require.NoError(t, err)
	assert.Contains(t, opts, "ro")
	assert.Contains(t, opts, "nolock")
	assert.Contains(t, opts, "noac")
}

func TestBuildMountOpts_ExtraOpts_Appended(t *testing.T) {
	opts, err := BuildMountOpts("darwin", 5555, false, "rsize=32768,wsize=32768")
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(opts, "rsize=32768,wsize=32768"))
	// Extra opts come after the defaults
	assert.Contains(t, opts, "noac,rdonly,rsize=32768")
}

func TestBuildMountOpts_ExtraOpts_Empty(t *testing.T) {
	withExtra, _ := BuildMountOpts("darwin", 5555, false, "")
	withoutExtra, _ := BuildMountOpts("darwin", 5555, false, "")
	assert.Equal(t, withExtra, withoutExtra)
	assert.False(t, strings.HasSuffix(withExtra, ","))
}

func TestBuildMountOpts_UnsupportedOS(t *testing.T) {
	_, err := BuildMountOpts("windows", 1234, false, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported OS")
}
