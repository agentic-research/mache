package procsig

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHelperWriteToClosedStderr is not a test: it is the child the tests below
// re-execute. It waits for a line on stdin (sent once the parent has closed
// the read end of the child's stderr), writes to stderr, and exits 0 if it
// survived the write.
func TestHelperWriteToClosedStderr(t *testing.T) {
	mode := os.Getenv("PROCSIG_HELPER")
	if mode == "" {
		t.Skip("helper process")
	}
	if mode == "survive" {
		SurviveBrokenPipes()
	}
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	fmt.Fprintln(os.Stderr, "shutting down")
	os.Exit(0)
}

// writeAfterReaderGone runs the helper with its stderr on a pipe whose read
// end is closed before the helper writes, and returns how the helper ended.
func writeAfterReaderGone(t *testing.T, mode string) error {
	t.Helper()
	self, err := os.Executable()
	require.NoError(t, err)
	r, w, err := os.Pipe()
	require.NoError(t, err)

	cmd := exec.Command(self, "-test.run=^TestHelperWriteToClosedStderr$")
	cmd.Env = append(os.Environ(), "PROCSIG_HELPER="+mode)
	cmd.Stderr = w
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	_ = w.Close()

	require.NoError(t, r.Close(), "the reader goes away, as an MCP client's does on shutdown")
	_, err = fmt.Fprintln(stdin, "go")
	require.NoError(t, err)
	_ = stdin.Close()
	return cmd.Wait()
}

func TestSurviveBrokenPipes_AWriteToAClosedStderrIsNotFatal(t *testing.T) {
	assert.NoError(t, writeAfterReaderGone(t, "survive"),
		"with SurviveBrokenPipes, writing to a closed stderr must return an error, not kill the process")
}

// The control: without it, the same write kills the process. If this ever
// passes, the test above proves nothing.
func TestSurviveBrokenPipes_WithoutItTheWriteIsFatal(t *testing.T) {
	err := writeAfterReaderGone(t, "default")
	var exit *exec.ExitError
	require.True(t, errors.As(err, &exit), "expected the helper to be killed, got %v", err)
	status, ok := exit.Sys().(syscall.WaitStatus)
	require.True(t, ok)
	assert.True(t, status.Signaled() && status.Signal() == syscall.SIGPIPE,
		"expected death by SIGPIPE, got %v", exit)
}
