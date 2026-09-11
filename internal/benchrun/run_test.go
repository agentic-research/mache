package benchrun

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// allocSrc is a program that touches a known amount of memory and holds it
// until exit. Touching matters: an untouched allocation is not resident, so a
// program that only makes it would prove nothing about peak RSS.
const allocSrc = `package main

import "os"

func main() {
	const mb = 1 << 20
	buf := make([]byte, 256*mb)
	for i := range buf {
		buf[i] = byte(i)
	}
	os.Stdout.Write(buf[:1])
}
`

// buildAllocator compiles allocSrc and returns the binary path.
func buildAllocator(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(src, []byte(allocSrc), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module alloc\n\ngo 1.22\n"), 0o644))
	bin := filepath.Join(dir, "alloc")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "build allocator: %s", out)
	return bin
}

// TestRun_MeasuresPeakRSSOfChild is the test that makes maxrssUnitBytes
// falsifiable. A child that touches 256 MB must report a peak in that
// neighbourhood — not zero, and not 1024x off in either direction, which is
// exactly what a wrong unit constant looks like.
func TestRun_MeasuresPeakRSSOfChild(t *testing.T) {
	const mb = 1 << 20
	m, err := Run(io.Discard, nil, buildAllocator(t))
	require.NoError(t, err)

	assert.Greater(t, m.PeakRSS, int64(200*mb),
		"a child that touched 256 MB reported %s; the Maxrss unit for this OS is wrong (too small) or the field is unread",
		HumanBytes(m.PeakRSS))
	assert.Less(t, m.PeakRSS, int64(4096*mb),
		"a child that touched 256 MB reported %s; the Maxrss unit for this OS is wrong (too large)",
		HumanBytes(m.PeakRSS))
	assert.Positive(t, m.Wall)
}

// TestRun_CapturesOutputAndEnv pins the two things a measured command needs
// from the harness: its output is routed where the caller asked, and the env
// the caller sets reaches it (GOMAXPROCS is how the 4-core envelope is
// applied, so an env that silently did not arrive would make every cold-path
// number a measurement of the wrong machine).
func TestRun_CapturesOutputAndEnv(t *testing.T) {
	var buf bytes.Buffer
	_, err := Run(&buf, []string{"BENCHRUN_PROBE=applied"}, "sh", "-c", "echo $BENCHRUN_PROBE")
	require.NoError(t, err)
	assert.Equal(t, "applied\n", buf.String())
}

// TestRun_FailedCommandIsStillMeasured: a run that exits non-zero after
// allocating is the data point the budget exists to catch, so the error must
// not discard the measurement.
func TestRun_FailedCommandIsStillMeasured(t *testing.T) {
	m, err := Run(io.Discard, nil, "sh", "-c", "exit 3")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exit status 3")
	assert.Positive(t, m.Wall, "a failed command still took time; the measurement must survive the error")
	assert.NotEmpty(t, m.Command)
}

// TestFileBytes_CountsSQLiteSidecars: a .db whose pages are still in a write
// -ahead log has not stopped costing the disk, so -wal and -shm count toward
// the artifact budget.
func TestFileBytes_CountsSQLiteSidecars(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "x.db")
	require.NoError(t, os.WriteFile(db, make([]byte, 100), 0o644))

	got, err := FileBytes(db)
	require.NoError(t, err)
	assert.Equal(t, int64(100), got)

	require.NoError(t, os.WriteFile(db+"-wal", make([]byte, 50), 0o644))
	require.NoError(t, os.WriteFile(db+"-shm", make([]byte, 7), 0o644))
	got, err = FileBytes(db)
	require.NoError(t, err)
	assert.Equal(t, int64(157), got, "a 50-byte WAL and a 7-byte shm are part of what the artifact costs")

	_, err = FileBytes(filepath.Join(dir, "absent.db"))
	assert.Error(t, err, "a missing artifact is an error, not a free one")
}

func TestHumanBytes(t *testing.T) {
	assert.Equal(t, "2.00 GB", HumanBytes(2_000_000_000))
	assert.Equal(t, "1.05 GB", HumanBytes(1_050_000_000))
	assert.Equal(t, "60.0 MB", HumanBytes(60_000_000))
	assert.Equal(t, "512 B", HumanBytes(512))
}
