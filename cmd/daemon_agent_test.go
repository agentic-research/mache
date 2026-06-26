package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLaunchAgentPlist_ShapeAndArgs(t *testing.T) {
	plist := launchAgentPlist("/usr/local/bin/mache", "/tmp/mache.log")

	assert.Contains(t, plist, "<?xml")
	assert.Contains(t, plist, launchAgentLabel)
	assert.Contains(t, plist, "/usr/local/bin/mache")
	// Canonical transport: serve --http localhost:7532, never --stdio.
	assert.Contains(t, plist, "<string>serve</string>")
	assert.Contains(t, plist, "<string>--http</string>")
	assert.Contains(t, plist, macheHTTPListen)
	assert.NotContains(t, plist, "--stdio")
	// Crash-loop guard.
	assert.Contains(t, plist, "ThrottleInterval")
}

func TestSystemdUserUnit_ExecStart(t *testing.T) {
	unit := systemdUserUnit("/usr/local/bin/mache")

	assert.Contains(t, unit, "ExecStart=/usr/local/bin/mache serve --http "+macheHTTPListen)
	assert.Contains(t, unit, "Restart=on-failure")
	assert.NotContains(t, unit, "--stdio")
}

func TestInstallDaemonAgent_WritesSupervisorFile(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("no supervisor on %s", runtime.GOOS)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	origAutoload := daemonAgentAutoload
	daemonAgentAutoload = false // write the file, don't run launchctl/systemctl
	t.Cleanup(func() { daemonAgentAutoload = origAutoload })

	buf := new(bytes.Buffer)
	installDaemonAgent(buf, "/usr/local/bin/mache")

	var want string
	switch runtime.GOOS {
	case "darwin":
		want = filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
	case "linux":
		want = filepath.Join(home, ".config", "systemd", "user", "mache.service")
	}
	data, err := os.ReadFile(want)
	require.NoError(t, err, "supervisor file should be written")
	assert.Contains(t, string(data), "serve")
	assert.Contains(t, string(data), macheHTTPListen)
	assert.True(t, strings.Contains(buf.String(), want), "output should mention the written path")
}
