//go:build !unix

package leyline

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup is a no-op on non-unix platforms (no POSIX process
// groups). mache targets darwin/linux; this stub only keeps the package
// compiling elsewhere.
func setProcessGroup(_ *exec.Cmd) {}

// signalProcessGroup degrades to signaling just the leader process on
// platforms without POSIX process groups.
func signalProcessGroup(proc *os.Process, sig syscall.Signal) error {
	if proc == nil {
		return os.ErrProcessDone
	}
	return proc.Signal(sig)
}
