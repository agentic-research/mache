// Package testport reserves ephemeral TCP ports for tests that must choose
// one up front. `mache serve` rejects a bare ":0" and never reports the port
// it actually bound, so callers (installverify's client harness, the launchd
// lifecycle E2E) have to pick before launching. One definition, because two
// copies of this helper is exactly the duplication the smell gate flags.
package testport

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

// Free reserves a port by binding and immediately releasing it. The window
// between release and re-bind is a theoretical race, not a practical one on
// a test host.
func Free(t testing.TB) int {
	t.Helper()
	l, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}
