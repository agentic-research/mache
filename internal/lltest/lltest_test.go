package lltest

import (
	"bufio"
	"encoding/json"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sendLine dials the fake daemon, writes one JSON line, and decodes the
// single-line response — the exact protocol leyline.SocketClient speaks.
func sendLine(t *testing.T, sock string, req map[string]any) map[string]any {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	data, err := json.Marshal(req)
	require.NoError(t, err)
	_, err = conn.Write(append(data, '\n'))
	require.NoError(t, err)

	line, err := bufio.NewReader(conn).ReadBytes('\n')
	require.NoError(t, err)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(line, &resp))
	return resp
}

func TestFakeDaemon_RoundTripsHandlerResponse(t *testing.T) {
	sock := FakeDaemon(t, func(req map[string]any) any {
		return map[string]any{"ok": true, "echo": req["op"]}
	})

	resp := sendLine(t, sock, map[string]any{"op": "validate"})
	assert.Equal(t, true, resp["ok"])
	assert.Equal(t, "validate", resp["echo"])
}

func TestFakeDaemon_SurvivesProbeConnections(t *testing.T) {
	// DiscoverOrStart's liveness check dials and immediately closes; the
	// fake must keep serving afterwards.
	sock := FakeDaemon(t, func(map[string]any) any {
		return map[string]any{"ok": true}
	})

	probe, err := net.Dial("unix", sock)
	require.NoError(t, err)
	require.NoError(t, probe.Close())

	resp := sendLine(t, sock, map[string]any{"op": "anything"})
	assert.Equal(t, true, resp["ok"])
}

func TestFakeDaemon_ServesMultipleRequestsPerConnection(t *testing.T) {
	calls := 0
	sock := FakeDaemon(t, func(map[string]any) any {
		calls++
		return map[string]any{"n": calls}
	})

	conn, err := net.Dial("unix", sock)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	rd := bufio.NewReader(conn)

	for i := 1; i <= 2; i++ {
		_, err = conn.Write([]byte(`{"op":"x"}` + "\n"))
		require.NoError(t, err)
		line, err := rd.ReadBytes('\n')
		require.NoError(t, err)
		var resp map[string]any
		require.NoError(t, json.Unmarshal(line, &resp))
		assert.EqualValues(t, i, resp["n"])
	}
}
