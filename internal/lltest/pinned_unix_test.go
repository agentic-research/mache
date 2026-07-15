//go:build unix

package lltest

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStartPinnedDaemon_AnswersValidateOp(t *testing.T) {
	// Gated on the pinned binary (skips when absent, never downloads).
	// Pins the contract every consumer relies on: the spawned daemon is
	// private, live, and speaks the v0.7.8 validate wire (errors key
	// present even on a clean parse).
	sock := StartPinnedDaemon(t)

	resp := sendLine(t, sock, map[string]any{
		"op": "validate", "content": "package main\n", "language": "go",
	})
	assert.Equal(t, true, resp["ok"])
	errs, hasErrors := resp["errors"]
	assert.True(t, hasErrors, "v0.7.8 wire always includes the errors key")
	assert.Empty(t, errs)
}
