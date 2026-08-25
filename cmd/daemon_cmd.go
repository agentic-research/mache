package cmd

import (
	"github.com/spf13/cobra"
)

// daemonCmd groups operations on the supervised mache HTTP daemon — the
// keepalive process `mache init --global` installs (launchd on macOS, systemd
// --user on Linux).
var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Operate the supervised mache HTTP daemon",
}

// daemonRestartCmd re-execs the supervised daemon.
//
// Exists because replacing the binary does not restart the process holding it:
// a supervisor keeps serving the inode it exec'd. Observed on a real machine
// after `task install` — the installed binary reported v0.20.0 while the
// daemon on localhost:7532 answered `initialize` as 0.19.0-10-g1c3d812, so
// every MCP session silently got pre-release code.
//
// Unlike the old restart-if-running no-op, this VERIFIES: it waits for the
// endpoint to answer and fails if it does not. See runDaemonVerb.
var daemonRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the supervised daemon so it serves the current binary",
	Long: "Restart the supervised mache HTTP daemon so it re-execs the binary currently " +
		"installed.\n\nReplacing the binary on disk does not restart a running supervisor — it " +
		"keeps serving the old process. Run this after installing a new build.\n\nWaits for the " +
		"daemon to answer again and fails if it does not, so a restart that did not take is not " +
		"reported as success.",
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runDaemonVerb(cmd.OutOrStdout(), verbRestart)
	},
}

// daemonStartCmd starts a daemon that is installed but not running.
//
// `restart` deliberately refuses to start one, which left NO first-party way
// to revive a dead daemon — the documented answer was to run raw `launchctl
// kickstart`, which is the gap this closes (mache-4421f7).
var daemonStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the supervised daemon",
	Long: "Start the supervised mache HTTP daemon.\n\nUse this when the daemon is installed " +
		"but not running. Installing it in the first place is `mache init --global`.\n\nWaits " +
		"for the daemon to answer and fails if it does not.",
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runDaemonVerb(cmd.OutOrStdout(), verbStart)
	},
}

// daemonStopCmd stops the supervised daemon and confirms it stopped.
//
// SIGTERM rather than unloading the job: mache's plist declares
// KeepAlive{SuccessfulExit:false}, so launchd resurrects only on a non-zero
// exit, and serve handles SIGTERM cleanly — so a stop sticks without leaving
// the job unloaded and a later `start` unable to find it.
var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the supervised daemon",
	Long: "Stop the supervised mache HTTP daemon.\n\nThe job stays installed, so `mache " +
		"daemon start` brings it back. Waits until the endpoint stops answering and fails if " +
		"it does not.",
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runDaemonVerb(cmd.OutOrStdout(), verbStop)
	},
}

// daemonStatusCmd reports the supervisor's view AND the endpoint's, because
// they disagree in the case worth catching: a job the supervisor still
// considers loaded whose process is gone.
var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Report whether the supervised daemon is installed, running, and answering",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return reportDaemonStatus(cmd.OutOrStdout())
	},
}

func init() {
	daemonCmd.AddCommand(daemonStatusCmd)
	daemonCmd.AddCommand(daemonStartCmd)
	daemonCmd.AddCommand(daemonStopCmd)
	daemonCmd.AddCommand(daemonRestartCmd)
	rootCmd.AddCommand(daemonCmd)
}
