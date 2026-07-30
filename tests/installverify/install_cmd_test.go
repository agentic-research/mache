package installverify

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The install gate's view of `mache install`.
//
// cmd/install_test.go covers the command's behaviour in process. What it
// cannot answer is the question this package exists for: does the SHIPPED
// binary carry a working provisioning path? A release-asset user has no
// checkout and no Taskfile, so `mache install` is the only provisioning they
// have — and a binary that lost the subcommand, or that shipped it broken,
// looks identical from inside the source tree.
//
// leyline provisioning is skipped here (--no-leyline): reaching the pin is
// already asserted, from the outside and against a stale PATH shadow, in
// leyline_pin_test.go. What is unproven without this file is the BINARY-COPY
// half — that the thing landing in the bin dir is a working mache.

// TestInstalledMacheCanInstallItself installs the binary under test into a
// scratch bin dir with its own install subcommand, then runs the RESULT. A
// copy that lands non-executable, truncated, or unable to report its version
// is not an install, however cleanly the command exited.
func TestInstalledMacheCanInstallItself(t *testing.T) {
	bin := macheBinary(t)
	binDir := filepath.Join(t.TempDir(), "bin")

	out := runner{}.mustRun(t, bin, "install", "--bin-dir", binDir, "--no-leyline")
	assert.Contains(t, out.combined(), binDir, "install must report where it put the binary")

	installed := filepath.Join(binDir, "mache")
	info := requireFileExists(t, installed, "the binary `mache install` copied")
	require.NotZerof(t, info.Mode()&0o111, "%s is not executable", installed)

	assert.Equal(t, reportedVersion(t, bin), reportedVersion(t, installed),
		"the installed copy must be the same mache, not a truncated or stale one")
}

// TestInstalledMacheInstallDryRunChangesNothing — --dry-run has to be
// trustworthy on the shipped artifact specifically, because it is what a
// cautious user runs FIRST against a binary they just downloaded.
func TestInstalledMacheInstallDryRunChangesNothing(t *testing.T) {
	bin := macheBinary(t)
	binDir := filepath.Join(t.TempDir(), "bin")

	out := runner{}.mustRun(t, bin, "install", "--bin-dir", binDir, "--no-leyline", "--dry-run")
	assert.Contains(t, out.combined(), "would install")

	_, err := os.Stat(filepath.Join(binDir, "mache"))
	assert.True(t, os.IsNotExist(err), "--dry-run wrote a binary it said it would only plan")
}

// TestInstalledMacheUninstallRemovesWhatInstallCreated closes the loop: a user
// who can install from a release asset must be able to undo it with the same
// binary.
func TestInstalledMacheUninstallRemovesWhatInstallCreated(t *testing.T) {
	bin := macheBinary(t)
	binDir := filepath.Join(t.TempDir(), "bin")

	runner{}.mustRun(t, bin, "install", "--bin-dir", binDir, "--no-leyline")
	installed := filepath.Join(binDir, "mache")
	requireFileExists(t, installed, "the binary to be removed")

	// --keep-leyline so the developer's real cache is untouched; the cached
	// pin is shared with everything else on this machine, and removing it is
	// not this test's business.
	out := runner{}.mustRun(t, bin, "uninstall", "--bin-dir", binDir, "--keep-leyline")
	assert.Contains(t, out.combined(), installed, "uninstall must report what it removed")

	_, err := os.Lstat(installed)
	assert.True(t, os.IsNotExist(err), "uninstall reported success but left %s", installed)
}
