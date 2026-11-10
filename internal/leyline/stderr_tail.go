package leyline

import (
	"strings"
	"sync"
)

// stderrTail keeps the last max bytes written to it: the managed daemon's
// stderr, so a daemon that dies at startup is reported with its own reason.
// The daemon runs for the whole session, so the pipe has to be drained for as
// long as it lives; a bounded tail keeps that from growing without limit.
// Without it the only report was "exit status 1", and a control block left by
// an older leyline ("control block VERSION mismatch") stopped every spawn with
// nothing saying why (mache-e70109).
type stderrTail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newStderrTail(max int) *stderrTail { return &stderrTail{max: max} }

func (t *stderrTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

// String is the kept output with surrounding whitespace trimmed.
func (t *stderrTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}
