package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubRunningJob makes the supervisor report a running daemon whose program is
// program, and records every supervisor command instead of running it.
func stubRunningJob(t *testing.T, program string) *[][]string {
	t.Helper()
	prevRun, prevQuery, prevAuto := runSupervisorCmd, querySupervisorCmd, daemonAgentAutoload
	t.Cleanup(func() { runSupervisorCmd, querySupervisorCmd, daemonAgentAutoload = prevRun, prevQuery, prevAuto })
	daemonAgentAutoload = true
	querySupervisorCmd = func(string, ...string) (string, error) {
		if runtime.GOOS == "linux" {
			return "LoadState=loaded\nActiveState=active\nExecStart={ path=" + program + " ; argv[]=" + program + " serve }\n", nil
		}
		return "mache = {\n\tstate = running\n\tprogram = " + program + "\n}\n", nil
	}
	var calls [][]string
	runSupervisorCmd = func(name string, args ...string) error {
		calls = append(calls, append([]string{filepath.Base(name)}, args...))
		return nil
	}
	stubDaemonSettle(t, true)
	withLaunchAgentPlist(t, true)
	return &calls
}

// Installing a copy somewhere the supervised daemon does not run from leaves
// the daemon alone. installverify's `mache install --bin-dir <temp>` restarted
// a developer's real daemon on every `task ci` (mache-6d5e62).
func TestInstall_LeavesTheDaemonAloneWhenItRunsAnotherBinary(t *testing.T) {
	supervised := filepath.Join(t.TempDir(), "mache")
	require.NoError(t, os.WriteFile(supervised, []byte("supervised"), 0o755))
	calls := stubRunningJob(t, supervised)

	var out bytes.Buffer
	restartIfSupervised(&out, filepath.Join(t.TempDir(), "mache"))
	assert.Empty(t, *calls, "the daemon was restarted for a binary it does not run")
	assert.Contains(t, out.String(), "left it running")
}

// Replacing the binary the daemon runs restarts it, as before.
func TestInstall_RestartsTheDaemonThatRunsTheReplacedBinary(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "mache")
	require.NoError(t, os.WriteFile(dst, []byte("new"), 0o755))
	calls := stubRunningJob(t, dst)

	var out bytes.Buffer
	restartIfSupervised(&out, dst)
	assert.NotEmpty(t, *calls, "the daemon running the replaced binary was not restarted")
	assert.NotContains(t, out.String(), "left it running")
}
