// Package procsig sets the signal dispositions mache's long-running servers
// need to shut down cleanly.
package procsig

import (
	"os/signal"
	"syscall"
)

// SurviveBrokenPipes stops a write to a closed pipe from killing the process.
//
// Go kills a program that writes to a broken pipe on stdout or stderr with
// SIGPIPE unless SIGPIPE is handled or ignored; once ignored, the write just
// returns EPIPE. That default is right for a one-shot CLI (`mache ... | head`
// should exit quietly) and wrong for a server: an MCP client closes the stdio
// pipes when it stops mache, so mache's first shutdown log line killed it
// before it could stop its leyline daemon, and the daemon was orphaned
// (mache-25f49e). A server's cleanup must not depend on someone still reading
// its log.
func SurviveBrokenPipes() {
	signal.Ignore(syscall.SIGPIPE)
}
