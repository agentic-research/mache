package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMask_KeepsStableValuesAndMasksTheRest(t *testing.T) {
	a := map[string]any{"stable": 1.0, "clock": "10:00", "list": []any{"x", "y"}}
	b := map[string]any{"stable": 1.0, "clock": "10:01", "list": []any{"x", "z"}}
	assert.Equal(t, map[string]any{"stable": 1.0, "clock": noise, "list": []any{"x", noise}}, mask(a, b))
}

// A field present in one run and not another is noise in both directions:
// whichever run lacks it, the key is masked rather than reported.
func TestMask_KeyPresenceThatVariesIsNoise(t *testing.T) {
	with := map[string]any{"state": "up", "reason": "dial failed"}
	without := map[string]any{"state": "up"}
	assert.Equal(t, map[string]any{"state": "up", "reason": noise}, mask(with, without))
	assert.Equal(t, map[string]any{"state": "up", "reason": noise}, mask(without, with))
}

// An array whose LENGTH changes cannot be compared index by index, so the
// whole array is noise; an equal array survives intact.
func TestMask_ArraysOfDifferentLengthAreMaskedWhole(t *testing.T) {
	assert.Equal(t, noise, mask([]any{1.0}, []any{1.0, 2.0}))
	assert.Equal(t, []any{1.0, 2.0}, mask([]any{1.0, 2.0}, []any{1.0, 2.0}))
}

// Folding a third run in keeps everything earlier runs already masked.
func TestMask_FoldsAcrossRuns(t *testing.T) {
	r1 := map[string]any{"a": 1.0, "b": 1.0}
	r2 := map[string]any{"a": 2.0, "b": 1.0}
	r3 := map[string]any{"a": 1.0, "b": 3.0}
	assert.Equal(t, map[string]any{"a": noise, "b": noise}, mask(mask(r1, r2), r3))
}

func TestMaskEither_NoiseOnOneSideIsNoiseOnBoth(t *testing.T) {
	base := map[string]any{"v": noise, "same": "x", "only": noise}
	head := map[string]any{"v": "real", "same": "x"}
	gotBase, gotHead := maskEither(base, head)
	assert.Equal(t, map[string]any{"v": noise, "same": "x"}, gotBase,
		"a key that is noise on one side and absent on the other must be dropped, not reported")
	assert.Equal(t, map[string]any{"v": noise, "same": "x"}, gotHead)
}

// A real difference is NOT masked: only within-version variation is.
func TestMaskEither_LeavesRealDifferences(t *testing.T) {
	gotBase, gotHead := maskEither(map[string]any{"v": "old"}, map[string]any{"v": "new"})
	assert.Equal(t, map[string]any{"v": "old"}, gotBase)
	assert.Equal(t, map[string]any{"v": "new"}, gotHead)
}

func TestUncoveredTools(t *testing.T) {
	tools := []mcp.Tool{{Name: "search"}, {Name: "read_file"}, {Name: "write_file"}}
	plan := []call{{Tool: "search"}, {Tool: "read_file"}, {Tool: "read_file"}}
	assert.Equal(t, []string{"write_file"}, uncoveredTools(tools, plan))
	assert.Empty(t, uncoveredTools(tools, append(plan, call{Tool: "write_file"})))
}

// mache reports canonical paths, so on macOS a corpus under /var arrives as
// /private/var. Both spellings must normalize, or every path-bearing answer
// differs between runs that used different temp directories.
func TestNormalizeText_ReplacesBothPathSpellings(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "corpus")
	require.NoError(t, os.Mkdir(root, 0o755))
	resolved, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)

	got := normalizeText(`{"a":"`+root+`/x.go","b":"`+resolved+`/y.go","c":"`+home+`/.mache"}`, root, home)
	assert.Equal(t, map[string]any{"a": "$CORPUS/x.go", "b": "$CORPUS/y.go", "c": "$HOME/.mache"}, got)
}

func TestNormalizeText_NonJSONStaysText(t *testing.T) {
	assert.Equal(t, "not found: $CORPUS/x", normalizeText("not found: /c/x", "/c", "/h"))
}

// A process still naming the session's private HOME after shutdown is a leak:
// reapSurvivors must report it AND kill it, so it cannot outlive the run.
func TestReapSurvivors_ReportsAndKillsALeak(t *testing.T) {
	home := t.TempDir()
	// The shell's own argv carries the marker ($0), and it blocks in the read
	// builtin on a pipe nobody writes, so no child process stands in for it.
	leak := exec.Command("/bin/sh", "-c", "read x", "leak-under-"+home)
	stdin, err := leak.StdinPipe()
	require.NoError(t, err)
	t.Cleanup(func() { _ = stdin.Close() })
	require.NoError(t, leak.Start())
	exited := make(chan struct{})
	go func() { _ = leak.Wait(); close(exited) }()
	t.Cleanup(func() { _ = leak.Process.Kill() })

	err = reapSurvivors(home, 200*time.Millisecond)

	require.Error(t, err, "a process naming the session's HOME must be reported")
	assert.Contains(t, err.Error(), strconv.Itoa(leak.Process.Pid))
	require.Eventually(t, func() bool {
		select {
		case <-exited:
			return true
		default:
			return false
		}
	}, 5*time.Second, 10*time.Millisecond, "the leaked process must be killed")
}

func TestReapSurvivors_CleanShutdownIsSilent(t *testing.T) {
	assert.NoError(t, reapSurvivors(t.TempDir(), 200*time.Millisecond))
}

// The gate judges the candidate. A base release that leaks (v0.21.1 does) is
// a regression it already shipped: noted, not fatal. A head leak, or any other
// base failure, must still stop the run.
func TestClassifyRunError(t *testing.T) {
	leak := &leakError{"left 2 process(es) running"}
	// answerPlan joins the close error onto its own, so the leak arrives wrapped.
	wrapped := errors.Join(nil, leak)

	note, fatal := classifyRunError("base", wrapped)
	assert.NoError(t, fatal, "a base leak must not abort the comparison")
	assert.Contains(t, note, "left 2 process(es) running")

	_, fatal = classifyRunError("head", wrapped)
	require.Error(t, fatal, "a head leak is a regression in the release being gated")

	_, fatal = classifyRunError("base", errors.New("tools/list: broken pipe"))
	require.Error(t, fatal, "a base that cannot answer cannot be compared")

	note, fatal = classifyRunError("base", nil)
	assert.Empty(t, note)
	assert.NoError(t, fatal)
}
