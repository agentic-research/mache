package gitutil

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHermeticGitCommand(t *testing.T) {
	t.Setenv("GIT_DIR", "/tmp/poison.git")

	cmd := HermeticGitCommand("--version")
	assert.NotContains(t, cmd.Env, "GIT_DIR=/tmp/poison.git")
	require.NoError(t, cmd.Run())
}

func TestWithoutLocalEnv(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"GIT_DIR=/repo/.git",
		"GIT_INDEX_FILE=/repo/.git/index",
		"GIT_CONFIG_COUNT=2",
		"HOME=/tmp/home",
	}

	assert.Equal(t, []string{"PATH=/usr/bin", "HOME=/tmp/home"}, WithoutLocalEnv(env))
}
