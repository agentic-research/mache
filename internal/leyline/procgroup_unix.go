//go:build unix

package leyline

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup makes the spawned command a process-group leader, so its
// own children (e.g. the `mache serve --control` that `leyline daemon`
// spawns) inherit the group. signalProcessGroup can then reap the whole
// tree instead of orphaning the grandchild (mache-823d91).
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// signalProcessGroup delivers sig to the entire process group led by proc.
// Because proc was started with setProcessGroup, its PID is also its PGID,
// so a negative-PID kill targets the daemon AND every child it spawned.
// Falls back to signaling just proc if the group send fails (e.g. the
// leader already exited and the group is gone). Returns the underlying
// error so callers can detect an already-dead group.
func signalProcessGroup(proc *os.Process, sig syscall.Signal) error {
	if proc == nil {
		return os.ErrProcessDone
	}
	if err := syscall.Kill(-proc.Pid, sig); err != nil {
		// Group gone (leader already reaped) — try the leader directly so
		// callers still get a meaningful result.
		return proc.Signal(sig)
	}
	return nil
}
