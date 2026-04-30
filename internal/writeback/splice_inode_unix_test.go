//go:build unix

package writeback

import (
	"os"
	"syscall"
	"testing"
)

// fileInode returns the platform inode for path. Calls t.Fatalf if Stat
// fails (unix tests assume the file exists — stat failure is a setup bug,
// not a platform-capability gap). Returns 0 if Sys() doesn't return a
// *syscall.Stat_t — the caller treats 0 as "platform doesn't expose
// inodes" and skips.
func fileInode(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(st.Ino) //nolint:unconvert // Ino is uint64 on most unixen but uint32 on some
}
