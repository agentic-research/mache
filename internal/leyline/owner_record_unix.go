//go:build unix

package leyline

import "syscall"

// processAlive reports whether pid names a live process. Signal 0 delivers
// nothing and only performs the permission/existence check — the classic
// `kill -0`. EPERM means "alive but not ours", which is still alive.
//
// Split per-platform following the procgroup_unix/procgroup_other pattern:
// windows is out of scope today (the tree does not build for it — see
// internal/control's untagged unix.Mmap), but the design goal is "anywhere
// eventually", so the one syscall this feature needs lives behind the same
// seam the eventual port will fill rather than scattered untagged.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
