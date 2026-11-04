package leyline

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestWritableDaemon_RefusesWhatItCannotTarget: no root names no tree, and a
// daemon this process did not spawn (here, one reached by LEYLINE_SOCKET) has
// a --source mache cannot ask, so neither is written through.
func TestWritableDaemon_RefusesWhatItCannotTarget(t *testing.T) {
	_, err := WritableDaemon("")
	assert.ErrorIs(t, err, ErrNoWritableDaemon)

	t.Setenv(BinaryOverrideEnv, "")
	t.Setenv("LEYLINE_SOCKET", mockServer(t, func(map[string]any) map[string]any { return map[string]any{"ok": true} }))
	_, err = WritableDaemon(t.TempDir())
	assert.ErrorIs(t, err, ErrNoWritableDaemon)
	assert.Contains(t, err.Error(), "not started by this mache")
}
