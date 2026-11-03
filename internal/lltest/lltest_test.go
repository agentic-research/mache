package lltest

import (
	"bufio"
	"encoding/json"
	"net"
	"testing"

	"github.com/agentic-research/mache/internal/leyline"
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

// TestFakeDaemon_IsThePinnedDaemon: the fake stands in for the daemon mache
// would run, so mache's own refusal (mache-a2edb2) must accept it, and the
// version op must not reach a handler written for other ops.
func TestFakeDaemon_IsThePinnedDaemon(t *testing.T) {
	t.Setenv(leyline.BinaryOverrideEnv, "")
	sock := FakeDaemon(t, func(req map[string]any) any {
		return map[string]any{"ok": true, "handled": req["op"]}
	})
	t.Setenv("LEYLINE_SOCKET", sock)

	require.NoError(t, leyline.VerifyReachableDaemonVersion(), "mache must adopt the fake")
	assert.Equal(t, mapOfJSON(t, VersionReply(leyline.PinnedBinaryVersion())),
		sendLine(t, sock, map[string]any{"op": "leyline_version"}))
	assert.Equal(t, "embed_status", sendLine(t, sock, map[string]any{"op": "embed_status"})["handled"],
		"every other op still goes to the handler")
}

// TestVersionReply_IsWhatMacheRefuses: a reply naming another release is
// refused by the same check, so the fake's answer is what decides adoption.
func TestVersionReply_IsWhatMacheRefuses(t *testing.T) {
	t.Setenv(leyline.BinaryOverrideEnv, "")
	sock := rawFakeDaemon(t, func(map[string]any) any { return VersionReply("0.20.5") })
	t.Setenv("LEYLINE_SOCKET", sock)

	assert.ErrorIs(t, leyline.VerifyReachableDaemonVersion(), leyline.ErrDaemonNotPinned)
}

// mapOfJSON is v as it reads back off the wire (numbers become float64).
func mapOfJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(data, &out))
	return out
}
