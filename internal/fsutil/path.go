package fsutil

import (
	"os"
	"path/filepath"
)

// DirectoryOrParent returns a file's parent and leaves directories and
// missing paths as supplied.
func DirectoryOrParent(path string) string {
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		return filepath.Dir(path)
	}
	return path
}
