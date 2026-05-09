package control

import (
	"crypto/subtle"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	ControlSize = 4096       // 1 page
	Magic       = 0x4C455943 // 'LEYC'
	// Version 2 is the current control-block layout: identity is the BLAKE3
	// `current_root` of the arena payload. Old (v1) controllers — which
	// exposed a monotonic `generation` counter instead — are rejected with
	// a descriptive error so cross-version mismatches fail loudly.
	Version = 2
)

// Field offsets must match LLO `rs/ll-core/core/src/control.rs` exactly.
// Bytes 8..16 (the old `Generation` slot) are now used as a private
// sync atom for Acquire/Release fencing — no public surface.
const (
	offMagic          = 0
	offVersion        = 4
	offSync           = 8 // private fence atom (formerly Generation)
	offArenaPath      = 16
	arenaPathLen      = 256
	offArenaSize      = 272
	offInterruptFlags = 280
	offInterruptEpoch = 288
	offInterruptAck   = 296
	offPayloadOffset  = 304
	offPayloadLen     = 312
	offCurrentRoot    = 320
	currentRootLen    = 32
)

// Controller manages the memory-mapped control file.
type Controller struct {
	path string
	file *os.File
	data []byte
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
					"remove the stale .ctrl file and let a current writer (ley-line-open ≥ 0.2.0) recreate it",
				existingVersion, Version,
			)
		}
	}

	return &Controller{path: path, file: f, data: data}, nil
}

// GetCurrentRoot returns the BLAKE3 root hash of the arena payload that
// the writer most recently published. An Acquire-load on the private
// sync atom fences the subsequent root-byte reads against the writer's
// Release-store + write. The zero sentinel `[32]byte{}` means no
// snapshot has been published yet (fresh controller).
func (c *Controller) GetCurrentRoot() [32]byte {
	atomic.LoadUint64((*uint64)(unsafe.Pointer(&c.data[offSync])))
	var out [32]byte
	copy(out[:], c.data[offCurrentRoot:offCurrentRoot+currentRootLen])
	return out
}

// GetArenaPath returns the path to the currently active arena.
//
// The Acquire-load on the sync atom only fences the subsequent path-byte
// reads against publishes that have completed (sync bumped) by the time
// we load it; a writer that begins after the load and writes bytes before
// bumping sync can still produce a torn read here. End-to-end safety
// against torn reads relies on the BLAKE3 current_root check at the
// callsite — the path bytes alone are advisory. If a stronger guarantee
// is needed, the writer needs a seqlock-style protocol (bump-then-write-
// then-bump, with reader retry on odd / mismatched seq).
func (c *Controller) GetArenaPath() string {
	atomic.LoadUint64((*uint64)(unsafe.Pointer(&c.data[offSync])))
	b := c.data[offArenaPath : offArenaPath+arenaPathLen]
	for i, v := range b {
		if v == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// GetArenaSize returns the size in bytes of the currently active arena file.
func (c *Controller) GetArenaSize() uint64 {
	return atomic.LoadUint64((*uint64)(unsafe.Pointer(&c.data[offArenaSize])))
}

// SetArena atomically updates the control block to point to a new arena
// without changing `current_root`. Bumps the private sync atom so
// readers fence and re-read path/size.
//
// Use SetArenaWithRoot when publishing a new snapshot (i.e. when the
// payload bytes changed). Use SetArena when only the path/size moved
// but the root remains the previous value.
func (c *Controller) SetArena(path string, size uint64) error {
	if len(path) >= arenaPathLen {
		return fmt.Errorf("path too long (max %d)", arenaPathLen-1)
	}

	pathBuf := c.data[offArenaPath : offArenaPath+arenaPathLen]
	for i := range pathBuf {
		pathBuf[i] = 0
	}
	copy(pathBuf, path)

	atomic.StoreUint64((*uint64)(unsafe.Pointer(&c.data[offArenaSize])), size)

	atomic.AddUint64((*uint64)(unsafe.Pointer(&c.data[offSync])), 1)
	return nil
}

// SetArenaWithRoot atomically publishes a new arena and the BLAKE3 root
// of its payload. Readers polling GetCurrentRoot observe a coherent
// (path, size, root) triple thanks to the Release-store fence on the
// sync atom.
func (c *Controller) SetArenaWithRoot(path string, size uint64, root [32]byte) error {
	if len(path) >= arenaPathLen {
		return fmt.Errorf("path too long (max %d)", arenaPathLen-1)
	}

	pathBuf := c.data[offArenaPath : offArenaPath+arenaPathLen]
	for i := range pathBuf {
		pathBuf[i] = 0
	}
	copy(pathBuf, path)

	atomic.StoreUint64((*uint64)(unsafe.Pointer(&c.data[offArenaSize])), size)

	copy(c.data[offCurrentRoot:offCurrentRoot+currentRootLen], root[:])

	atomic.AddUint64((*uint64)(unsafe.Pointer(&c.data[offSync])), 1)
	return nil
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
	return c.file.Close()
}

func validatePath(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("invalid path: %w", err)
	}

	if home, err := os.UserHomeDir(); err == nil {
		safeHome := filepath.Join(home, ".mache")
		if isUnder(abs, safeHome) {
			return nil
		}
	}

	if isUnder(abs, os.TempDir()) {
		return nil
	}

	return fmt.Errorf("security violation: control file path %q must be under ~/.mache or %s", abs, os.TempDir())
}

func isUnder(path, base string) bool {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}
