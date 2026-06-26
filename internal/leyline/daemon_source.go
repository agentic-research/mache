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
