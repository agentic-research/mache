package procscan

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// holdPath starts a process whose argv names path and returns it: `tail -f`
// on a file at path blocks until killed. (A shell given `-c 'sleep 30' path`
// does not do: it execs the single command and drops its own argv.)
func holdPath(t *testing.T, path string) *exec.Cmd {
	t.Helper()
	require.NoError(t, os.WriteFile(path, nil, 0o644))
	cmd := exec.Command("tail", "-f", path)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

func TestReferencing_FindsTheProcessNamingThePathAndNoOther(t *testing.T) {
	path := filepath.Join(t.TempDir(), "held")
	cmd := holdPath(t, path)

	hits, err := Referencing(path)
	require.NoError(t, err)
	require.Len(t, hits, 1, "exactly the holder names the path: %v", hits)
	fields := strings.Fields(hits[0])
	assert.Equal(t, strconv.Itoa(cmd.Process.Pid), fields[0], "pid is the first field")
	assert.Contains(t, hits[0], path)

	none, err := Referencing(filepath.Join(t.TempDir(), "never-named"))
	require.NoError(t, err)
	assert.Empty(t, none)
}

// recordingTB keeps the failures a check reports instead of ending the test,
// so a check can be shown to fail.
type recordingTB struct {
	testing.TB
	failures []string
}

func (r *recordingTB) Helper()                           {}
func (r *recordingTB) Errorf(format string, args ...any) { r.failures = append(r.failures, format) }
func (r *recordingTB) FailNow()                          { r.failures = append(r.failures, "FailNow") }

func TestAssertNoneReferencing_PassesOnceTheHolderIsGoneAndFailsWhileItLives(t *testing.T) {
	path := filepath.Join(t.TempDir(), "held")
	cmd := holdPath(t, path)

	living := &recordingTB{TB: t}
	noneReferencingWithin(living, path, 300*time.Millisecond)
	require.NotEmpty(t, living.failures, "a live process naming the path must fail the check")
	assert.Contains(t, strings.Join(living.failures, "\n"), "still name")

	require.NoError(t, cmd.Process.Kill())
	_ = cmd.Wait()
	gone := &recordingTB{TB: t}
	AssertNoneReferencing(gone, path)
	assert.Empty(t, gone.failures, "the check must pass once nothing names the path")
}
