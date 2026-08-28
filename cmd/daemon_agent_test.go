package cmd

import (
	"bytes"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/agentic-research/mache/internal/projcfg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLaunchAgentPlist_ShapeAndArgs(t *testing.T) {
	plist := launchAgentPlist("/usr/local/bin/mache", "/Users/u", "/tmp/mache.log")

	assert.Contains(t, plist, "<?xml")
	assert.Contains(t, plist, launchAgentLabel)
	// HOME is baked so launchd cannot point the daemon at a different home
	// than the one the agent was installed for (and so the hermetic E2E's
	// isolated HOME actually isolates the supervised process).
	assert.Contains(t, plist, "<key>EnvironmentVariables</key>")
	assert.Contains(t, plist, "<key>HOME</key>")
	assert.Contains(t, plist, "<string>/Users/u</string>")
	assert.Contains(t, plist, "/usr/local/bin/mache")
	// Canonical transport: serve --http localhost:7532, never --stdio.
	assert.Contains(t, plist, "<string>serve</string>")
	assert.Contains(t, plist, "<string>--http</string>")
	assert.Contains(t, plist, projcfg.MacheHTTPListen)
	assert.NotContains(t, plist, "--stdio")
	// Crash-loop guard.
	assert.Contains(t, plist, "ThrottleInterval")
}

func TestSystemdUserUnit_ExecStart(t *testing.T) {
	unit := systemdUserUnit("/usr/local/bin/mache")

	// Path is double-quoted so systemd treats it as one argument.
	assert.Contains(t, unit, `ExecStart="/usr/local/bin/mache" serve --http `+projcfg.MacheHTTPListen)
	assert.Contains(t, unit, "Restart=on-failure")
	assert.NotContains(t, unit, "--stdio")
}

// TestLaunchAgentPlist_EscapesSpecialChars guards against a nonstandard install
// path (os.Executable can resolve under e.g. "/Applications/Mache Tools/")
// producing a malformed plist.
func TestLaunchAgentPlist_EscapesSpecialChars(t *testing.T) {
	bin := "/Applications/Mache & Tools/<m>ache"
	plist := launchAgentPlist(bin, "/Users/a&b", "/tmp/a&b<c>.log")

	// Must be well-formed XML end to end.
	dec := xml.NewDecoder(strings.NewReader(plist))
	for {
		_, err := dec.Token()
		if err == io.EOF {
			break
		}
		require.NoError(t, err, "rendered plist must be valid XML")
	}

	// XML-significant chars from the path are escaped, not raw.
	assert.Contains(t, plist, "&amp;")
	assert.Contains(t, plist, "&lt;m&gt;ache")
	assert.NotContains(t, plist, "<m>ache")
	// The space survives (legal in element text) so the path is preserved.
	assert.Contains(t, plist, "Mache &amp; Tools")
}

func TestSystemdUserUnit_QuotesSpacedPath(t *testing.T) {
	unit := systemdUserUnit("/Applications/Mache Tools/mache")
	assert.Contains(t, unit, `ExecStart="/Applications/Mache Tools/mache" serve --http `+projcfg.MacheHTTPListen)
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
	assert.Contains(t, string(data), projcfg.MacheHTTPListen)
	assert.True(t, strings.Contains(buf.String(), want), "output should mention the written path")
}
