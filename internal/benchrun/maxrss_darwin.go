//go:build darwin

package benchrun

// maxrssUnitBytes converts syscall.Rusage.Maxrss to bytes. Darwin's
// getrusage(2) reports ru_maxrss in BYTES; Linux reports kilobytes. Getting
// this constant wrong is a silent 1024x error in either direction, which is
// why TestRun_MeasuresPeakRSSOfChild asserts a known allocation lands in a
// plausible band rather than merely being non-zero.
// Typed int64 on purpose: syscall.Rusage.Maxrss is int64 on the 64-bit
// darwin/linux targets this package builds for, and a typed constant makes
// an arch where it is not a compile error rather than a silent overflow.
const maxrssUnitBytes int64 = 1
