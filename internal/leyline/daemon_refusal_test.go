package leyline

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wireSchemaVersion is the schema_version real ley-line daemons report. It is
// deliberately NOT the binary version: v0.19.1 and v0.20.1 both report 0.18.1,
// because LLO bumps the public schema-client contract independently.
const wireSchemaVersion = "0.18.1"

// pinnedVersion is the pin as a daemon reports it, without the "v".
var pinnedVersion = strings.TrimPrefix(leylineBinaryVersion, "v")

// versionDaemon serves the leyline_version op in the shape ley-line actually
// sends (rs/ll-open/cli-lib/src/daemon/ops.rs op_leyline_version): the binary
// version under binary_version, beside a schema_version that differs from it.
//
// The fake used to answer {"version": v}, a key no daemon sends. That shape is
// why mache read the wrong field and every test still passed (mache-d446a4).
func versionDaemon(t *testing.T, binaryVersion string) string {
	t.Helper()
	return rawMockServer(t, func(req map[string]any) map[string]any {
		if req["op"] == "leyline_version" {
			return versionResponse(binaryVersion)
		}
		return map[string]any{"ok": true}
	})
}

// versionResponse is lltest.VersionReply, which this package's tests cannot
// import (lltest imports leyline).
func versionResponse(binaryVersion string) map[string]any {
	return map[string]any{
		"ok":                true,
		"binary_version":    binaryVersion,
		"schema_version":    wireSchemaVersion,
		"wire_format_major": float64(1),
		"compat_min":        "0.4.1",
	}
}

// TestVerifyDaemon_RefusesAnotherRelease is mache-a2edb2. A daemon mache did
// not spawn was never held to the pin ResolveBinary enforces, and LLO changes
// the projection and the write wire between releases, so serving through it
// answers with the wrong shapes while the wire handshake passes. It is refused,
// and the refusal names both versions so the user knows what to restart.
func TestVerifyDaemon_RefusesAnotherRelease(t *testing.T) {
	t.Setenv(BinaryOverrideEnv, "")
	sock := versionDaemon(t, "0.20.5")

	err := verifyDaemon(sock)

	require.ErrorIs(t, err, ErrDaemonNotPinned)
	assert.Contains(t, err.Error(), "is v0.20.5", "must name what the daemon reported")
	assert.Contains(t, err.Error(), "mache needs "+leylineBinaryVersion, "and what mache needs")
	assert.Contains(t, err.Error(), sock, "and which daemon to stop")
}

// TestVerifyDaemon_APatchReleaseIsAnotherRelease: LLO ships _ast schema changes
// in patch releases, which is how a 0.10.2 daemon once served a v0.10.3 pin.
func TestVerifyDaemon_APatchReleaseIsAnotherRelease(t *testing.T) {
	t.Setenv(BinaryOverrideEnv, "")
	p := parseSemverParts(pinnedVersion)
	sock := versionDaemon(t, fmt.Sprintf("%d.%d.%d", p[0], p[1], p[2]+1))

	require.ErrorIs(t, verifyDaemon(sock), ErrDaemonNotPinned)
}

// TestVerifyDaemon_ServesThePin: the daemon mache would have spawned is served.
// Its schema_version differs from the binary pin, as every pin from v0.19.1 on
// does; comparing that field told users to stop the daemon mache had just
// started (the bug the release parity run found).
func TestVerifyDaemon_ServesThePin(t *testing.T) {
	t.Setenv(BinaryOverrideEnv, "")
	require.NotEqual(t, wireSchemaVersion, pinnedVersion,
		"this test needs a pin whose binary and schema versions differ")

	assert.NoError(t, verifyDaemon(versionDaemon(t, pinnedVersion)))
	assert.NoError(t, verifyDaemon(versionDaemon(t, leylineBinaryVersion)), "a leading v is the same release")
}

// TestVerifyDaemon_RefusesADaemonThatCannotSayWhichReleaseItIs: no binary
// version, an error answer to the op, or a dead socket all fail closed. Falling
// back to schema_version would compare a different quantity, and a daemon that
// predates leyline_version predates every release mache could pin.
func TestVerifyDaemon_RefusesADaemonThatCannotSayWhichReleaseItIs(t *testing.T) {
	t.Setenv(BinaryOverrideEnv, "")
	cases := map[string]string{
		"no binary_version": rawMockServer(t, func(map[string]any) map[string]any {
			return map[string]any{"ok": true, "schema_version": wireSchemaVersion}
		}),
		"op unknown": rawMockServer(t, func(map[string]any) map[string]any {
			return map[string]any{"ok": false, "error": "unknown op"}
		}),
		"nothing listening": "/tmp/definitely-not-a-socket-" + t.Name(),
	}
	for name, sock := range cases {
		t.Run(name, func(t *testing.T) {
			assert.ErrorIs(t, verifyDaemon(sock), ErrDaemonNotPinned)
		})
	}
}

// TestVerifyDaemon_TheOverrideIsAChoice: MACHE_LEYLINE_BINARY names another
// leyline on purpose (a local LLO build, a release candidate), so its daemon is
// not refused for not being the pin. The wire check still applies.
func TestVerifyDaemon_TheOverrideIsAChoice(t *testing.T) {
	t.Setenv(BinaryOverrideEnv, "/some/leyline")

	assert.NoError(t, verifyDaemon(versionDaemon(t, "0.20.5")))
}

// TestDiscoverOrStart_RefusesAMismatchedDaemonWithoutSpawning: the refusal is
// on the adoption path every consumer goes through, and it neither adopts the
// daemon nor starts a second one beside it on the same arena.
func TestDiscoverOrStart_RefusesAMismatchedDaemonWithoutSpawning(t *testing.T) {
	t.Setenv(BinaryOverrideEnv, "")
	t.Setenv("LEYLINE_SOCKET", versionDaemon(t, "0.20.5"))

	sock, err := DiscoverOrStart()

	require.ErrorIs(t, err, ErrDaemonNotPinned)
	assert.Empty(t, sock)
	managed.mu.Lock()
	defer managed.mu.Unlock()
	assert.Empty(t, managed.sock, "a refused daemon must not be replaced by a spawned one")
}

// TestVerifyReachableDaemonVersion_RefusesAtServeStartup: serve's startup
// handshake applies the same refusal, so a mismatched daemon stops serve before
// it answers a single tool call.
func TestVerifyReachableDaemonVersion_RefusesAtServeStartup(t *testing.T) {
	t.Setenv(BinaryOverrideEnv, "")
	t.Setenv("LEYLINE_SOCKET", versionDaemon(t, "0.20.5"))

	err := VerifyReachableDaemonVersion()

	assert.True(t, errors.Is(err, ErrDaemonNotPinned), "got %v", err)
}

// TestAdoptedDaemonVersion_ReportsWhatTheDaemonSays backs `mache doctor`'s
// leyline-pin check, which previously reported the RESOLVED BINARY and said
// nothing about what was actually serving.
//
// Asking beats inferring: the cached binary FILE named leyline-v0.18.2 reports
// 0.18.1 when run. A check reading the resolved path would have said 0.18.2.
func TestAdoptedDaemonVersion_ReportsWhatTheDaemonSays(t *testing.T) {
	sock := versionDaemon(t, "0.19.1")
	t.Setenv("LEYLINE_SOCKET", sock)

	got, ok := AdoptedDaemonVersion()

	require.True(t, ok)
	assert.Equal(t, "0.19.1", got, "the binary version, not the %s schema version beside it", wireSchemaVersion)
}
