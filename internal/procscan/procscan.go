// Package procscan finds live processes by what their command line names.
//
// A test that starts a daemon under a private directory can tell whether the
// daemon outlived it: the daemon's argv names that directory (its arena,
// control file and socket live there), and nothing else on the machine does.
// Asserting on argv rather than on a PID is deliberate: the leak this guards
// against is a process the test never held a handle to, such as a daemon a
// background goroutine spawned, or a child the daemon itself started.
package procscan

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Referencing lists the live processes whose command line contains path, one
// "pid ppid command" line each, excluding the calling process and the ps it
// runs. It lists processes it cannot wait on, so a caller that stopped one
// polls this until it is gone.
func Referencing(path string) ([]string, error) {
	// -e, -o and the empty headers are POSIX; the same line works on macOS
	// and procps. There is no etimes on macOS, so age is not a criterion.
	cmd := exec.Command("ps", "-eo", "pid=,ppid=,command=")
	out, err := cmd.Output()
	if err != nil {
		// An error here must reach the caller: a scan that cannot run and
		// reports "nothing found" would pass every leak check vacuously.
		return nil, fmt.Errorf("scan processes: %w", err)
	}
	self, ps := strconv.Itoa(os.Getpid()), strconv.Itoa(cmd.Process.Pid)
	var hits []string
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, path) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] == self || fields[0] == ps {
			continue
		}
		hits = append(hits, strings.TrimSpace(line))
	}
	return hits, nil
}

// survivorGrace is how long a stopped process gets to disappear from the
// process table. A group signal is asynchronous and a graceful exit takes the
// leyline daemon well under a second, so this is margin, not a budget.
const survivorGrace = 3 * time.Second

// AssertNoneReferencing fails t if any process still names path once a short
// grace has passed. Register it as the FIRST cleanup of a test that starts a
// daemon under path, so it runs after the cleanups that stop the daemon and
// remove the directory.
func AssertNoneReferencing(t testing.TB, path string) {
	t.Helper()
	noneReferencingWithin(t, path, survivorGrace)
}

func noneReferencingWithin(t testing.TB, path string, grace time.Duration) {
	t.Helper()
	// The condition runs in assert.Eventually's goroutine; what it last saw
	// is read here after it stopped, under the same lock, so the report
	// names the survivors without a data race on the timeout path.
	var (
		mu       sync.Mutex
		lastHits []string
		lastErr  error
	)
	gone := assert.Eventually(t, func() bool {
		hits, err := Referencing(path)
		mu.Lock()
		defer mu.Unlock()
		lastHits, lastErr = hits, err
		return err == nil && len(hits) == 0
	}, grace, 100*time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	require.NoError(t, lastErr, "cannot check for processes that survived the test")
	if !gone {
		t.Errorf("%d process(es) still name %s %s after the test's cleanup — the daemon "+
			"started under it was not stopped, or was stopped without its process group:\n  %s",
			len(lastHits), path, grace, strings.Join(lastHits, "\n  "))
	}
}
