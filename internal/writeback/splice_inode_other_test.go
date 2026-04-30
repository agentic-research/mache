//go:build !unix

package writeback

import "testing"

// fileInode is a no-op on platforms where os.FileInfo.Sys() doesn't
// expose an inode number (e.g., Windows). The test that calls this
// will skip when 0 is returned.
func fileInode(t *testing.T, path string) uint64 {
	t.Helper()
	_ = path
	return 0
}
