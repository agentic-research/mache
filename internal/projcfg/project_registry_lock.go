package projcfg

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// The project registry is a read-modify-write over a single JSON file, and it
// has TWO independent writers to serialize against.
//
// In-process: a shared HTTP daemon resolves sessions concurrently — one
// goroutine per request through wrapHandler — so many sessions can each learn
// a different root at the same time. Measured without a lock: 50 goroutines
// registering 50 distinct roots left 1-3 of them in the file. Not corruption
// (WriteFileAtomic renames, so the file is never torn) but near-total silent
// LOSS, which defeats the point of registering at all: the token a later
// ?project= lookup needs was simply never written.
//
// Cross-process: `mache init`, a supervised `mache serve --http`, and any
// number of `mache serve --stdio` subprocesses are separate processes sharing
// ~/.mache/projects.json. A mutex cannot see across them. flock can.
//
// Both are required. A mutex alone leaves the multi-process case, and flock
// alone would serialize processes while goroutines inside one process still
// raced on the same descriptor.
var registryMu sync.Mutex

// registryLockPath is a dedicated lock file, never the registry itself.
// Locking the registry would mean holding a descriptor to a file that
// WriteFileAtomic replaces by rename — the lock would end up held on an
// unlinked inode while a new one took its place, protecting nothing.
func registryLockPath() (string, error) {
	dir, err := macheHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, projectRegistryFile+".lock"), nil
}

// withRegistryLock runs fn holding both the in-process mutex and an exclusive
// advisory file lock, so a read-modify-write of the registry is atomic against
// every other mache goroutine and every other mache process.
//
// LOCK_EX blocks rather than failing: these critical sections are a file read,
// a map insert, and a rename — microseconds — so waiting is correct and
// starvation is not a realistic concern. Failing fast would just reintroduce
// the lost-update the lock exists to prevent.
func withRegistryLock(fn func() error) error {
	registryMu.Lock()
	defer registryMu.Unlock()

	path, err := registryLockPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create registry lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open registry lock: %w", err)
	}
	defer func() { _ = f.Close() }()

	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock registry: %w", err)
	}
	defer func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }()

	return fn()
}
