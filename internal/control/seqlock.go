package control

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

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
// while it holds the lock shared, knows the publisher is gone. The payload
// may be torn, so it answers with its own last consistent read, whole; with
// none, it answers the path and size as left and the zero root ("nothing
// published"), which every consumer refuses to load data under; the path
// lets a restarted publisher find its arena. A publish that finds the
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

// snapshot reads the payload under the seqlock. A consistent copy is
// remembered as this handle's last; facing a dead publisher, a reader returns
// that last copy whole, or, having none, the payload as left with the zero
// root. The torn root is never returned. This is ley-line-open's reader rule
// (ley-line-open-0a8c14), step for step.
func (c *Controller) snapshot() snapshot {
	seq := c.word(offSync)
	for {
		before := atomic.LoadUint64(seq)
		if before&1 == 1 {
			left, dead := c.abandonedPayload(before)
			if !dead {
				yieldWhileOdd() // a live publish is in flight
				continue
			}
			if last := c.last.Load(); last != nil {
				return *last
			}
			left.root = [currentRootLen]byte{}
			return left
		}
		s := c.copyPayload()
		if atomic.LoadUint64(seq) == before {
			c.last.Store(&s)
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

// abandonedPayload reports whether the odd sequence odd was left by a
// publisher that is gone, and if so the payload as it left it: no one holds
// the publish lock, and the sequence does not move while this reader holds it
// shared, which keeps a new publish from starting, so the copy is taken under
// that shared lock. A held lock, a failed probe or a sequence that moved all
// say "keep waiting".
func (c *Controller) abandonedPayload(odd uint64) (snapshot, bool) {
	fd := int(c.probe.Fd())
	if err := unix.Flock(fd, unix.LOCK_SH|unix.LOCK_NB); err != nil {
		return snapshot{}, false
	}
	defer func() { _ = unix.Flock(fd, unix.LOCK_UN) }()
	if atomic.LoadUint64(c.word(offSync)) != odd {
		return snapshot{}, false
	}
	return c.copyPayload(), true
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
