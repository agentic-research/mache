package leyline

import "sync/atomic"

// daemonSourceDir is the source tree the auto-spawned daemon is started
// against (--source). It is read at spawn time in DiscoverOrStart. The
// daemon needs it to run enrichment passes — op_enrich fails "no --source
// configured" without it — so live LSP/embed enrichment (get_type_info /
// get_diagnostics with file=) only works when this is set (mache-303036).
//
// Empty (the zero value) means "no --source", which is correct for serving
// a pre-baked .db that already carries its own _lsp* tables.
var daemonSourceDir atomic.Pointer[string]

// daemonCDC controls whether the auto-spawned leyline daemon receives
// --cdc. It is deliberately disabled by default: existing and externally
// managed daemons keep their operator-provided startup configuration.
var daemonCDC atomic.Bool

// SetDaemonSource configures the source directory passed to the
// auto-spawned leyline daemon. Call once at serve startup when serving a
// source tree. Safe for concurrent use; takes effect on the next daemon
// spawn (a daemon already running is not restarted).
func SetDaemonSource(dir string) {
	daemonSourceDir.Store(&dir)
}

// DaemonSource returns the configured daemon --source dir, or "" if unset.
func DaemonSource() string {
	if p := daemonSourceDir.Load(); p != nil {
		return *p
	}
	return ""
}

// SetDaemonCDC configures CDC for the auto-spawned leyline daemon. It is
// read at spawn time, so a daemon that is already running is not restarted
// or reconfigured.
func SetDaemonCDC(enabled bool) {
	daemonCDC.Store(enabled)
}

// DaemonCDC reports whether the next auto-spawned leyline daemon should
// receive --cdc.
func DaemonCDC() bool {
	return daemonCDC.Load()
}
