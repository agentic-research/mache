package control

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	ControlSize = 4096       // 1 page
	Magic       = 0x4C455943 // 'LEYC'
)

// Block represents the memory-mapped control file.
// It must match the C layout exactly for interoperability.
type Block struct {
	Magic      uint32
	Version    uint32
	Generation uint64 // Atomic
	ArenaPath  [256]byte
	ArenaSize  uint64
	Padding    [ControlSize - 272]byte // Pad to 4096 bytes
}

// Controller manages the memory-mapped control file.
type Controller struct {
	path string
	file *os.File
	data []byte
	ptr  *Block
}

// OpenOrCreate opens or creates a control file at the given path.
func OpenOrCreate(path string) (*Controller, error) {
	// Ensure directory exists
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

	ptr := (*Block)(unsafe.Pointer(&data[0]))

	// Initialize if new
	if ptr.Magic == 0 {
		ptr.Magic = Magic
		ptr.Version = 1
	} else if ptr.Magic != Magic {
		_ = unix.Munmap(data)
		_ = f.Close()
		return nil, fmt.Errorf("invalid magic: %x", ptr.Magic)
	}

	return &Controller{
		path: path,
		file: f,
		data: data,
		ptr:  ptr,
	}, nil
}

// GetGeneration returns the current generation ID atomically.
func (c *Controller) GetGeneration() uint64 {
	return atomic.LoadUint64(&c.ptr.Generation)
}

// GetArenaPath returns the path to the currently active arena.
func (c *Controller) GetArenaPath() string {
	// Simple null-terminated string read
	b := c.ptr.ArenaPath[:]
	for i, v := range b {
		if v == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// SetArena atomically updates the control block to point to a new arena.
func (c *Controller) SetArena(path string, size, generation uint64) error {
	if len(path) >= len(c.ptr.ArenaPath) {
		return fmt.Errorf("path too long (max %d)", len(c.ptr.ArenaPath)-1)
	}

	// Update fields
	copy(c.ptr.ArenaPath[:], path)
	c.ptr.ArenaPath[len(path)] = 0 // Null terminate
	c.ptr.ArenaSize = size

	// Memory barrier before generation update (Go atomic handles this)
	atomic.StoreUint64(&c.ptr.Generation, generation)

	// msync to ensure durability (optional but good for crash consistency)
	// unix.Msync(c.data, unix.MS_SYNC)

	return nil
}

// Close unmaps and closes the control file.
func (c *Controller) Close() error {
	if err := unix.Munmap(c.data); err != nil {
		return err
	}
	return c.file.Close()
}
