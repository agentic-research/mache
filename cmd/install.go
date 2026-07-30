// `mache install` / `mache uninstall` — provisioning that ships INSIDE the
// binary.
//
// Until now provisioning lived only in the Taskfile: `task install` copies
// bin/mache to ~/.local/bin and `task leyline:ensure` fetches the pinned
// ley-line-open release. Both require a repo checkout. A user who installed
// from a GitHub release asset or a Homebrew tap has neither, so the shipped
// binary could not provision its own REQUIRED backend — leyline is mache's
// sole parser since ADR-0012 step 4, and without it `mache build` has nothing
// to project from.
//
// That gap is not theoretical. On the reporter's machine a stale leyline
// 0.10.3 sat on PATH at ~/.local/bin/leyline while the pin was v0.13.0, so
// `leyline cdc enable --db x.db` typed at a shell ran a parser three minors
// off from the one that built the .db. Both binaries are named `leyline`;
// neither warns (bead mache-19326d).
//
// # What this deliberately does NOT do
//
//   - It never writes ~/.mache/bin/leyline, the UNVERSIONED path. That was a
//     shared mutable resource concurrent mache builds overwrote, silently
//     changing _ast shape between runs, and mache-0acdf6 made it
//     read-only-never-written. Provisioning goes through
//     leyline.EnsureCachedBinary, which only ever writes the pin-namespaced
//     path; nothing here touches the cache directly.
//   - It never puts leyline on PATH, by symlink, shim or otherwise. A shim
//     reintroduces "which version does bare `leyline` mean" the moment the pin
//     moves. `mache leyline path` / `mache leyline exec` answer that question
//     without the ambiguity.
//   - It never edits your shell rc behind your back. rustup's posture: print
//     the line, let the human run it. --update-rc is an explicit opt-in and
//     writes one idempotent, marked block it can also remove.
//   - `uninstall` refuses to delete a Homebrew-managed mache. Removing a file
//     brew believes it owns leaves brew's manifest lying, and the next `brew
//     upgrade` silently restores it.
package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/agentic-research/mache/internal/leyline"
	"github.com/spf13/cobra"
)

// executablePath is indirected so tests can name the file being installed
// instead of copying the test binary. Production always points at
// os.Executable.
var executablePath = os.Executable

// rcBlockStart / rcBlockEnd delimit the single region of a shell rc file this
// command owns. Marked rather than appended blindly so --update-rc is
// idempotent and `uninstall --update-rc` can remove exactly what it added,
// leaving anything a human wrote alone.
const (
	rcBlockStart = "# >>> mache install >>>"
	rcBlockEnd   = "# <<< mache install <<<"
)

type installOpts struct {
	binDir      string
	updateRC    bool
	dryRun      bool
	skipLeyline bool
	keepLeyline bool
}

var installFlags installOpts

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Install this mache binary and provision the exact ley-line-open release it pins",
	Long: `Install this mache binary into a bin directory and provision the exact
ley-line-open release it pins (` + leyline.BinaryVersion + `).

leyline is mache's sole parser, and mache accepts only the pinned version —
LLO ships _ast schema changes in PATCH releases, so a near-miss silently
produces a different projection. Provisioning caches it version-namespaced
under ~/.mache/bin, which is NOT put on PATH: reach it with
` + "`mache leyline path`" + ` or ` + "`mache leyline exec`" + `, which resolve
through mache's pin-checked chokepoint rather than through PATH order.

Your shell rc is not modified unless you pass --update-rc; otherwise the line
to add is printed for you to run.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runInstall(cmd.OutOrStdout(), installFlags)
	},
}

var uninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove what `mache install` created, and report what was removed",
	Long: `Remove the mache binary from the bin directory and the pinned leyline from
mache's cache, reporting each path.

Only THIS build's pinned leyline is removed: the cache is namespaced by version
so other mache builds' binaries live in their own files, and deleting them
would force those builds to re-download. Pass --keep-leyline to leave the cache
untouched entirely.

A Homebrew-managed mache is refused rather than deleted — brew's manifest would
still claim the file, and the next upgrade would silently restore it. Pass
--update-rc to also remove the PATH block ` + "`install --update-rc`" + ` added.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runUninstall(cmd.OutOrStdout(), installFlags)
	},
}

func init() {
	for _, c := range []*cobra.Command{installCmd, uninstallCmd} {
		c.Flags().StringVar(&installFlags.binDir, "bin-dir", "",
			"Directory to install into (default ~/.local/bin)")
		c.Flags().BoolVar(&installFlags.dryRun, "dry-run", false,
			"Report what would change without changing anything")
		c.Flags().BoolVar(&installFlags.updateRC, "update-rc", false,
			"Also add (or, on uninstall, remove) the PATH line in your shell rc")
	}
	installCmd.Flags().BoolVar(&installFlags.skipLeyline, "no-leyline", false,
		"Skip provisioning the pinned ley-line-open release")
	uninstallCmd.Flags().BoolVar(&installFlags.keepLeyline, "keep-leyline", false,
		"Leave the cached pinned leyline in place")

	rootCmd.AddCommand(installCmd)
	rootCmd.AddCommand(uninstallCmd)
}

// resolveBinDir returns the directory to install into, defaulting to
// ~/.local/bin — the same location `task install` uses, so the two agree
// rather than each owning a different copy.
func resolveBinDir(opts installOpts) (string, error) {
	if opts.binDir != "" {
		return filepath.Abs(opts.binDir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, ".local", "bin"), nil
}

// homebrewCellar reports the Cellar path backing target, and whether target is
// Homebrew-managed at all.
//
// Detection is structural, not a `brew --prefix` shell-out: every
// brew-installed binary in a prefix bin/ is a symlink into
// <prefix>/Cellar/<formula>/<version>/…, so a resolved path containing a
// "Cellar" element IS the marker, and it works with no brew on PATH and no
// assumption about where the prefix lives (/usr/local, /opt/homebrew, or a
// custom HOMEBREW_PREFIX).
func homebrewCellar(target string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", false
	}
	for _, part := range strings.Split(resolved, string(os.PathSeparator)) {
		if part == "Cellar" {
			return resolved, true
		}
	}
	return "", false
}

// sameFile reports whether a and b are the same file on disk, so installing
// over yourself is recognised rather than truncating the running binary's
// source.
func sameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

// copyExecutable writes src to dst via a temp file in dst's directory and one
// rename. The rename is what makes it safe to overwrite a mache that is
// currently running: the old inode survives for anyone holding it open, where
// writing in place would corrupt a live process.
func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(dst), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".mache-install-*")
	if err != nil {
		return fmt.Errorf("create temp file next to %s: %w", dst, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename succeeds

	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("copy to %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("install to %s: %w", dst, err)
	}
	return nil
}

func runInstall(out io.Writer, opts installOpts) error {
	self, err := executablePath()
	if err != nil {
		return fmt.Errorf("cannot determine the running mache binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	binDir, err := resolveBinDir(opts)
	if err != nil {
		return err
	}
	dst := filepath.Join(binDir, "mache")

	if cellar, brewed := homebrewCellar(dst); brewed {
		return fmt.Errorf(
			"%s is Homebrew-managed (resolves to %s) — installing over it would leave brew's "+
				"manifest claiming a file it no longer wrote, and the next `brew upgrade` would "+
				"silently restore brew's copy. Run `brew uninstall mache` first, or install "+
				"elsewhere with --bin-dir", dst, cellar)
	}

	switch {
	case sameFile(self, dst):
		logf(out, "mache is already installed at %s (this is the running binary)\n", dst)
	case opts.dryRun:
		logf(out, "would install %s -> %s\n", self, dst)
	default:
		if err := copyExecutable(self, dst); err != nil {
			return err
		}
		logf(out, "installed %s -> %s\n", self, dst)
	}

	if err := provisionLeyline(out, opts); err != nil {
		return err
	}
	return reportPATH(out, binDir, opts)
}

// provisionLeyline fetches the pinned release into the version-namespaced
// cache. EnsureCachedBinary is the chokepoint that owns this: it verifies the
// download's SHA-256 against the pinned digest and writes ONLY
// ~/.mache/bin/leyline-<pin>. Nothing here writes the cache itself, which is
// how the unversioned path stays never-written (mache-0acdf6).
func provisionLeyline(out io.Writer, opts installOpts) error {
	if opts.skipLeyline {
		logf(out, "skipping leyline provisioning (--no-leyline); mache pins %s\n", leyline.BinaryVersion)
		return nil
	}
	if opts.dryRun {
		logf(out, "would provision the pinned leyline %s into the version-namespaced cache\n",
			leyline.BinaryVersion)
		return nil
	}
	path, err := leyline.EnsureCachedBinary()
	if err != nil {
		return fmt.Errorf("provision leyline %s: %w", leyline.BinaryVersion, err)
	}
	logf(out, "leyline %s ready at %s\n", leyline.BinaryVersion, path)
	logf(out, "  run it version-correctly with: mache leyline exec -- <args>\n")
	return nil
}

// onPATH reports whether dir is already an entry of PATH. Compared as resolved
// paths so ~/.local/bin and /Users/x/.local/bin are the same answer.
func onPATH(dir string) bool {
	want := resolveOrSelf(dir)
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		if entry == "" {
			continue
		}
		if resolveOrSelf(entry) == want {
			return true
		}
	}
	return false
}

func resolveOrSelf(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// exportLine is the shell line that puts dir on PATH. Kept in one place so
// what is printed and what --update-rc writes cannot drift.
func exportLine(dir string) string {
	return fmt.Sprintf("export PATH=%q:$PATH", dir)
}

// reportPATH prints the export line when the bin dir is not on PATH, and
// writes it only under --update-rc. Printing rather than writing is the
// default on purpose: a tool that edits a shell rc unasked is a tool people
// stop trusting with their home directory.
func reportPATH(out io.Writer, binDir string, opts installOpts) error {
	if onPATH(binDir) {
		logf(out, "%s is already on PATH\n", binDir)
		return nil
	}
	line := exportLine(binDir)
	if !opts.updateRC {
		logf(out, "\n%s is not on PATH. Add it:\n\n    %s\n\n"+
			"Put that in your shell rc, or re-run with --update-rc to have mache add it.\n",
			binDir, line)
		return nil
	}
	rc, err := shellRCPath()
	if err != nil {
		return err
	}
	if opts.dryRun {
		logf(out, "would add the PATH block to %s\n", rc)
		return nil
	}
	if err := writeRCBlock(rc, line); err != nil {
		return err
	}
	logf(out, "added the PATH block to %s — open a new shell or `source %s`\n", rc, rc)
	return nil
}

// shellRCPath picks the rc file for the current $SHELL. Only the shells whose
// rc syntax is actually known are supported; guessing at an unknown shell's
// syntax would write a line that silently does nothing.
func shellRCPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	switch shell := filepath.Base(os.Getenv("SHELL")); shell {
	case "zsh":
		return filepath.Join(home, ".zshrc"), nil
	case "bash":
		return filepath.Join(home, ".bashrc"), nil
	case "sh", "ksh", "dash":
		return filepath.Join(home, ".profile"), nil
	default:
		return "", fmt.Errorf(
			"--update-rc does not know where %q keeps its rc file (SHELL=%q); add this line yourself:\n    %s",
			shell, os.Getenv("SHELL"), exportLine("<bin-dir>"))
	}
}

// writeRCBlock replaces (or appends) the single marked block this command
// owns. Replacing rather than appending is what makes repeated installs
// idempotent instead of growing the rc file one stanza per run.
func writeRCBlock(rc, line string) error {
	existing, err := os.ReadFile(rc)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", rc, err)
	}
	block := rcBlockStart + "\n" + line + "\n" + rcBlockEnd + "\n"
	body := stripRCBlock(string(existing))
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return os.WriteFile(rc, []byte(body+block), 0o600)
}

// stripRCBlock removes the marked block from s, leaving everything else
// byte-identical. An unterminated start marker removes to end of file — the
// alternative, leaving it, would make the next write nest a block inside a
// block.
func stripRCBlock(s string) string {
	start := strings.Index(s, rcBlockStart)
	if start < 0 {
		return s
	}
	rest := s[start:]
	end := strings.Index(rest, rcBlockEnd)
	if end < 0 {
		return s[:start]
	}
	tail := rest[end+len(rcBlockEnd):]
	return s[:start] + strings.TrimPrefix(tail, "\n")
}

func runUninstall(out io.Writer, opts installOpts) error {
	binDir, err := resolveBinDir(opts)
	if err != nil {
		return err
	}
	dst := filepath.Join(binDir, "mache")

	if cellar, brewed := homebrewCellar(dst); brewed {
		return fmt.Errorf(
			"%s is Homebrew-managed (resolves to %s) — refusing to delete it out from under brew. "+
				"Run `brew uninstall mache` instead", dst, cellar)
	}

	removed := 0
	switch _, statErr := os.Lstat(dst); {
	case errors.Is(statErr, os.ErrNotExist):
		logf(out, "nothing to remove at %s\n", dst)
	case opts.dryRun:
		logf(out, "would remove %s\n", dst)
	default:
		if err := os.Remove(dst); err != nil {
			return fmt.Errorf("remove %s: %w", dst, err)
		}
		logf(out, "removed %s\n", dst)
		removed++
	}

	if err := removePinnedLeyline(out, opts); err != nil {
		return err
	}
	if err := removeRCBlock(out, opts); err != nil {
		return err
	}
	if removed == 0 && !opts.dryRun {
		logf(out, "note: `mache install` installs to ~/.local/bin by default; "+
			"use --bin-dir if you installed elsewhere\n")
	}
	return nil
}

// removePinnedLeyline deletes only THIS build's namespaced cache entry. Other
// versions in ~/.mache/bin belong to other mache builds — the whole point of
// namespacing (mache-0acdf6) — so removing them would force those builds to
// re-download, which is not this command's business.
func removePinnedLeyline(out io.Writer, opts installOpts) error {
	if opts.keepLeyline {
		logf(out, "leaving the cached leyline in place (--keep-leyline)\n")
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cannot determine home directory: %w", err)
	}
	pinned := filepath.Join(home, ".mache", "bin", "leyline-"+leyline.BinaryVersion)
	switch _, statErr := os.Lstat(pinned); {
	case errors.Is(statErr, os.ErrNotExist):
		logf(out, "no cached leyline %s to remove\n", leyline.BinaryVersion)
	case opts.dryRun:
		logf(out, "would remove %s\n", pinned)
	default:
		if err := os.Remove(pinned); err != nil {
			return fmt.Errorf("remove %s: %w", pinned, err)
		}
		logf(out, "removed %s\n", pinned)
	}
	return nil
}

// removeRCBlock takes back exactly what `install --update-rc` wrote, and only
// when asked. An uninstall that edited a shell rc unprompted would be the same
// overreach as an install that did.
func removeRCBlock(out io.Writer, opts installOpts) error {
	if !opts.updateRC {
		return nil
	}
	rc, err := shellRCPath()
	if err != nil {
		return err
	}
	existing, err := os.ReadFile(rc)
	if errors.Is(err, os.ErrNotExist) {
		logf(out, "no %s to edit\n", rc)
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", rc, err)
	}
	stripped := stripRCBlock(string(existing))
	if stripped == string(existing) {
		logf(out, "no mache PATH block found in %s\n", rc)
		return nil
	}
	if opts.dryRun {
		logf(out, "would remove the PATH block from %s\n", rc)
		return nil
	}
	if err := os.WriteFile(rc, []byte(stripped), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", rc, err)
	}
	logf(out, "removed the PATH block from %s\n", rc)
	return nil
}
