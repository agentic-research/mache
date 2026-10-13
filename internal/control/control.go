package control

import (
	"crypto/subtle"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Controller manages the memory-mapped control file.
type Controller struct {
	path string
	file *os.File
	// probe is a second open of the control file, on which a reader tests the
	// publish lock. flock(2) locks belong to an open file description, so a
	// shared lock taken on file itself would quietly convert this process's
	// own exclusive publish lock instead of contending with it.
	probe *os.File
	data  []byte
}

// OpenOrCreate opens or creates a control file at the given path.
// Rejects mismatched VERSION with a clear error so old mache binaries
// reading new control blocks (or vice versa) fail loudly rather than
// silently misinterpreting the layout.
func OpenOrCreate(path string) (*Controller, error) {
	if err := validatePath(path); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open control file: %w", err)
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat: %w", err)
	}

	if info.Size() < ControlSize {
		if err := f.Truncate(ControlSize); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("truncate: %w", err)
		}
	}

	data, err := unix.Mmap(int(f.Fd()), 0, ControlSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("mmap: %w", err)
	}

	existingMagic := *(*uint32)(unsafe.Pointer(&data[offMagic]))
	switch {
	case existingMagic == 0:
		*(*uint32)(unsafe.Pointer(&data[offMagic])) = Magic
		*(*uint32)(unsafe.Pointer(&data[offVersion])) = Version
	case existingMagic != Magic:
		_ = unix.Munmap(data)
		_ = f.Close()
		return nil, fmt.Errorf("invalid control block magic: 0x%08X", existingMagic)
	default:
		existingVersion := *(*uint32)(unsafe.Pointer(&data[offVersion]))
		if existingVersion != Version {
			_ = unix.Munmap(data)
			_ = f.Close()
			return nil, fmt.Errorf(
				"control block version mismatch: file is v%d, expected v%d — "+
					"v3 publishes under a seqlock (ley-line-open ≥ 0.20.1); "+
					"remove the stale .ctrl file and let a current writer recreate it",
				existingVersion, Version,
			)
		}
	}

	probe, err := os.Open(path)
	if err != nil {
		_ = unix.Munmap(data)
		_ = f.Close()
		return nil, fmt.Errorf("open control file for the publish lock: %w", err)
	}
	return &Controller{path: path, file: f, probe: probe, data: data}, nil
}

// GetCurrentRoot returns the BLAKE3 root hash of the arena payload that
// the writer most recently published, or the zero sentinel `[32]byte{}`
// when no snapshot has been published yet. The root is always one some
// publish wrote in full. The consistency is per call: a following
// GetArenaPath is its own snapshot and may reflect a later publish.
func (c *Controller) GetCurrentRoot() [32]byte {
	return c.snapshot().root
}

// GetArenaPath returns the path to the currently active arena.
func (c *Controller) GetArenaPath() string {
	b := c.snapshot().path
	for i, v := range b {
		if v == 0 {
			return string(b[:i])
		}
	}
	return string(b[:])
}

// GetArenaSize returns the size in bytes of the currently active arena file.
func (c *Controller) GetArenaSize() uint64 {
	return c.snapshot().size
}

// SetArena points the control block at a new arena without changing
// `current_root`: a re-advertisement, which polling readers (who compare
// roots) do not treat as new content.
//
// Use SetArenaWithRoot when publishing a new snapshot (i.e. when the
// payload bytes changed). Use SetArena when only the path/size moved
// but the root remains the previous value.
func (c *Controller) SetArena(path string, size uint64) error {
	return c.publish(path, size, nil)
}

// SetArenaWithRoot publishes a new arena and the BLAKE3 root of its
// payload. A reader sees either the previous (path, size, root) or this
// one, never a mixture.
func (c *Controller) SetArenaWithRoot(path string, size uint64, root [32]byte) error {
	return c.publish(path, size, &root)
}

// IsZeroRoot reports whether root is the all-zero sentinel that
// indicates "no snapshot has been published yet."
func IsZeroRoot(root [32]byte) bool {
	var zero [32]byte
	return subtle.ConstantTimeCompare(root[:], zero[:]) == 1
}

// Close unmaps and closes the control file.
func (c *Controller) Close() error {
	if err := unix.Munmap(c.data); err != nil {
		return err
	}
	_ = c.probe.Close()
	return c.file.Close()
}
