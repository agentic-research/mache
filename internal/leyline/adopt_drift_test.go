package leyline

import (
	"bytes"
	"log"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetDriftOnce lets each test exercise the once-guard from a clean slate.
// The guard is process-wide by design (one warning per process, not per op),
// so without this only the first test in the file would observe anything.
func resetDriftOnce(t *testing.T) {
	t.Helper()
	driftCheckOnce = sync.Once{}
	t.Cleanup(func() { driftCheckOnce = sync.Once{} })
}

// captureStdLog redirects the standard logger and returns what was written.
func captureStdLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	out, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(out); log.SetFlags(flags) })
	fn()
	return buf.String()
}

// wireSchemaVersion is the schema_version real ley-line daemons report. It is
// deliberately NOT the binary version: v0.19.1 and v0.20.1 both report 0.18.1,
// because LLO bumps the public schema-client contract independently.
const wireSchemaVersion = "0.18.1"

// versionDaemon serves the leyline_version op in the shape ley-line actually
// sends (rs/ll-open/cli-lib/src/daemon/ops.rs op_leyline_version): the binary
// version under binary_version, beside a schema_version that differs from it.
//
// The fake used to answer {"version": v}, a key no daemon sends. That shape is
// why mache read the wrong field and every test still passed (mache-d446a4).
func versionDaemon(t *testing.T, binaryVersion string) string {
	t.Helper()
	return mockServer(t, func(req map[string]any) map[string]any {
		if req["op"] == "leyline_version" {
			return map[string]any{
				"ok":                true,
				"binary_version":    binaryVersion,
				"schema_version":    wireSchemaVersion,
				"wire_format_major": float64(1),
				"compat_min":        "0.4.1",
			}
		}
		return map[string]any{"ok": true}
	})
}

// TestWarnIfAdoptedDaemonDrifts_FiresOnAMismatch covers the case that motivated
// moving this check (mache-233902).
//
// ResolveBinary enforces the exact pin on a daemon mache SPAWNS, but a daemon
// already listening is ADOPTED — the pin never applied to it. LLO ships _ast
// schema changes in patch releases, so an adopted near-miss produces a
// structurally different graph while every compatibility check reports green.
//
// Verified against a real stale daemon too: a cached leyline reporting 0.18.1
// adopted by this v0.19.0-pinned build warns, with the remedy.
func TestWarnIfAdoptedDaemonDrifts_FiresOnAMismatch(t *testing.T) {
	resetDriftOnce(t)
	sock := versionDaemon(t, "0.1.2")

	got := captureStdLog(t, func() { warnIfAdoptedDaemonDrifts(sock) })

	require.Contains(t, got, "WARNING")
	assert.Contains(t, got, "0.1.2", "must name what the daemon actually reported")
	assert.Contains(t, got, leylineBinaryVersion, "and what was expected")
}

// TestWarnIfAdoptedDaemonDrifts_SilentWhenPinned pins the other half. This runs
// on every adoption, so a warning on the correct case would be pure noise and
// would train readers to ignore the line that matters.
func TestWarnIfAdoptedDaemonDrifts_SilentWhenPinned(t *testing.T) {
	resetDriftOnce(t)
	sock := versionDaemon(t, strings.TrimPrefix(leylineBinaryVersion, "v"))

	got := captureStdLog(t, func() { warnIfAdoptedDaemonDrifts(sock) })

	assert.Empty(t, got, "a daemon matching the pin must say nothing")
}

// TestWarnIfAdoptedDaemonDrifts_SchemaVersionIsNotTheBinaryVersion is the bug
// the release parity run found. A daemon on exactly the pinned binary reports
// schema_version 0.18.1, and mache compared THAT to the binary pin, so it told
// every user to "stop the stale daemon" about the daemon it had just spawned.
func TestWarnIfAdoptedDaemonDrifts_SchemaVersionIsNotTheBinaryVersion(t *testing.T) {
	resetDriftOnce(t)
	pinned := strings.TrimPrefix(leylineBinaryVersion, "v")
	require.NotEqual(t, wireSchemaVersion, pinned,
		"this test needs a pin whose binary and schema versions differ, as every pin from v0.19.1 on does")
	sock := versionDaemon(t, pinned)

	got := captureStdLog(t, func() { warnIfAdoptedDaemonDrifts(sock) })

	assert.Empty(t, got, "a daemon on the pinned binary is not stale, whatever its schema version")
}

// TestWarnIfAdoptedDaemonDrifts_SilentWithoutABinaryVersion: a daemon that does
// not report its binary version cannot be judged, so it is not. Falling back to
// schema_version would compare a different quantity, which is the bug above.
func TestWarnIfAdoptedDaemonDrifts_SilentWithoutABinaryVersion(t *testing.T) {
	resetDriftOnce(t)
	sock := mockServer(t, func(req map[string]any) map[string]any {
		return map[string]any{"ok": true, "schema_version": wireSchemaVersion}
	})

	got := captureStdLog(t, func() { warnIfAdoptedDaemonDrifts(sock) })

	assert.Empty(t, got)
}

// TestWarnIfAdoptedDaemonDrifts_WarnsOnceNotPerCall is why the guard exists.
//
// DiscoverOrStart is called PER OPERATION by several consumers (write-back
// validation, the embed trigger, the LSP and semantic-search handlers), so an
// unguarded probe would add a socket round-trip to every call and repeat the
// same line until it is noise rather than a warning.
func TestWarnIfAdoptedDaemonDrifts_WarnsOnceNotPerCall(t *testing.T) {
	resetDriftOnce(t)
	sock := versionDaemon(t, "0.1.2")

	got := captureStdLog(t, func() {
		for range 5 {
			warnIfAdoptedDaemonDrifts(sock)
		}
	})

	assert.Equal(t, 1, strings.Count(got, "WARNING"),
		"five adoptions, one warning: this runs per-op and must not become noise")
}

// TestWarnIfAdoptedDaemonDrifts_SurvivesAnUnreachableDaemon: the probe is a
// diagnostic and must never block adoption. A daemon that predates the
// leyline_version op, or a socket that dies between the liveness check and the
// probe, is not something to fail on — it is something we cannot judge.
func TestWarnIfAdoptedDaemonDrifts_SurvivesAnUnreachableDaemon(t *testing.T) {
	resetDriftOnce(t)
	assert.NotPanics(t, func() {
		warnIfAdoptedDaemonDrifts("/tmp/definitely-not-a-socket-" + t.Name())
	})
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
