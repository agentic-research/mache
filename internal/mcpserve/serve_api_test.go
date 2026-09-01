package mcpserve

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServeCmd_ReturnsTheRegisteredCommand pins registration hook #3: what
// cmd/register.go wires onto rootCmd must be the live serve command with its
// flag set, not a clone that drops flags.
func TestServeCmd_ReturnsTheRegisteredCommand(t *testing.T) {
	c := ServeCmd()
	require.NotNil(t, c)
	assert.Equal(t, "serve [data-source]", c.Use)
	assert.Same(t, serveCmd, c, "must be the live command, not a clone")
	assert.NotNil(t, c.Flags().Lookup("http"), "flag set must ride along")
}

// TestSetVersion_RefusesEmpty pins the injection contract (stage-7 pattern):
// an empty version must be refused — a graph cache key with an empty version
// term silently collides across releases, the exact defect full-provenance
// keys exist to prevent.
func TestSetVersion_RefusesEmpty(t *testing.T) {
	prev := producerVersion
	t.Cleanup(func() { producerVersion = prev })

	SetVersion("7.7.7-injection-test")
	assert.Equal(t, "7.7.7-injection-test", serverVersion())

	SetVersion("")
	assert.Equal(t, "7.7.7-injection-test", serverVersion(),
		"empty injection must be refused, never announced or bound into keys")
}
