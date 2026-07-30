// Package installverify is the INSTALL gate: it verifies a real mache
// installation instead of assuming one.
//
// # Why this exists separately from the rest of the suite
//
// mache already has an all-tools harness (cmd/all_tools_e2e_test.go,
// TestArena_AllTools). It exercises the tool surface by constructing graphs
// IN PROCESS, which answers "does this source tree behave" — a different
// question from "does the artifact a user actually installed behave". Three
// things are invisible to an in-process test and are exactly what shipped
// broken:
//
//   - the version the installed binary REPORTS (bead mache-4d7f2c: nothing
//     verified a published artifact after the fact);
//   - which leyline that binary REACHES. A stale ~/.local/bin/leyline 0.10.3
//     shadowed the v0.13.0 pin on the reporter's machine, so `leyline cdc
//     enable` at a shell ran a parser three minors off from the one that built
//     the .db (bead mache-19326d). An in-process test resolves leyline through
//     the same package it is testing, so it cannot see the skew;
//   - whether the binary works from a CLEAN HOME with no repo checkout and no
//     Taskfile — the condition every release-asset and Homebrew user is in,
//     and the one a dev box cannot honestly simulate (docker_test.go).
//
// So this package drives an EXTERNAL binary: fork/exec only, MCP over the
// wire, no imports from cmd/. It deliberately does not use
// github.com/mark3labs/mcp-go's client either — the server speaks that
// library, and a gate that verifies a library against itself proves less than
// one that speaks the raw protocol a third-party consumer speaks.
//
// # Running it
//
//	task install:verify                        # the just-built bin/mache
//	task install:verify BIN=~/.local/bin/mache # what `task install` installed
//	task install:verify:docker                 # clean-HOME leg, published image
//
// The binary under test comes from MACHE_VERIFY_BINARY. With it unset every
// test here skips, so `go test ./...` stays green without a build step; the
// Taskfile targets above are the supported entry points and CI invokes those.
package installverify

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// binaryEnv names the mache binary under test. Absent -> skip, so this package
// is inert under a plain `go test ./...` and load-bearing under `task
// install:verify`.
const binaryEnv = "MACHE_VERIFY_BINARY"

// cmdTimeout bounds every subprocess this package runs. Generous: `mache
// build` shells out to leyline, which parses a whole tree.
const cmdTimeout = 5 * time.Minute

// macheBinary returns the absolute path of the binary under test, skipping the
// test when none is configured.
func macheBinary(t *testing.T) string {
	t.Helper()
	p := strings.TrimSpace(os.Getenv(binaryEnv))
	if p == "" {
		t.Skipf("%s unset — run `task install:verify` (or set %s=/path/to/mache)", binaryEnv, binaryEnv)
	}
	abs, err := filepath.Abs(p)
	require.NoError(t, err, "resolve %s=%q", binaryEnv, p)
	info, err := os.Stat(abs)
	require.NoErrorf(t, err, "%s=%s does not exist — the gate verifies an INSTALLED binary, so a missing one is a failure, not a skip", binaryEnv, abs)
	require.False(t, info.IsDir(), "%s=%s is a directory", binaryEnv, abs)
	require.NotZero(t, info.Mode()&0o111, "%s=%s is not executable", binaryEnv, abs)
	return abs
}

// result is the outcome of one subprocess run.
type result struct {
	stdout string
	stderr string
	code   int
	err    error // non-nil only when the process could not be run/awaited
}

// combined is stdout+stderr, for assertions that don't care which stream a
// diagnostic landed on.
func (r result) combined() string { return r.stdout + r.stderr }

// runner runs a command with a bounded timeout and an explicit environment.
type runner struct {
	env []string // when nil, the parent environment is inherited
	dir string
}

// run executes name with args and captures both streams. A non-zero exit is a
// RESULT, not an error — callers assert on the code — so err is reserved for
// "could not run it at all".
func (rn runner) run(t *testing.T, name string, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), cmdTimeout)
	defer cancel()

	c := exec.CommandContext(ctx, name, args...)
	c.Dir = rn.dir
	if rn.env != nil {
		c.Env = rn.env
	}
	var stdout, stderr strings.Builder
	c.Stdout, c.Stderr = &stdout, &stderr

	err := c.Run()
	res := result{stdout: stdout.String(), stderr: stderr.String()}
	if err == nil {
		return res
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.code = exitErr.ExitCode()
		return res
	}
	res.err = err
	return res
}

// mustRun runs a command and fails the test unless it exits 0, quoting both
// streams — a gate whose failure message omits the tool's own diagnostic wastes
// the run that produced it.
func (rn runner) mustRun(t *testing.T, name string, args ...string) result {
	t.Helper()
	res := rn.run(t, name, args...)
	require.NoError(t, res.err, "run %s %v", name, args)
	require.Zerof(t, res.code, "%s %v exited %d\n--- stdout ---\n%s\n--- stderr ---\n%s",
		name, args, res.code, res.stdout, res.stderr)
	return res
}

// macheTreeRoot is the mache checkout this test file lives in. Used to read
// version.txt and to reach the fixture; the gate reads the SOURCE OF TRUTH
// from the tree and compares the INSTALLED binary against it, which is the
// whole point.
func macheTreeRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd() // tests/installverify
	require.NoError(t, err)
	root := filepath.Dir(filepath.Dir(wd))
	_, err = os.Stat(filepath.Join(root, "go.mod"))
	require.NoErrorf(t, err, "expected the repo root at %s (no go.mod there)", root)
	return root
}

// fixtureDir is the tiny, deliberately stable source tree the MCP assertions
// project. See its package doc for why its shape is load-bearing.
func fixtureDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.Abs(filepath.Join("testdata", "fixture"))
	require.NoError(t, err)
	return d
}
