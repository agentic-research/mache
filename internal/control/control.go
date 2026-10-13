package control

import (
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	ControlSize = 4096       // 1 page
	Magic       = 0x4C455943 // 'LEYC'
	// Version 3 is the current control-block protocol (ley-line-open
	// v0.20.1+, ley-line-open-49a1ef): the layout is v2's, identity is the
	// BLAKE3 `current_root` of the arena payload, and the word at offset 8 is
	// a seqlock sequence, odd while a writer is changing the payload and even
	// when it is whole. v2 wrote the payload and then bumped the word once,
	// so a reader could not tell a publish in flight from a finished one.
	// Any other version is rejected with a descriptive error, so
	// cross-version mismatches fail loudly (mache-3451a1).
	Version = 3
)

// Field offsets must match LLO `rs/ll-core/core/src/control.rs` exactly.
// Bytes 8..16 (the old `Generation` slot) are the private seqlock sequence.
const (
	offMagic          = 0
	offVersion        = 4
	offSync           = 8 // seqlock sequence (formerly Generation)
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

// The payload (path, size, root) is 296 bytes, which nothing writes
// atomically, so a reader overlapping a publish could copy half of one and
// half of the next. The seqlock makes the overlap detectable, exactly as
// ley-line-open's control.rs does:
//
//   - a writer increments the sequence to ODD before touching the payload
//     and to EVEN after the last byte;
//   - a reader loads the sequence, retries while it is odd, copies the
//     payload, reloads the sequence, and keeps the copy only if it did not
//     move. A publish that began or finished during the copy changed it.
//
// A publisher can die mid-publish and leave the sequence odd. So a publish
// also holds an exclusive flock(2) on the control file for as long as the
// sequence is odd, as ley-line-open's does (ley-line-open-0a8c14). A reader
// facing an odd sequence whose lock it can take, and which does not move
// while it holds the lock shared, knows the publisher is gone: the payload
// may be torn, so it answers the zero root ("nothing published"), which
// every consumer refuses to load data under, with the path and size as left,
// so a restarted publisher can find its arena. A publish that finds the
// sequence already odd keeps it odd, and ends even over a whole payload. The
// lock decides, not a clock, so a publisher that takes no lock (leyline
// before the release that added it) reads as dead while odd: every process
// publishing to one control block moves to the lock together.
//
// Every payload word is moved with an 8-byte atomic load or store (the
// fields are 8-byte aligned in a page-aligned mapping). Go has no volatile
// access; atomics give the same "really read it from the mapping" guarantee,
// order every access against the sequence, and keep a reader racing a
// writer in this process free of data races.

// snapshot is one consistent copy of the payload: path, size and root as a
// single publish left them.
type snapshot struct {
	path [arenaPathLen]byte
	size uint64
	root [currentRootLen]byte
}

func (c *Controller) word(off int) *uint64 { return (*uint64)(unsafe.Pointer(&c.data[off])) }

func (c *Controller) loadWords(off int, dst []byte) {
	for i := 0; i < len(dst); i += 8 {
		binary.NativeEndian.PutUint64(dst[i:], atomic.LoadUint64(c.word(off+i)))
	}
}

func (c *Controller) storeWords(off int, src []byte) {
	for i := 0; i < len(src); i += 8 {
		atomic.StoreUint64(c.word(off+i), binary.NativeEndian.Uint64(src[i:]))
	}
}

// yieldWhileOdd is what a reader does on finding a publish in flight. A
// variable so a test can observe the reader waiting, rather than inferring
// it from how long a read took.
var yieldWhileOdd = runtime.Gosched

// snapshot reads the payload under the seqlock.
func (c *Controller) snapshot() snapshot {
	seq := c.word(offSync)
	for {
		before := atomic.LoadUint64(seq)
		if before&1 == 1 {
			if c.publisherIsDead(before) {
				s := c.copyPayload()
				s.root = [currentRootLen]byte{}
				return s
			}
			yieldWhileOdd() // a live publish is in flight
			continue
		}
		s := c.copyPayload()
		if atomic.LoadUint64(seq) == before {
			return s
		}
	}
}

// copyPayload copies the payload as it stands, consistent or not.
func (c *Controller) copyPayload() snapshot {
	var s snapshot
	c.loadWords(offArenaPath, s.path[:])
	s.size = atomic.LoadUint64(c.word(offArenaSize))
	c.loadWords(offCurrentRoot, s.root[:])
	return s
}

// publisherIsDead reports whether the odd sequence odd was left by a publisher
// that is gone: no one holds the publish lock, and the sequence does not move
// while this reader holds it shared, which keeps a new publish from starting.
// A held lock, a failed probe or a sequence that moved all say "keep waiting".
func (c *Controller) publisherIsDead(odd uint64) bool {
	fd := int(c.probe.Fd())
	if err := unix.Flock(fd, unix.LOCK_SH|unix.LOCK_NB); err != nil {
		return false
	}
	unmoved := atomic.LoadUint64(c.word(offSync)) == odd
	_ = unix.Flock(fd, unix.LOCK_UN)
	return unmoved
}

// duringPublish runs while a publish holds the lock with the payload half
// written. A test seam, so a test can look at the control block mid-publish
// instead of racing one.
var duringPublish = func() {}

// publish writes the payload under the seqlock. root nil keeps the
// published root, for a re-advertisement that moves only path and size.
//
// The publish lock serializes writers, across processes and within this one.
func (c *Controller) publish(path string, size uint64, root *[currentRootLen]byte) error {
	if len(path) >= arenaPathLen {
		return fmt.Errorf("path too long (max %d)", arenaPathLen-1)
	}
	var buf [arenaPathLen]byte
	copy(buf[:], path)

	fd := int(c.file.Fd())
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		return fmt.Errorf("take the control block's publish lock: %w", err)
	}
	defer func() { _ = unix.Flock(fd, unix.LOCK_UN) }()

	seq := c.word(offSync)
	beginPublish(seq) // odd: the payload is changing
	c.storeWords(offArenaPath, buf[:])
	duringPublish()
	atomic.StoreUint64(c.word(offArenaSize), size)
	if root != nil {
		c.storeWords(offCurrentRoot, root[:])
	}
	atomic.AddUint64(seq, 1) // even: the payload is whole
	return nil
}

// beginPublish makes the sequence odd. Called with the publish lock held, so
// a sequence that is already odd cannot belong to a live publish: a publisher
// died mid-publish. It stays odd, since turning it even would let readers copy
// this publish half-written, and this publish's last increment makes it even
// over a whole payload.
func beginPublish(seq *uint64) {
	for {
		cur := atomic.LoadUint64(seq)
		if cur&1 == 1 || atomic.CompareAndSwapUint64(seq, cur, cur+1) {
			return
		}
	}
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
