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

// daemonRestartCmd re-execs an already-running supervised daemon.
//
// Exists because replacing the binary does not restart the process holding it:
// a supervisor keeps serving the inode it exec'd. Observed on a real machine
// after `task install` — the installed binary reported v0.20.0 while the
// daemon on localhost:7532 answered `initialize` as 0.19.0-10-g1c3d812, so
// every MCP session silently got pre-release code.
//
// Restart-if-running, never start: see restartDaemonAgent. Running this on a
// machine with no supervised daemon is a successful no-op, not an error —
// there is nothing stale to fix, which is the outcome the caller wanted.
var daemonRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the supervised daemon so it serves the current binary",
	Long: "Restart the supervised mache HTTP daemon so it re-execs the binary currently " +
		"installed.\n\nReplacing the binary on disk does not restart a running supervisor — it " +
		"keeps serving the old process. Run this after installing a new build.\n\nIf no " +
		"supervised daemon is running this does nothing and succeeds: it never starts a daemon " +
		"you did not ask for (that is `mache init --global`).",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		restartDaemonAgent(cmd.OutOrStdout())
		return nil
	},
}

func init() {
	daemonCmd.AddCommand(daemonRestartCmd)
	rootCmd.AddCommand(daemonCmd)
}
