package cmd

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-research/mache/internal/leyline"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isolateInstall gives a test its own HOME, its own PATH, and a fake mache to
// install, so nothing it does can touch the developer's real ~/.local/bin or
// ~/.mache. Returns (home, sourceBinary).
//
// MACHE_NO_LEYLINE is deliberately NOT set here: several tests plant the pinned
// binary in the cache and assert the install path finds it there, which is the
// behaviour that matters. Tests that must not provision pass --no-leyline.
func isolateInstall(t *testing.T) (home, src string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	t.Setenv(leyline.BinaryOverrideEnv, "")
	t.Setenv("SHELL", "/bin/zsh")

	src = filepath.Join(t.TempDir(), "mache")
	require.NoError(t, os.WriteFile(src, []byte("#!/bin/sh\necho fake-mache\n"), 0o755))
	orig := executablePath
	executablePath = func() (string, error) { return src, nil }
	t.Cleanup(func() { executablePath = orig })
	return home, src
}

// plantPinnedLeyline puts a binary reporting the pinned version at the
// version-namespaced cache path, so provisioning is a cache hit and no test
// reaches the network.
func plantPinnedLeyline(t *testing.T, home string) string {
	t.Helper()
	p := filepath.Join(home, ".mache", "bin", "leyline-"+leyline.BinaryVersion)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	body := "#!/bin/sh\necho 'leyline " + strings.TrimPrefix(leyline.BinaryVersion, "v") + " (open)'\n"
	require.NoError(t, os.WriteFile(p, []byte(body), 0o755))
	return p
}

// runCommandBody runs one of the two command bodies and returns its stdout, so
// every assertion below reads the report the user would see rather than
// reaching into internals.
func runCommandBody(t *testing.T, run func(io.Writer, installOpts) error, opts installOpts) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(&out, opts)
	return out.String(), err
}

// ---------------------------------------------------------------------------
// The hard constraint: the unversioned cache path stays unwritten.
// ---------------------------------------------------------------------------

// TestInstall_NeverWritesUnversionedLeylinePath guards mache-0acdf6, which is
// the reason this command provisions through leyline.EnsureCachedBinary rather
// than managing the cache itself. ~/.mache/bin/leyline (no version suffix) was
// a shared mutable resource that concurrent mache builds overwrote, silently
// changing _ast shape between runs; it is read-only-never-written.
//
// This is the assertion most likely to be broken by a well-meaning later
// change — "install should make leyline easy to run, let's symlink it" — which
// is exactly why it is stated here rather than left to the doc comment.
func TestInstall_NeverWritesUnversionedLeylinePath(t *testing.T) {
	home, _ := isolateInstall(t)
	plantPinnedLeyline(t, home)

	_, err := runCommandBody(t, runInstall, installOpts{binDir: filepath.Join(home, "bin")})
	require.NoError(t, err)

	unversioned := filepath.Join(home, ".mache", "bin", "leyline")
	_, statErr := os.Lstat(unversioned)
	assert.Truef(t, os.IsNotExist(statErr),
		"the unversioned ~/.mache/bin/leyline must stay unwritten (mache-0acdf6); got %v", statErr)
}

// TestInstall_DoesNotPutLeylineOnPATH is the other half of the same
// constraint. Putting leyline on PATH — by copy, symlink or shim — recreates
// "which version does bare `leyline` mean" the moment the pin moves, which is
// the mache-19326d skew rather than a fix for it.
func TestInstall_DoesNotPutLeylineOnPATH(t *testing.T) {
	home, _ := isolateInstall(t)
	plantPinnedLeyline(t, home)
	binDir := filepath.Join(home, "bin")

	_, err := runCommandBody(t, runInstall, installOpts{binDir: binDir})
	require.NoError(t, err)

	entries, err := os.ReadDir(binDir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Equal(t, []string{"mache"}, names,
		"the bin dir must receive mache and nothing else — a leyline here is a PATH shim by another name")
}

// ---------------------------------------------------------------------------
// Installing
// ---------------------------------------------------------------------------

func TestInstall_CopiesTheBinaryExecutable(t *testing.T) {
	home, src := isolateInstall(t)
	plantPinnedLeyline(t, home)
	binDir := filepath.Join(home, "bin")

	out, err := runCommandBody(t, runInstall, installOpts{binDir: binDir})
	require.NoError(t, err)

	dst := filepath.Join(binDir, "mache")
	got, err := os.ReadFile(dst)
	require.NoError(t, err, "install must create %s", dst)
	want, err := os.ReadFile(src)
	require.NoError(t, err)
	assert.Equal(t, want, got, "the installed bytes must be the running binary's")

	info, err := os.Stat(dst)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&0o111, "an installed mache that is not executable is not installed")
	assert.Contains(t, out, dst)
}

func TestInstall_OverwritesAnEarlierInstall(t *testing.T) {
	home, src := isolateInstall(t)
	plantPinnedLeyline(t, home)
	binDir := filepath.Join(home, "bin")
	dst := filepath.Join(binDir, "mache")

	require.NoError(t, os.MkdirAll(binDir, 0o755))
	require.NoError(t, os.WriteFile(dst, []byte("#!/bin/sh\necho stale\n"), 0o755))

	_, err := runCommandBody(t, runInstall, installOpts{binDir: binDir})
	require.NoError(t, err)

	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	want, err := os.ReadFile(src)
	require.NoError(t, err)
	assert.Equal(t, want, got, "a re-install must replace the older copy, not leave it")
}

func TestInstall_ReportsWhenItIsAlreadyTheInstalledBinary(t *testing.T) {
	home, _ := isolateInstall(t)
	plantPinnedLeyline(t, home)
	binDir := filepath.Join(home, "bin")
	dst := filepath.Join(binDir, "mache")

	// Run "from" the destination: the same file is both source and target.
	require.NoError(t, os.MkdirAll(binDir, 0o755))
	require.NoError(t, os.WriteFile(dst, []byte("#!/bin/sh\necho installed\n"), 0o755))
	executablePath = func() (string, error) { return dst, nil }

	out, err := runCommandBody(t, runInstall, installOpts{binDir: binDir})
	require.NoError(t, err, "installing over yourself must be a no-op, not a truncated binary")
	assert.Contains(t, out, "already installed")

	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, "#!/bin/sh\necho installed\n", string(got), "the running binary must be left intact")
}

func TestInstall_DryRunChangesNothing(t *testing.T) {
	home, _ := isolateInstall(t)
	plantPinnedLeyline(t, home)
	binDir := filepath.Join(home, "bin")

	out, err := runCommandBody(t, runInstall, installOpts{binDir: binDir, dryRun: true, updateRC: true})
	require.NoError(t, err)

	assert.Contains(t, out, "would install")
	_, statErr := os.Stat(filepath.Join(binDir, "mache"))
	assert.True(t, os.IsNotExist(statErr), "--dry-run must not write the binary")
	_, statErr = os.Stat(filepath.Join(home, ".zshrc"))
	assert.True(t, os.IsNotExist(statErr), "--dry-run must not write the shell rc")
}

func TestInstall_ProvisionsThePinnedLeyline(t *testing.T) {
	home, _ := isolateInstall(t)
	pinned := plantPinnedLeyline(t, home)

	out, err := runCommandBody(t, runInstall, installOpts{binDir: filepath.Join(home, "bin")})
	require.NoError(t, err)
	assert.Contains(t, out, pinned, "install must report the leyline it provisioned")
	assert.Contains(t, out, leyline.BinaryVersion, "and name the pin, since every _ast number inherits it")
}

func TestInstall_NoLeylineSkipsProvisioningAndSaysSo(t *testing.T) {
	home, _ := isolateInstall(t)
	t.Setenv("MACHE_NO_LEYLINE", "1") // provisioning would fail if it ran at all

	out, err := runCommandBody(t, runInstall, installOpts{binDir: filepath.Join(home, "bin"), skipLeyline: true})
	require.NoError(t, err)
	assert.Contains(t, out, "skipping leyline provisioning")
	_, statErr := os.Stat(filepath.Join(home, ".mache", "bin"))
	assert.True(t, os.IsNotExist(statErr), "--no-leyline must not create the cache dir")
}

// TestInstall_FailsWhenLeylineCannotBeProvisioned — leyline is mache's sole
// parser, so an install that could not provision it has not installed a
// working mache. Reporting success would be the silent-degradation failure
// this whole area exists to prevent.
func TestInstall_FailsWhenLeylineCannotBeProvisioned(t *testing.T) {
	home, _ := isolateInstall(t)
	t.Setenv("MACHE_NO_LEYLINE", "1") // no cache planted, no download allowed

	_, err := runCommandBody(t, runInstall, installOpts{binDir: filepath.Join(home, "bin")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), leyline.BinaryVersion, "the failure must name the pin it could not get")
}

// ---------------------------------------------------------------------------
// Homebrew
// ---------------------------------------------------------------------------

// plantBrewMache builds the shape a Homebrew install has — a bin/ symlink into
// a Cellar tree — and returns the bin dir holding it.
func plantBrewMache(t *testing.T, prefix string) string {
	t.Helper()
	cellar := filepath.Join(prefix, "Cellar", "mache", "0.1.31", "bin")
	binDir := filepath.Join(prefix, "bin")
	require.NoError(t, os.MkdirAll(cellar, 0o755))
	require.NoError(t, os.MkdirAll(binDir, 0o755))
	real := filepath.Join(cellar, "mache")
	require.NoError(t, os.WriteFile(real, []byte("#!/bin/sh\necho brew\n"), 0o755))
	require.NoError(t, os.Symlink(real, filepath.Join(binDir, "mache")))
	return binDir
}

// TestUninstall_RefusesAHomebrewManagedMache is the case that has actually
// bitten: agentic-research/homebrew-tap ships a Formula/mache.rb that installed
// kiln's bundled binary under the name `mache`, and it shadowed a ~/.local/bin
// install for weeks (see the mache-6ec106 triage). Deleting the file would
// leave brew's manifest claiming it and the next upgrade would restore it, so
// the answer is `brew uninstall mache`, not `rm`.
func TestUninstall_RefusesAHomebrewManagedMache(t *testing.T) {
	home, _ := isolateInstall(t)
	binDir := plantBrewMache(t, filepath.Join(home, "opt", "homebrew"))
	link := filepath.Join(binDir, "mache")

	_, err := runCommandBody(t, runUninstall, installOpts{binDir: binDir})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Homebrew")
	assert.Contains(t, err.Error(), "brew uninstall mache", "the error must name the correct action")

	_, statErr := os.Lstat(link)
	assert.NoError(t, statErr, "brew's symlink must survive the refusal")
}

// TestInstall_RefusesToOverwriteAHomebrewManagedMache — same reasoning in the
// other direction: overwriting brew's symlink target leaves brew believing it
// owns bytes it did not write.
func TestInstall_RefusesToOverwriteAHomebrewManagedMache(t *testing.T) {
	home, _ := isolateInstall(t)
	binDir := plantBrewMache(t, filepath.Join(home, "usr", "local"))

	_, err := runCommandBody(t, runInstall, installOpts{binDir: binDir})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Homebrew")

	got, err := os.ReadFile(filepath.Join(binDir, "mache"))
	require.NoError(t, err)
	assert.Equal(t, "#!/bin/sh\necho brew\n", string(got), "brew's binary must be untouched")
}

// TestHomebrewCellar_DoesNotFalsePositive — the detector must not treat an
// ordinary path as brew-managed, or `mache uninstall` would refuse to do its
// job on a normal install.
func TestHomebrewCellar_DoesNotFalsePositive(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "mache")
	require.NoError(t, os.WriteFile(plain, []byte("x"), 0o755))

	_, brewed := homebrewCellar(plain)
	assert.False(t, brewed, "a plain file is not Homebrew-managed")

	// A directory merely NAMED like a cellar component is still not one; the
	// marker is the path element "Cellar", exactly.
	near := filepath.Join(dir, "CellarDoor")
	require.NoError(t, os.MkdirAll(near, 0o755))
	notBrew := filepath.Join(near, "mache")
	require.NoError(t, os.WriteFile(notBrew, []byte("x"), 0o755))
	_, brewed = homebrewCellar(notBrew)
	assert.False(t, brewed, "'CellarDoor' is not the 'Cellar' path element")
}

// ---------------------------------------------------------------------------
// PATH advice and the shell rc
// ---------------------------------------------------------------------------

// TestInstall_PrintsTheExportLineAndLeavesTheRCAlone is the rustup posture:
// tell the user what to add, do not reach into their dotfiles uninvited.
func TestInstall_PrintsTheExportLineAndLeavesTheRCAlone(t *testing.T) {
	home, _ := isolateInstall(t)
	plantPinnedLeyline(t, home)
	binDir := filepath.Join(home, "bin")
	rc := filepath.Join(home, ".zshrc")
	require.NoError(t, os.WriteFile(rc, []byte("# my own config\n"), 0o600))

	out, err := runCommandBody(t, runInstall, installOpts{binDir: binDir})
	require.NoError(t, err)

	assert.Contains(t, out, exportLine(binDir), "the line to add must be printed verbatim, ready to paste")
	assert.Contains(t, out, "--update-rc", "and the opt-in must be discoverable")

	after, err := os.ReadFile(rc)
	require.NoError(t, err)
	assert.Equal(t, "# my own config\n", string(after),
		"the shell rc must be byte-identical without --update-rc")
}

func TestInstall_SaysNothingToDoWhenTheBinDirIsAlreadyOnPATH(t *testing.T) {
	home, _ := isolateInstall(t)
	plantPinnedLeyline(t, home)
	binDir := filepath.Join(home, "bin")
	require.NoError(t, os.MkdirAll(binDir, 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, err := runCommandBody(t, runInstall, installOpts{binDir: binDir})
	require.NoError(t, err)
	assert.Contains(t, out, "already on PATH")
	assert.NotContains(t, out, exportLine(binDir), "no advice is needed when the dir is already reachable")
}

// TestInstall_UpdateRCIsIdempotent — running install twice must leave ONE
// managed block, not two. An rc file that grows a stanza per install is how
// tools earn a reputation for vandalising dotfiles.
func TestInstall_UpdateRCIsIdempotent(t *testing.T) {
	home, _ := isolateInstall(t)
	plantPinnedLeyline(t, home)
	binDir := filepath.Join(home, "bin")
	rc := filepath.Join(home, ".zshrc")
	require.NoError(t, os.WriteFile(rc, []byte("# mine\nalias k=kubectl\n"), 0o600))

	for range 2 {
		_, err := runCommandBody(t, runInstall, installOpts{binDir: binDir, updateRC: true})
		require.NoError(t, err)
	}

	body, err := os.ReadFile(rc)
	require.NoError(t, err)
	got := string(body)
	assert.Equal(t, 1, strings.Count(got, rcBlockStart), "exactly one managed block")
	assert.Equal(t, 1, strings.Count(got, rcBlockEnd))
	assert.Contains(t, got, exportLine(binDir))
	assert.Contains(t, got, "alias k=kubectl", "the user's own lines must survive")
}

func TestInstall_UpdateRCCreatesTheFileWhenAbsent(t *testing.T) {
	home, _ := isolateInstall(t)
	plantPinnedLeyline(t, home)
	binDir := filepath.Join(home, "bin")

	_, err := runCommandBody(t, runInstall, installOpts{binDir: binDir, updateRC: true})
	require.NoError(t, err)

	body, err := os.ReadFile(filepath.Join(home, ".zshrc"))
	require.NoError(t, err)
	assert.Contains(t, string(body), exportLine(binDir))
}

func TestInstall_UpdateRCRefusesAnUnknownShell(t *testing.T) {
	home, _ := isolateInstall(t)
	plantPinnedLeyline(t, home)
	t.Setenv("SHELL", "/usr/bin/fish")

	_, err := runCommandBody(t, runInstall, installOpts{binDir: filepath.Join(home, "bin"), updateRC: true})
	require.Error(t, err, "guessing at an unknown shell's syntax would write a line that silently does nothing")
	assert.Contains(t, err.Error(), "fish")
}

func TestShellRCPath_PerShell(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for shell, want := range map[string]string{
		"/bin/zsh":  ".zshrc",
		"/bin/bash": ".bashrc",
		"/bin/sh":   ".profile",
	} {
		t.Setenv("SHELL", shell)
		got, err := shellRCPath()
		require.NoError(t, err, shell)
		assert.Equal(t, filepath.Join(home, want), got, shell)
	}
}

// TestStripRCBlock covers the shapes writeRCBlock has to survive re-reading.
func TestStripRCBlock(t *testing.T) {
	block := rcBlockStart + "\nexport PATH=\"/x\":$PATH\n" + rcBlockEnd + "\n"
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "no block is untouched", in: "# mine\n", want: "# mine\n"},
		{name: "block alone leaves nothing", in: block, want: ""},
		{name: "block after user lines", in: "# mine\n" + block, want: "# mine\n"},
		{name: "block before user lines", in: block + "# after\n", want: "# after\n"},
		{
			name: "an unterminated start marker is removed to EOF",
			in:   "# mine\n" + rcBlockStart + "\nexport PATH=oops\n",
			want: "# mine\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, stripRCBlock(tt.in))
		})
	}
}

// ---------------------------------------------------------------------------
// Uninstalling
// ---------------------------------------------------------------------------

func TestUninstall_RemovesTheBinaryAndThePinnedLeyline(t *testing.T) {
	home, _ := isolateInstall(t)
	pinned := plantPinnedLeyline(t, home)
	binDir := filepath.Join(home, "bin")
	_, err := runCommandBody(t, runInstall, installOpts{binDir: binDir})
	require.NoError(t, err)

	out, err := runCommandBody(t, runUninstall, installOpts{binDir: binDir})
	require.NoError(t, err)

	dst := filepath.Join(binDir, "mache")
	_, statErr := os.Lstat(dst)
	assert.True(t, os.IsNotExist(statErr), "the installed binary must be gone")
	_, statErr = os.Lstat(pinned)
	assert.True(t, os.IsNotExist(statErr), "the pinned leyline must be gone")

	assert.Contains(t, out, dst, "uninstall must report what it removed")
	assert.Contains(t, out, pinned)
}

// TestUninstall_LeavesOtherBuildsCachedLeylinesAlone — the cache is namespaced
// by version precisely so concurrent mache builds cannot fight over it
// (mache-0acdf6). Deleting another build's file would force it to re-download
// and is not this command's business.
func TestUninstall_LeavesOtherBuildsCachedLeylinesAlone(t *testing.T) {
	home, _ := isolateInstall(t)
	plantPinnedLeyline(t, home)
	other := filepath.Join(home, ".mache", "bin", "leyline-v0.0.1-someone-elses-pin")
	require.NoError(t, os.WriteFile(other, []byte("other build's binary"), 0o755))

	_, err := runCommandBody(t, runUninstall, installOpts{binDir: filepath.Join(home, "bin")})
	require.NoError(t, err)

	body, err := os.ReadFile(other)
	require.NoError(t, err, "another build's cached leyline must survive")
	assert.Equal(t, "other build's binary", string(body))
}

func TestUninstall_KeepLeylineLeavesTheCache(t *testing.T) {
	home, _ := isolateInstall(t)
	pinned := plantPinnedLeyline(t, home)

	out, err := runCommandBody(t, runUninstall, installOpts{binDir: filepath.Join(home, "bin"), keepLeyline: true})
	require.NoError(t, err)
	assert.Contains(t, out, "--keep-leyline")
	_, statErr := os.Lstat(pinned)
	assert.NoError(t, statErr)
}

func TestUninstall_DryRunChangesNothing(t *testing.T) {
	home, _ := isolateInstall(t)
	pinned := plantPinnedLeyline(t, home)
	binDir := filepath.Join(home, "bin")
	_, err := runCommandBody(t, runInstall, installOpts{binDir: binDir})
	require.NoError(t, err)

	out, err := runCommandBody(t, runUninstall, installOpts{binDir: binDir, dryRun: true})
	require.NoError(t, err)
	assert.Contains(t, out, "would remove")

	_, statErr := os.Stat(filepath.Join(binDir, "mache"))
	assert.NoError(t, statErr, "--dry-run must not remove the binary")
	_, statErr = os.Stat(pinned)
	assert.NoError(t, statErr, "--dry-run must not remove the cached leyline")
}

func TestUninstall_ReportsWhenThereIsNothingToRemove(t *testing.T) {
	home, _ := isolateInstall(t)

	out, err := runCommandBody(t, runUninstall, installOpts{binDir: filepath.Join(home, "bin")})
	require.NoError(t, err, "an absent install is not an error")
	assert.Contains(t, out, "nothing to remove")
	assert.Contains(t, out, "--bin-dir", "and the likely reason must be named")
}

func TestUninstall_UpdateRCRemovesExactlyTheManagedBlock(t *testing.T) {
	home, _ := isolateInstall(t)
	plantPinnedLeyline(t, home)
	binDir := filepath.Join(home, "bin")
	rc := filepath.Join(home, ".zshrc")
	require.NoError(t, os.WriteFile(rc, []byte("# mine\n"), 0o600))

	_, err := runCommandBody(t, runInstall, installOpts{binDir: binDir, updateRC: true})
	require.NoError(t, err)
	_, err = runCommandBody(t, runUninstall, installOpts{binDir: binDir, updateRC: true})
	require.NoError(t, err)

	body, err := os.ReadFile(rc)
	require.NoError(t, err)
	assert.Equal(t, "# mine\n", string(body), "the rc must return to exactly its pre-install content")
}

func TestUninstall_LeavesTheRCAloneWithoutUpdateRC(t *testing.T) {
	home, _ := isolateInstall(t)
	plantPinnedLeyline(t, home)
	binDir := filepath.Join(home, "bin")
	rc := filepath.Join(home, ".zshrc")

	_, err := runCommandBody(t, runInstall, installOpts{binDir: binDir, updateRC: true})
	require.NoError(t, err)
	before, err := os.ReadFile(rc)
	require.NoError(t, err)

	_, err = runCommandBody(t, runUninstall, installOpts{binDir: binDir})
	require.NoError(t, err)

	after, err := os.ReadFile(rc)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after),
		"uninstall must not edit dotfiles it was not asked to edit")
}

// ---------------------------------------------------------------------------
// Command surface
// ---------------------------------------------------------------------------

// TestInstallCommands_Registered — these only help if they are reachable as
// `mache install` / `mache uninstall`.
func TestInstallCommands_Registered(t *testing.T) {
	byName := map[string]*cobra.Command{}
	for _, c := range rootCmd.Commands() {
		byName[c.Name()] = c
	}
	for _, name := range []string{"install", "uninstall"} {
		c, ok := byName[name]
		require.Truef(t, ok, "`mache %s` must be registered on the root command", name)
		assert.NotNil(t, c.Flags().Lookup("bin-dir"), "%s needs --bin-dir", name)
		assert.NotNil(t, c.Flags().Lookup("dry-run"), "%s needs --dry-run", name)
		assert.NotNil(t, c.Flags().Lookup("update-rc"), "%s needs --update-rc", name)
	}
	assert.NotNil(t, byName["install"].Flags().Lookup("no-leyline"))
	assert.NotNil(t, byName["uninstall"].Flags().Lookup("keep-leyline"))
}
