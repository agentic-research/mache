package lint

// Tests for scripts/go-workspace-check.sh (mache-49b87e).
//
// The condition under test — go.work and go.mod disagreeing about the `go`
// directive — stops EVERY go command before it does anything, `go test`
// included. So the check itself cannot be a Go test: it would never get to
// run in the state it exists to catch. It is a script, and what these tests
// pin is the script's failure HANDLING, from a healthy tree where go does
// work. Same arrangement as scripts/flamegraphs.sh.
//
// The defect that motivated it: a dependency bump raised go.mod to `go 1.26.0`
// while go.work stayed at `go 1.26`, and the result was six red required
// checks — lint, smells, install-verify, server-json-drift, test on both
// runners — none of them the dependency. It reads as infrastructure flake,
// and was misdiagnosed as exactly that before anyone read past the aggregator
// job to the one line that named the cause.

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// workspaceFixture writes a one-module workspace with the given directives.
// An empty workVersion means "no go.work at all".
func workspaceFixture(t *testing.T, modVersion, workVersion string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/probe\n\ngo "+modVersion+"\n"), 0o644))
	if workVersion != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "go.work"),
			[]byte("go "+workVersion+"\n\nuse .\n"), 0o644))
	}
	return dir
}

func runWorkspaceCheck(t *testing.T, dir string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash",
		filepath.Join(testutil.MacheRepoRoot(t), "scripts", "go-workspace-check.sh"), dir)
	out, err := cmd.CombinedOutput()
	code := 0
	var exit *exec.ExitError
	if err != nil {
		require.ErrorAs(t, err, &exit, "script failed to run at all: %v", err)
		code = exit.ExitCode()
	}
	return string(out), code
}

// TestWorkspaceCheck_AgreeingDirectivesPassQuietly — the gate runs first in
// `task check` and `task ci`, so on a healthy tree it must cost nothing and
// say nothing.
func TestWorkspaceCheck_AgreeingDirectivesPassQuietly(t *testing.T) {
	out, code := runWorkspaceCheck(t, workspaceFixture(t, "1.26.0", "1.26.0"))
	assert.Equal(t, 0, code)
	assert.Empty(t, out, "a healthy workspace must produce no output")
}

// TestWorkspaceCheck_NoWorkspaceIsNotAFailure — most repos have no go.work,
// and the script is expected to be a no-op there rather than an error.
func TestWorkspaceCheck_NoWorkspaceIsNotAFailure(t *testing.T) {
	out, code := runWorkspaceCheck(t, workspaceFixture(t, "1.26.0", ""))
	assert.Equal(t, 0, code)
	assert.Empty(t, out)
}

// TestWorkspaceCheck_CatchesTheExactPairThatShipped is the regression.
//
// `go 1.26` vs `go 1.26.0` is the pair that actually broke CI, and it is the
// one a naive comparison gets wrong: read as version numbers they look equal,
// and `sort -V` orders 1.26 AFTER 1.26.0. Go's own rule puts 1.26 BEFORE
// 1.26.0, which is why detection is delegated to the toolchain instead of
// reimplemented here.
func TestWorkspaceCheck_CatchesTheExactPairThatShipped(t *testing.T) {
	out, code := runWorkspaceCheck(t, workspaceFixture(t, "1.26.0", "1.26"))

	require.Equal(t, 1, code, "a disagreement must fail the gate:\n%s", out)
	assert.Contains(t, out, "go.work and go.mod disagree")
	assert.Contains(t, out, "go.mod:  go 1.26.0", "the message must show both directives")
	assert.Contains(t, out, "go.work: go 1.26")
	assert.Contains(t, out, "go work use", "and the command that fixes it")
	assert.Contains(t, out, "requires go >=",
		"and Go's own words, so the reader can match it against the CI log they arrived from")
}

// TestWorkspaceCheck_DoesNotSwallowOtherModuleErrors — the script exists to
// make one failure legible, not to become a place where other module errors
// disappear. A malformed go.mod must still fail, with its own message.
func TestWorkspaceCheck_DoesNotSwallowOtherModuleErrors(t *testing.T) {
	dir := workspaceFixture(t, "1.26.0", "1.26.0")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("this is not a go.mod\n"), 0o644))

	out, code := runWorkspaceCheck(t, dir)
	assert.Equal(t, 1, code)
	assert.NotContains(t, out, "go.work and go.mod disagree",
		"an unrelated failure must not be reported as the directive mismatch")
	assert.NotEmpty(t, out, "and it must still say something")
}
