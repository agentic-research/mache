package projcfg

import (
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to path via a temp file + rename, so readers
// never observe a partial write. Re-homed here from cmd/cache.go (R1,
// mache-96c378): the project registry is this package's own state and must
// not reach upward into the build cache for its write primitive — that edge
// was one of the two would-be import cycles the decomposition had to break.
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}
