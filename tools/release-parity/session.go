package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// session serves a private copy of the corpus with bin over stdio, under a
// private HOME. The copy matters because write_file edits the corpus: without
// it, one run's edits would become the next run's input.
type session struct {
	c    *client.Client
	home string
	root string
}

// serverLog is where a session's server stderr is kept, inside its private
// HOME, so a leak report can show how the leaked process came to be started.
func (s *session) serverLog() string { return filepath.Join(s.home, "server.log") }

// logTail returns the last n lines of the server's log.
func (s *session) logTail(n int) string {
	b, err := os.ReadFile(s.serverLog())
	if err != nil {
		return "(no server log: " + err.Error() + ")"
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}

func openSession(ctx context.Context, bin, corpus string) (*session, error) {
	home, err := isolatedHome()
	if err != nil {
		return nil, err
	}
	root := filepath.Join(home, "corpus")
	if err := os.CopyFS(root, os.DirFS(corpus)); err != nil {
		_ = os.RemoveAll(home)
		return nil, fmt.Errorf("copy corpus: %w", err)
	}
	env := append(os.Environ(), "HOME="+home)
	args := []string{"serve", "--stdio"}
	if schema != "" {
		args = append(args, "--schema", schema)
	}
	c, err := client.NewStdioMCPClient(bin, env, append(args, root)...)
	if err != nil {
		_ = os.RemoveAll(home)
		return nil, err
	}
	if err := initializeSession(ctx, c); err != nil {
		_ = c.Close()
		_ = os.RemoveAll(home)
		return nil, fmt.Errorf("initialize: %w", err)
	}
	s := &session{c: c, home: home, root: root}
	if r, ok := client.GetStderr(c); ok {
		if f, err := os.Create(s.serverLog()); err == nil {
			go func() {
				_, _ = io.Copy(f, r)
				_ = f.Close()
			}()
		}
	}
	return s, nil
}

// close shuts the session down and fails if the binary left anything running.
//
// The parity run starts real daemons, and a binary that orphans them is a
// regression a release gate must catch: unnoticed, they accumulated on one
// host until ley-line-open counted 81 (mache-25f49e). Everything a session
// starts references its private HOME, a path unique to the session, so
// anything still matching it after shutdown is a leak. It is killed so it
// cannot outlive the run, and reported so the run fails.
func (s *session) close() error {
	_ = s.c.Close()
	err := reapSurvivors(s.home, survivorGrace)
	if err != nil {
		err = fmt.Errorf("%w\nlast lines of the server's log:\n%s", err, s.logTail(25))
	}
	_ = os.RemoveAll(s.home)
	return err
}

// survivorGrace is how long a shut-down session's processes get to exit. mache
// stops its managed daemon on shutdown, and the daemon gets 3s to exit
// gracefully before mache kills it, so this allows that plus margin.
const survivorGrace = 10 * time.Second

// reapSurvivors waits up to grace for every process whose command line names
// home to exit, then SIGKILLs any that remain and returns an error naming
// them. The wait polls, because these processes are not this one's children
// and cannot be waited on.
func reapSurvivors(home string, grace time.Duration) error {
	deadline := time.Now().Add(grace)
	left := processesReferencing(home)
	for len(left) > 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		left = processesReferencing(home)
	}
	if len(left) == 0 {
		return nil
	}
	described := make([]string, 0, len(left))
	for _, pid := range left {
		described = append(described, describeProcess(pid))
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	return fmt.Errorf("the binary left %d process(es) running %s after shutdown; killed them:\n  %s",
		len(left), grace, strings.Join(described, "\n  "))
}

// describeProcess is "pid ppid elapsed command", so a leak report says what
// leaked and who started it.
func describeProcess(pid int) string {
	out, err := exec.Command("ps", "-o", "pid=,ppid=,etime=,command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return strconv.Itoa(pid)
	}
	return strings.TrimSpace(string(out))
}

// processesReferencing returns the pids whose command line contains path.
func processesReferencing(path string) []int {
	out, err := exec.Command("pgrep", "-f", path).Output()
	if err != nil {
		return nil // pgrep exits 1 when nothing matches
	}
	var pids []int
	for _, f := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(f); err == nil && pid != os.Getpid() {
			pids = append(pids, pid)
		}
	}
	return pids
}

// isolatedHome makes a HOME whose ~/.mache/bin is the real one, so each binary
// resolves its own pinned leyline without downloading, and whose parse cache
// and daemon state are private to this run.
func isolatedHome() (string, error) {
	// Under /tmp, not TMPDIR: the managed leyline daemon binds a unix socket
	// beneath HOME, macOS caps socket paths at 104 bytes, and TMPDIR there is a
	// long /var/folders/... path. A run under it measures a socket-length
	// failure no typical HOME has, not the code under test (mache-aab4f0).
	home, err := os.MkdirTemp("/tmp", "rp-")
	if err != nil {
		return "", err
	}
	real, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(home, ".mache"), 0o755); err != nil {
		return "", err
	}
	bin := filepath.Join(real, ".mache", "bin")
	if _, err := os.Stat(bin); err == nil {
		if err := os.Symlink(bin, filepath.Join(home, ".mache", "bin")); err != nil {
			return "", err
		}
	}
	return home, nil
}

// initializeSession performs the MCP initialize handshake.
func initializeSession(ctx context.Context, c *client.Client) error {
	init := mcp.InitializeRequest{}
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: "release-parity", Version: "1"}
	_, err := c.Initialize(ctx, init)
	return err
}
