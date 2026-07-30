// `mache leyline path` / `mache leyline exec` — invoking the PINNED leyline.
//
// mache resolves leyline through one pin-checked chokepoint
// (leyline.ResolveBinary: MACHE_LEYLINE_BINARY → PATH → version-namespaced
// ~/.mache/bin cache → SHA-verified download, each tier accepted only on the
// EXACT pinned version). A human or agent typing bare `leyline` gets none of
// that — they get whatever PATH happens to hold.
//
// Measured on a machine that had run `task install` (bead mache-19326d):
//
//	~/.local/bin/leyline           0.10.3   ON PATH      <- what `leyline` meant
//	~/.mache/bin/leyline-v0.13.0   0.13.0   not on PATH  <- what mache actually used
//
// `~/.mache/bin` is not on PATH, so `leyline cdc enable --db x.db` typed at a
// shell ran a 0.10.3 parser against a db mache built with 0.13.0. Both binaries
// are named `leyline` and neither announces the mismatch. Since v0.13.0 shipped
// `leyline cdc enable` / `leyline cdc gc`, invoking leyline directly is a real
// workflow, so this is a real skew — the same defect class as mache-0acdf6
// (silently changing _ast shape between runs), relocated from the cache to PATH.
//
// These two subcommands make the version question UNASKABLE rather than
// answered-by-PATH-convention:
//
//	$(mache leyline path) cdc enable --db x.db     # shell interpolation
//	mache leyline exec -- cdc enable --db x.db     # direct invocation
//
// Deliberately NOT done here (both re-open shipped bugs):
//
//   - No write to ~/.mache/bin/leyline (the unversioned path). mache-0acdf6
//     made that path read-only-never-written because it was a shared mutable
//     resource concurrent mache builds overwrote; see binary_cache_path.go.
//     Nothing in this file writes to the cache at all — ResolveBinary owns
//     provisioning, and it only ever writes the pin-namespaced path.
//   - No PATH or shell-rc mutation. A shim dir on PATH just reintroduces
//     "which version does bare `leyline` mean" the moment the pin moves.
//
// Resolution failures are returned verbatim from ResolveBinary so there is one
// error vocabulary for "no pinned leyline", not two.
package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/agentic-research/mache/internal/leyline"
	"github.com/spf13/cobra"
)

// leylineExecExit terminates mache with leyline's exit status. Indirected
// through a var so the exec path can be exercised in-process by tests without
// killing the test binary; production always points at os.Exit.
var leylineExecExit = os.Exit

var leylineCmd = &cobra.Command{
	Use:   "leyline",
	Short: "Locate or run the exact ley-line-open build mache pins",
	Long: `Reach the pinned leyline binary without depending on PATH.

mache pins an exact ley-line-open release (` + leyline.BinaryVersion + `) and
caches it version-namespaced under ~/.mache/bin, which is NOT on PATH. A bare
` + "`leyline`" + ` in your shell therefore runs whatever copy PATH holds — often an
older one — against .db files mache parsed with the pin. Both are named
` + "`leyline`" + `; neither warns.

  mache leyline path                          # absolute path, nothing else
  $(mache leyline path) cdc enable --db x.db  # version-correct by construction
  mache leyline exec -- cdc enable --db x.db  # same, without the subshell

Both resolve through mache's pin-checked chokepoint, so a non-pinned leyline
earlier on PATH is reported and ignored rather than silently used. Neither
mutates PATH, your shell rc, or the unversioned ~/.mache/bin/leyline path.`,
}

var leylinePathCmd = &cobra.Command{
	Use:   "path",
	Short: "Print the absolute path of the pinned leyline binary (and nothing else)",
	Long: `Print the absolute path of the pinned leyline binary, with no other
stdout output, so it composes: $(mache leyline path) cdc enable --db x.db

Diagnostics (e.g. "leyline on PATH is not the pinned <ver> — ignoring it") go to
stderr, keeping stdout safe for command substitution. Provisions the pinned
binary if it is absent, unless MACHE_NO_LEYLINE is set.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		p, err := pinnedLeylinePath()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), p)
		return err
	},
}

var leylineExecCmd = &cobra.Command{
	Use:   "exec [--] [args...]",
	Short: "Run the pinned leyline binary, forwarding args, stdio and exit status",
	Long: `Run the pinned leyline binary as a transparent proxy: every argument is
forwarded verbatim, stdin/stdout/stderr are inherited, and mache exits with
leyline's exit status.

Flag parsing is disabled for this subcommand so leyline's own flags reach
leyline instead of being claimed by mache — including --help. A leading "--" is
accepted (and stripped) for callers who prefer to be explicit:

  mache leyline exec -- cdc enable --db x.db
  mache leyline exec cdc gc --db x.db`,
	DisableFlagParsing: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		code, err := runLeylineExec(args, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
		if err != nil {
			return err
		}
		if code != 0 {
			// Faithful proxying means mache's exit status IS leyline's;
			// returning an error here would flatten every failure to 1.
			leylineExecExit(code)
		}
		return nil
	},
}

func init() {
	leylineCmd.AddCommand(leylinePathCmd)
	leylineCmd.AddCommand(leylineExecCmd)
	rootCmd.AddCommand(leylineCmd)
}

// pinnedLeylinePath resolves the pinned leyline binary and returns it as an
// absolute path.
//
// ResolveBinary may return a PATH-relative result (exec.LookPath preserves a
// relative PATH entry), which would break the moment the caller substituted it
// into a command run from another directory — the exact "wrong binary, no
// warning" failure this command exists to remove. Absolutising is therefore
// part of the contract, not a nicety.
//
// Errors are returned unwrapped: ResolveBinary already names the pin and the
// remedy, and a second layer of wording would give the same condition two
// vocabularies.
func pinnedLeylinePath() (string, error) {
	p, err := leyline.ResolveBinary(true)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolve absolute path of pinned leyline %s: %w", p, err)
	}
	return abs, nil
}

// runLeylineExec runs the pinned leyline with args, wired to the given streams,
// and reports leyline's exit status. A non-zero status is a RESULT, not an
// error: only a failure to resolve or launch leyline returns err non-nil, so
// callers can distinguish "leyline ran and said no" from "there is no leyline".
func runLeylineExec(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	bin, err := pinnedLeylinePath()
	if err != nil {
		return 0, err
	}
	// With DisableFlagParsing cobra hands over the raw argv, "--" included.
	// Strip one leading separator so `exec -- cdc enable` and `exec cdc enable`
	// are the same invocation; any later "--" belongs to leyline.
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	c := exec.Command(bin, args...) //nolint:gosec // bin is the pin-verified binary; proxying is the point
	c.Stdin, c.Stdout, c.Stderr = stdin, stdout, stderr
	if err := c.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return 0, fmt.Errorf("run pinned leyline %s: %w", bin, err)
	}
	return 0, nil
}
