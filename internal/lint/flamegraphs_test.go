package lint

// Tests for scripts/flamegraphs.sh — the profiling artifact renderer
// (mache-77e389).
//
// The defect these pin is measurement integrity, not cosmetics: the previous
// inline version discarded pprof's stderr and rendered every empty result as
// an SVG asserting a specific benign cause ("tool too fast for the 100Hz
// sampler"). A corrupt profile therefore produced an artifact affirmatively
// telling the reader nothing was wrong, while the task exited 0 and INDEX.md
// linked it. The whole point of the artifact is to be evidence.
//
// stackcollapse-go.pl and flamegraph.pl are stubbed rather than required, so
// these run on any machine: what is under test is the script's failure
// HANDLING, not brendangregg's renderers.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"testing"

	"github.com/agentic-research/mache/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubRenderers puts fake stackcollapse-go.pl and flamegraph.pl on PATH.
// collapseOut is what the stub collapser emits (empty models a profile with
// no samples to draw).
func stubRenderers(t *testing.T, collapseOut string) {
	t.Helper()
	bin := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(bin, name)
		require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755))
	}
	write("stackcollapse-go.pl", "cat >/dev/null; printf '%s' "+shellQuote(collapseOut))
	write("flamegraph.pl", "cat >/dev/null; echo '<svg>rendered</svg>'")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// writeValidCPUProfile writes a real, parseable CPU profile containing zero
// samples — the benign case the placeholder legitimately exists for.
func writeValidCPUProfile(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, pprof.StartCPUProfile(f))
	pprof.StopCPUProfile()
	require.NoError(t, f.Close())
}

func runFlamegraphs(t *testing.T, dir string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash",
		filepath.Join(testutil.MacheRepoRoot(t), "scripts", "flamegraphs.sh"), dir)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestFlamegraphs_CorruptProfileFailsLoudly is the defect itself: a profile
// pprof cannot parse must fail the run and show pprof's own words, not
// silently become an SVG claiming the tool was too fast to sample.
func TestFlamegraphs_CorruptProfileFailsLoudly(t *testing.T) {
	stubRenderers(t, "main;work 10")
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "toolx.cpu.pprof"),
		[]byte("this is not a pprof file"), 0o644))

	out, err := runFlamegraphs(t, dir)

	require.Error(t, err, "a corrupt profile must fail the run, not be rendered as a benign note")
	assert.Contains(t, out, "toolx.cpu.pprof", "the failing profile must be named")
	assert.Contains(t, out, "unrecognized profile format",
		"pprof's own stderr must survive — discarding it is what made this undiagnosable")
	assert.NotContains(t, out, "too fast",
		"the run must not assert a benign cause it did not establish")

	if data, rerr := os.ReadFile(filepath.Join(dir, "toolx.cpu.svg")); rerr == nil {
		assert.NotContains(t, string(data), "no CPU samples",
			"a failed render must not leave an artifact claiming nothing was wrong")
	}
}

// TestFlamegraphs_ValidButSampleFreeProfileIsBenign is the other half, and the
// reason the fix cannot simply be "fail on empty output": a profile that
// parses cleanly and holds no samples is a real, ordinary outcome. It must
// still produce a placeholder and exit 0.
func TestFlamegraphs_ValidButSampleFreeProfileIsBenign(t *testing.T) {
	stubRenderers(t, "") // collapses to nothing: valid profile, nothing to draw
	dir := t.TempDir()
	writeValidCPUProfile(t, filepath.Join(dir, "tooly.cpu.pprof"))

	out, err := runFlamegraphs(t, dir)

	require.NoError(t, err, "a valid profile with no samples is not a failure: %s", out)
	data, rerr := os.ReadFile(filepath.Join(dir, "tooly.cpu.svg"))
	require.NoError(t, rerr, "the placeholder artifact must still be written")
	assert.Contains(t, string(data), "zero CPU samples")
	assert.NotContains(t, string(data), "too fast",
		"the placeholder must report what is known, not guess why")
}

// TestFlamegraphs_RendersWhenThereIsSomethingToDraw guards the happy path, so
// the failure handling above cannot be satisfied by a script that never
// renders anything.
func TestFlamegraphs_RendersWhenThereIsSomethingToDraw(t *testing.T) {
	stubRenderers(t, "main;work 10")
	dir := t.TempDir()
	writeValidCPUProfile(t, filepath.Join(dir, "toolz.cpu.pprof"))

	out, err := runFlamegraphs(t, dir)
	require.NoError(t, err, out)

	data, rerr := os.ReadFile(filepath.Join(dir, "toolz.cpu.svg"))
	require.NoError(t, rerr)
	assert.Contains(t, string(data), "rendered", "the collapsed stacks must reach flamegraph.pl")
}

// TestFlamegraphs_MissingHeapBaselineIsAFailure: without a baseline the delta
// cannot be computed, so an artifact implying it was ("no heap delta — loop
// allocations were fully GC'd") is the same manufactured reassurance.
func TestFlamegraphs_MissingHeapBaselineIsAFailure(t *testing.T) {
	stubRenderers(t, "main;alloc 10")
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "toolh.heap.pprof"), []byte("x"), 0o644))

	out, err := runFlamegraphs(t, dir)

	require.Error(t, err, "a missing baseline must fail rather than render a benign note")
	assert.Contains(t, out, "missing baseline")
	assert.NoFileExists(t, filepath.Join(dir, "toolh.heap.svg"))
}
