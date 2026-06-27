package cmd

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Canonical MCP transport endpoint. mache serves Streamable HTTP on this
// address (serve.go default), and onboarding (`mache init`) registers clients
// against it. stdio is a deliberate escape hatch (`mache serve --stdio`) for
// CI / sandbox / headless use — it is never registered. See ADR-0022.
const (
	macheHTTPListen  = "localhost:7532"
	macheHTTPURL     = "http://localhost:7532/mcp"
	launchAgentLabel = "com.agentic-research.mache"
)

// daemonAgentAutoload gates the launchctl/systemctl load step. Tests set it
// false to exercise plist/unit writing without a real supervisor side effect.
var daemonAgentAutoload = true

// logf writes a progress line, discarding the write error (best-effort output).
func logf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

// xmlText escapes s for use as plist <string> element text. os.Executable() can
// resolve to a path with XML-significant characters (&, <, >) or quotes — e.g.
// an app bundle under "/Applications/Mache Tools/" — which would otherwise
// produce a malformed plist.
func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// systemdQuote double-quotes s for a systemd ExecStart argument so a binary
// path containing spaces (or quotes/backslashes) is treated as one argument
// rather than split on whitespace. systemd honors \" and \\ inside double
// quotes.
func systemdQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// launchAgentPlist renders the macOS LaunchAgent plist that keepalives the
// shared mache HTTP daemon, so the endpoint registered by `mache init` is
// answerable without anyone running `mache serve` by hand.
//
// KeepAlive only restarts on failure (SuccessfulExit=false) and ThrottleInterval
// guards against a tight crash-loop if the daemon can't start (e.g. stale
// ~/.mache state — mache-823d91). Pure function so it can be unit-tested.
func launchAgentPlist(binPath, logPath string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>serve</string>
		<string>--http</string>
		<string>%s</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, launchAgentLabel, xmlText(binPath), macheHTTPListen, xmlText(logPath), xmlText(logPath))
}

// systemdUserUnit renders the Linux systemd --user service that keepalives the
// shared mache HTTP daemon. Pure function so it can be unit-tested.
func systemdUserUnit(binPath string) string {
	return fmt.Sprintf(`[Unit]
Description=mache MCP HTTP daemon (Streamable HTTP on %s)
After=network.target

[Service]
ExecStart=%s serve --http %s
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
`, macheHTTPListen, systemdQuote(binPath), macheHTTPListen)
}

// installDaemonAgent writes and loads a per-user supervisor that keeps the
// shared mache HTTP daemon alive, so the canonical endpoint is always
// answerable. Best-effort: it reports what it did but never fails init — a
// user can always run `mache serve --http` by hand. Idempotent.
func installDaemonAgent(w io.Writer, binPath string) {
	switch runtime.GOOS {
	case "darwin":
		installLaunchAgent(w, binPath)
	case "linux":
		installSystemdUnit(w, binPath)
	default:
		logf(w, "  [daemon] no supervisor for %s — run `mache serve --http %s` to start the daemon.\n", runtime.GOOS, macheHTTPListen)
	}
}

func installLaunchAgent(w io.Writer, binPath string) {
	home, err := os.UserHomeDir()
	if err != nil {
		logf(w, "  [daemon] could not resolve home dir: %v — start manually: mache serve --http %s\n", err, macheHTTPListen)
		return
	}
	agentDir := filepath.Join(home, "Library", "LaunchAgents")
	logPath := filepath.Join(home, "Library", "Logs", "mache.log")
	plistPath := filepath.Join(agentDir, launchAgentLabel+".plist")

	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		logf(w, "  [daemon] could not create %s: %v\n", agentDir, err)
		return
	}
	if err := os.WriteFile(plistPath, []byte(launchAgentPlist(binPath, logPath)), 0o644); err != nil {
		logf(w, "  [daemon] could not write %s: %v\n", plistPath, err)
		return
	}
	logf(w, "  [daemon] wrote %s\n", plistPath)

	if !daemonAgentAutoload {
		return
	}
	// Reload: bootout (ignore failure if not loaded) then bootstrap.
	if launchctl, err := exec.LookPath("launchctl"); err == nil {
		target := fmt.Sprintf("gui/%d", os.Getuid())
		_ = exec.Command(launchctl, "bootout", target+"/"+launchAgentLabel).Run()
		if err := exec.Command(launchctl, "bootstrap", target, plistPath).Run(); err != nil {
			logf(w, "  [daemon] plist installed; load it with: launchctl bootstrap %s %s\n", target, plistPath)
			return
		}
		logf(w, "  [daemon] loaded — mache HTTP daemon will keepalive on %s\n", macheHTTPListen)
		return
	}
	logf(w, "  [daemon] plist installed; launchctl not found — load it after restart.\n")
}

func installSystemdUnit(w io.Writer, binPath string) {
	home, err := os.UserHomeDir()
	if err != nil {
		logf(w, "  [daemon] could not resolve home dir: %v — start manually: mache serve --http %s\n", err, macheHTTPListen)
		return
	}
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	unitPath := filepath.Join(unitDir, "mache.service")

	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		logf(w, "  [daemon] could not create %s: %v\n", unitDir, err)
		return
	}
	if err := os.WriteFile(unitPath, []byte(systemdUserUnit(binPath)), 0o644); err != nil {
		logf(w, "  [daemon] could not write %s: %v\n", unitPath, err)
		return
	}
	logf(w, "  [daemon] wrote %s\n", unitPath)

	if !daemonAgentAutoload {
		return
	}
	if systemctl, err := exec.LookPath("systemctl"); err == nil {
		_ = exec.Command(systemctl, "--user", "daemon-reload").Run()
		if err := exec.Command(systemctl, "--user", "enable", "--now", "mache.service").Run(); err != nil {
			logf(w, "  [daemon] unit installed; enable it with: systemctl --user enable --now mache.service\n")
			return
		}
		logf(w, "  [daemon] enabled — mache HTTP daemon will keepalive on %s\n", macheHTTPListen)
		return
	}
	logf(w, "  [daemon] unit installed; systemctl not found — enable it manually.\n")
}
