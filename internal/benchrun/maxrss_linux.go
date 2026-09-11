//go:build linux

package benchrun

// maxrssUnitBytes converts syscall.Rusage.Maxrss to bytes. Linux's
// getrusage(2) reports ru_maxrss in KILOBYTES; Darwin reports bytes.
// Typed int64 on purpose: syscall.Rusage.Maxrss is int64 on the 64-bit
// darwin/linux targets this package builds for, and a typed constant makes
// an arch where it is not a compile error rather than a silent overflow.
const maxrssUnitBytes int64 = 1024
