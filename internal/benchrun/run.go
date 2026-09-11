// Package benchrun measures what running a command costs: wall time and peak
// resident set size. It is the shared measurement core under `task bench:cold`
// (mache-2de6c0) and the scale gate that will follow (mache-543943).
//
// Peak RSS is the point. The cold-path budget is "does a first `mache build`
// fit a 4-core/16 GB laptop with an IDE already open", and wall time cannot
// answer that: a run that pages is slow for a reason no timing number
// attributes. Bytes are also machine-independent in a way milliseconds are
// not, which is why the hard gate is on memory and artifact size while wall
// time stays advisory.
package benchrun

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Measurement is one command's cost.
type Measurement struct {
	// Command is the command line, for reporting.
	Command string
	// Wall is elapsed real time — what the user waits.
	Wall time.Duration
	// PeakRSS is the high-water resident set size in bytes.
	//
	// It comes from the rusage wait4 returns for the child, which folds in
	// any descendant the child itself waited for — so `mache build`'s number
	// covers the `leyline parse` it spawns. That fold is a MAX, not a sum:
	// it answers "was any single process ever this big", not "how much was
	// resident at once". For this harness the two coincide, because mache
	// runs the parse to completion before it projects. A phase that ran them
	// concurrently would need a sampler instead.
	PeakRSS int64
}

// Run executes name with args, writing the command's own output to out, and
// reports what it cost. env entries are appended to the current environment
// (later entries win, which is how exec applies them).
//
// A non-zero exit is returned as an error AND measured: a run that died after
// allocating 4 GB is exactly the data point the budget exists to catch, so the
// Measurement is always usable.
func Run(out io.Writer, env []string, name string, args ...string) (Measurement, error) {
	cmd := exec.Command(name, args...)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Env = append(os.Environ(), env...)

	start := time.Now()
	runErr := cmd.Run()
	m := Measurement{
		Command: strings.Join(append([]string{name}, args...), " "),
		Wall:    time.Since(start),
	}
	if cmd.ProcessState != nil {
		if ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
			m.PeakRSS = ru.Maxrss * maxrssUnitBytes
		}
	}
	if runErr != nil {
		return m, fmt.Errorf("%s: %w", name, runErr)
	}
	return m, nil
}

// FileBytes is the on-disk size of path plus its SQLite sidecars. A .db whose
// pages are still in a 300 MB write-ahead log has not stopped costing the
// disk 300 MB, so the sidecars count toward the artifact budget.
func FileBytes(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	total := info.Size()
	for _, suffix := range []string{"-wal", "-shm"} {
		if side, serr := os.Stat(path + suffix); serr == nil {
			total += side.Size()
		}
	}
	return total, nil
}

// HumanBytes renders a byte count the way the budget is written and argued
// about — decimal GB, two decimals — so a reported number and a budget number
// can be compared by eye without a unit conversion in the reader's head.
func HumanBytes(n int64) string {
	const gb = 1_000_000_000
	const mb = 1_000_000
	switch {
	case n >= gb:
		return fmt.Sprintf("%.2f GB", float64(n)/gb)
	case n >= mb:
		return fmt.Sprintf("%.1f MB", float64(n)/mb)
	default:
		return fmt.Sprintf("%d B", n)
	}
}
