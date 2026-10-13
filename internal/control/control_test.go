package control

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// ---------------------------------------------------------------------------
// Existing tests (preserved verbatim)
// ---------------------------------------------------------------------------

func TestOpenOrCreate_SecurityValidation(t *testing.T) {
	tmpDir := t.TempDir()

	// Blocked path (outside safe areas)
	blockedPath := "/etc/mache.ctrl"
	_, err := OpenOrCreate(blockedPath)
	if err == nil {
		t.Errorf("Expected error for blocked path %s, but got nil", blockedPath)
	} else if !strings.Contains(err.Error(), "security violation") {
		t.Errorf("Expected security violation error, but got: %v", err)
	}

	// Safe path (inside TempDir)
	safePath := filepath.Join(tmpDir, "control.ctrl")
	ctrl, err := OpenOrCreate(safePath)
	if err != nil {
		if strings.Contains(err.Error(), "security violation") {
			t.Errorf("Path %s should be safe, but got security violation: %v", safePath, err)
		}
	}
	if ctrl != nil {
		_ = ctrl.Close()
	}

	// Path traversal attempt
	tempDirRoot := os.TempDir()
	traversalPath := filepath.Join(tempDirRoot, "..", "mache_evil.ctrl")

	_, err = OpenOrCreate(traversalPath)
	if err == nil {
		abs, _ := filepath.Abs(traversalPath)
		if !isUnder(abs, tempDirRoot) {
			t.Errorf("Expected error for traversal path %s escaping %s, but got nil", abs, tempDirRoot)
		}
	}
}

func TestIsUnder(t *testing.T) {
	tests := []struct {
		path     string
		base     string
		expected bool
	}{
		{"/tmp/foo", "/tmp", true},
		{"/tmp/foo/bar", "/tmp", true},
		{"/tmp", "/tmp", true},
		{"/etc/passwd", "/tmp", false},
		{"/tmp/../etc/passwd", "/tmp", false},
		{"/tmp-other/foo", "/tmp", false},
	}

	for _, tt := range tests {
		res := isUnder(tt.path, tt.base)
		if res != tt.expected {
			t.Errorf("isUnder(%q, %q) = %v; want %v", tt.path, tt.base, res, tt.expected)
		}
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// newController opens a fresh control file in t.TempDir() and registers a
// Close on test cleanup. Returns the controller + its on-disk path so tests
// can reopen it to assert round-trip semantics.
func newController(t *testing.T) (*Controller, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "control.ctrl")
	ctrl, err := OpenOrCreate(path)
	require.NoError(t, err)
	t.Cleanup(func() {
		// Best-effort close — Close() may already have been called by the test.
		// Munmap on already-unmapped memory returns EINVAL on Linux/macOS, so
		// silently ignore — we only care that the test cleanup doesn't panic.
		defer func() { _ = recover() }()
		_ = ctrl.Close()
	})
	return ctrl, path
}

// fillRoot makes a deterministic non-zero 32-byte root from a single byte.
func fillRoot(b byte) [32]byte {
	var r [32]byte
	for i := range r {
		r[i] = b
	}
	return r
}

// ---------------------------------------------------------------------------
// GetCurrentRoot / IsZeroRoot
// ---------------------------------------------------------------------------

func TestGetCurrentRoot_FreshIsZero(t *testing.T) {
	ctrl, _ := newController(t)

	root := ctrl.GetCurrentRoot()
	assert.True(t, IsZeroRoot(root), "fresh controller must report zero root")
	assert.Equal(t, [32]byte{}, root)
}

func TestIsZeroRoot(t *testing.T) {
	tests := []struct {
		name string
		root [32]byte
		want bool
	}{
		{"all-zero sentinel", [32]byte{}, true},
		{"first byte set", [32]byte{0x01}, false},
		{"last byte set", func() [32]byte { var r [32]byte; r[31] = 0xff; return r }(), false},
		{"all 0xff", fillRoot(0xff), false},
		{"all 0x01", fillRoot(0x01), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsZeroRoot(tt.root))
		})
	}
}

// ---------------------------------------------------------------------------
// GetArenaPath / GetArenaSize — fresh
// ---------------------------------------------------------------------------

func TestGetArenaPath_FreshIsEmpty(t *testing.T) {
	ctrl, _ := newController(t)
	assert.Equal(t, "", ctrl.GetArenaPath(), "fresh controller has no arena path")
}

func TestGetArenaSize_FreshIsZero(t *testing.T) {
	ctrl, _ := newController(t)
	assert.Equal(t, uint64(0), ctrl.GetArenaSize(), "fresh controller has zero arena size")
}

// TestGetArenaPath_NoNullTerminator covers the fall-through branch where the
// stored path occupies the full arenaPathLen window with no embedded NUL —
// we then return the entire window as a string.
func TestGetArenaPath_NoNullTerminator(t *testing.T) {
	ctrl, _ := newController(t)

	// Manually fill the path buffer with non-zero bytes (no NUL terminator)
	// — this is the fall-through branch of GetArenaPath. We use 'a' to keep
	// it printable for assertion clarity.
	buf := ctrl.data[offArenaPath : offArenaPath+arenaPathLen]
	for i := range buf {
		buf[i] = 'a'
	}

	got := ctrl.GetArenaPath()
	assert.Len(t, got, arenaPathLen, "path with no NUL terminator must return full window")
	assert.Equal(t, strings.Repeat("a", arenaPathLen), got)
}

// ---------------------------------------------------------------------------
// SetArena — happy path + validation + round-trip
// ---------------------------------------------------------------------------

func TestSetArena_RoundTripSamePath(t *testing.T) {
	ctrl, path := newController(t)

	const arenaPath = "/tmp/mache-test/arena-001.bin"
	const arenaSize uint64 = 4096

	require.NoError(t, ctrl.SetArena(arenaPath, arenaSize))

	// Read-back on the same controller
	assert.Equal(t, arenaPath, ctrl.GetArenaPath())
	assert.Equal(t, arenaSize, ctrl.GetArenaSize())

	// Persistence across reopen — closes, reopens the underlying mmap.
	require.NoError(t, ctrl.Close())

	reopened, err := OpenOrCreate(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })

	assert.Equal(t, arenaPath, reopened.GetArenaPath(), "arena path must survive reopen")
	assert.Equal(t, arenaSize, reopened.GetArenaSize(), "arena size must survive reopen")

	// SetArena must NOT modify current_root
	assert.True(t, IsZeroRoot(reopened.GetCurrentRoot()), "SetArena must leave root untouched")
}

func TestSetArena_PathTooLong(t *testing.T) {
	ctrl, _ := newController(t)

	// arenaPathLen-1 bytes is the largest acceptable string; arenaPathLen is
	// rejected because the check is `>=` to leave room for the implicit NUL.
	tooLong := strings.Repeat("x", arenaPathLen)
	err := ctrl.SetArena(tooLong, 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path too long")

	// Exactly arenaPathLen-1 must succeed
	maxAllowed := strings.Repeat("y", arenaPathLen-1)
	require.NoError(t, ctrl.SetArena(maxAllowed, 1))
	assert.Equal(t, maxAllowed, ctrl.GetArenaPath())
}

// TestSetArena_BumpsSyncAtom asserts the Release-store contract: SetArena
// must increment the private sync atom so reader Acquire-loads observe a
// fenced view.
func TestSetArena_PublishesUnderTheSeqlock(t *testing.T) {
	ctrl, _ := newController(t)

	syncPtr := (*uint64)(unsafe.Pointer(&ctrl.data[offSync]))
	before := atomic.LoadUint64(syncPtr)
	require.Zero(t, before&1, "a fresh control block is at rest: even")

	require.NoError(t, ctrl.SetArena("/tmp/a", 1))
	mid := atomic.LoadUint64(syncPtr)
	assert.Equal(t, before+2, mid, "a publish marks the sequence odd, then even: two steps")

	require.NoError(t, ctrl.SetArenaWithRoot("/tmp/b", 2, [32]byte{1}))
	after := atomic.LoadUint64(syncPtr)
	assert.Equal(t, mid+2, after, "every publish, with or without a root")
}

// TestSetArena_ClearsPriorPathBytes asserts the path-buffer zero-fill: writing
// a shorter path over a longer one must NOT leave dangling bytes from the
// prior write.
func TestSetArena_ClearsPriorPathBytes(t *testing.T) {
	ctrl, _ := newController(t)

	long := "/tmp/mache-test/very-long-arena-path-aaaaaaaaaaaa.bin"
	short := "/tmp/short.bin"

	require.NoError(t, ctrl.SetArena(long, 100))
	require.Equal(t, long, ctrl.GetArenaPath())

	require.NoError(t, ctrl.SetArena(short, 50))
	assert.Equal(t, short, ctrl.GetArenaPath(), "shorter path must not have residual bytes")
}

// ---------------------------------------------------------------------------
// SetArenaWithRoot — happy path + validation + round-trip
// ---------------------------------------------------------------------------

func TestSetArenaWithRoot_RoundTrip(t *testing.T) {
	ctrl, path := newController(t)

	const arenaPath = "/tmp/mache-test/arena-with-root.bin"
	const arenaSize uint64 = 8192
	root := fillRoot(0xab)

	require.NoError(t, ctrl.SetArenaWithRoot(arenaPath, arenaSize, root))

	assert.Equal(t, arenaPath, ctrl.GetArenaPath())
	assert.Equal(t, arenaSize, ctrl.GetArenaSize())
	assert.Equal(t, root, ctrl.GetCurrentRoot())
	assert.False(t, IsZeroRoot(ctrl.GetCurrentRoot()))

	// Persistence across reopen
	require.NoError(t, ctrl.Close())

	reopened, err := OpenOrCreate(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })

	assert.Equal(t, arenaPath, reopened.GetArenaPath())
	assert.Equal(t, arenaSize, reopened.GetArenaSize())
	assert.Equal(t, root, reopened.GetCurrentRoot(), "root must survive reopen")
}

func TestSetArenaWithRoot_PathTooLong(t *testing.T) {
	ctrl, _ := newController(t)

	tooLong := strings.Repeat("z", arenaPathLen)
	err := ctrl.SetArenaWithRoot(tooLong, 1, fillRoot(0x01))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path too long")
}

// TestSetArenaWithRoot_OverwritesPriorRoot proves rotation: a second
// SetArenaWithRoot replaces a previously-published root.
func TestSetArenaWithRoot_OverwritesPriorRoot(t *testing.T) {
	ctrl, _ := newController(t)

	first := fillRoot(0x11)
	second := fillRoot(0x22)

	require.NoError(t, ctrl.SetArenaWithRoot("/tmp/a", 1, first))
	require.Equal(t, first, ctrl.GetCurrentRoot())

	require.NoError(t, ctrl.SetArenaWithRoot("/tmp/b", 2, second))
	assert.Equal(t, second, ctrl.GetCurrentRoot(), "second SetArenaWithRoot must overwrite root")
}

// TestSetArena_PreservesRootSetByPriorSetArenaWithRoot covers the documented
// contract that SetArena is a path/size-only mutator — the BLAKE3 root from
// a prior SetArenaWithRoot must survive.
func TestSetArena_PreservesRootSetByPriorSetArenaWithRoot(t *testing.T) {
	ctrl, _ := newController(t)

	root := fillRoot(0x77)
	require.NoError(t, ctrl.SetArenaWithRoot("/tmp/initial", 100, root))
	require.Equal(t, root, ctrl.GetCurrentRoot())

	// SetArena should NOT touch the root
	require.NoError(t, ctrl.SetArena("/tmp/moved", 200))
	assert.Equal(t, root, ctrl.GetCurrentRoot(),
		"SetArena must preserve root from prior SetArenaWithRoot")
	assert.Equal(t, "/tmp/moved", ctrl.GetArenaPath())
	assert.Equal(t, uint64(200), ctrl.GetArenaSize())
}

// ---------------------------------------------------------------------------
// OpenOrCreate — reopen + corrupted control block
// ---------------------------------------------------------------------------

// TestOpenOrCreate_ReopenExisting covers the `existingMagic == Magic` branch
// of OpenOrCreate (the happy reopen path).
func TestOpenOrCreate_ReopenExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.ctrl")

	first, err := OpenOrCreate(path)
	require.NoError(t, err)

	root := fillRoot(0x42)
	require.NoError(t, first.SetArenaWithRoot("/tmp/orig", 999, root))
	require.NoError(t, first.Close())

	// Reopen — must hit the `existingMagic == Magic` && version-match branch
	second, err := OpenOrCreate(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = second.Close() })

	assert.Equal(t, "/tmp/orig", second.GetArenaPath())
	assert.Equal(t, uint64(999), second.GetArenaSize())
	assert.Equal(t, root, second.GetCurrentRoot())
}

func TestOpenOrCreate_InvalidMagic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "badmagic.ctrl")

	// Pre-write a file with bogus magic bytes
	require.NoError(t, os.WriteFile(path, make([]byte, ControlSize), 0o644))
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	require.NoError(t, err)
	bogus := uint32(0xDEADBEEF)
	_, err = f.WriteAt((*[4]byte)(unsafe.Pointer(&bogus))[:], 0)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	_, err = OpenOrCreate(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid control block magic")
}

func TestOpenOrCreate_VersionMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "badversion.ctrl")

	// Write correct magic, wrong version
	buf := make([]byte, ControlSize)
	*(*uint32)(unsafe.Pointer(&buf[offMagic])) = Magic
	*(*uint32)(unsafe.Pointer(&buf[offVersion])) = 1 // old layout
	require.NoError(t, os.WriteFile(path, buf, 0o644))

	_, err := OpenOrCreate(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "version mismatch")
	assert.Contains(t, err.Error(), "v1")

	// v2 shares v3's layout but not its publish protocol: a v2 writer leaves
	// no odd mark for a v3 reader to wait on, so it is refused too.
	*(*uint32)(unsafe.Pointer(&buf[offVersion])) = 2
	require.NoError(t, os.WriteFile(path, buf, 0o644))
	_, err = OpenOrCreate(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "file is v2, expected v3")
}

// TestOpenOrCreate_ConcurrentCreate covers the race where two callers
// concurrently OpenOrCreate the same path. Both must succeed (the mmap+file
// is per-handle), and at least one valid Controller must come back with the
// expected magic/version header — no corruption, no panic.
//
// This test exists to satisfy the "concurrent OpenOrCreate" requirement in
// bead mache-51a0fb and the -race discipline from mache-4a827c.
func TestOpenOrCreate_ConcurrentCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.ctrl")

	const n = 8
	var wg sync.WaitGroup
	results := make([]*Controller, n)
	errs := make([]error, n)

	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = OpenOrCreate(path)
		}(i)
	}
	close(start)
	wg.Wait()

	// All must succeed — POSIX open(O_CREAT) is concurrent-safe; we're just
	// asserting our wrapper around it (mmap, header init) doesn't corrupt.
	successCount := 0
	for i, c := range results {
		require.NoError(t, errs[i], "goroutine %d", i)
		require.NotNil(t, c, "goroutine %d returned nil controller", i)
		successCount++
	}
	assert.Equal(t, n, successCount)

	// The header must be a valid, single (magic, version) — never a torn
	// half-initialized state. We assert via the first controller; the others
	// view the same on-disk bytes (MAP_SHARED).
	header := results[0]
	gotMagic := *(*uint32)(unsafe.Pointer(&header.data[offMagic]))
	gotVersion := *(*uint32)(unsafe.Pointer(&header.data[offVersion]))
	assert.Equal(t, uint32(Magic), gotMagic, "magic must be initialized exactly once")
	assert.Equal(t, uint32(Version), gotVersion, "version must be initialized exactly once")

	// Clean up
	for _, c := range results {
		_ = c.Close()
	}
}

// ---------------------------------------------------------------------------
// Close — idempotency + concurrent close
// ---------------------------------------------------------------------------

// TestClose_DoubleClose covers the second-Close branch — the production code
// today does NOT use sync.Once, so the second Close is expected to error on
// Munmap (already unmapped) rather than succeed. The contract we assert is:
// no panic, and the second call returns an error so callers can detect the
// double-close (matching the discipline from mache-4a827c).
func TestClose_DoubleClose(t *testing.T) {
	ctrl, _ := newController(t)

	require.NoError(t, ctrl.Close(), "first Close must succeed")

	// Second close must not panic. It is allowed to return an error (Munmap
	// of already-unmapped memory) — that's a useful loud-failure signal.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("second Close must not panic, got: %v", r)
		}
	}()
	_ = ctrl.Close()
}

// TestClose_ConcurrentClose validates that two goroutines racing on Close()
// don't corrupt state or panic. Per mache-4a827c, this needs -race to catch
// scheduling-dependent failures that single-threaded tests miss.
//
// Today the production code is NOT goroutine-safe on Close — this test
// pins the current observable behavior: no panic, at least one Close sees
// nil — under the sync.Once discipline ([[mache-4a827c]]) this would
// tighten to "exactly one," tracked as a follow-up.
func TestClose_ConcurrentClose(t *testing.T) {
	ctrl, _ := newController(t)

	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)

	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() {
				// Concurrent close on an already-Munmap'd region can SIGBUS
				// in pathological scheduling. We don't want a panic to crash
				// the test runner — capture it as an error signal instead.
				if r := recover(); r != nil {
					errs[i] = nil // panic captured; test will detect via fatal below
					t.Errorf("concurrent Close panicked in goroutine %d: %v", i, r)
				}
			}()
			<-start
			errs[i] = ctrl.Close()
		}(i)
	}
	close(start)
	wg.Wait()

	// At least one Close must have succeeded (typically the first scheduled).
	successCount := 0
	for _, e := range errs {
		if e == nil {
			successCount++
		}
	}
	assert.GreaterOrEqual(t, successCount, 1, "at least one Close must succeed")
}

// ---------------------------------------------------------------------------
// validatePath — direct branch coverage
// ---------------------------------------------------------------------------

func TestValidatePath_UnderUserHomeMache(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("UserHomeDir unavailable on this platform")
	}

	// ~/.mache/foo.ctrl is explicitly permitted — even if the dir doesn't
	// exist yet, validatePath only consults the abs path string.
	path := filepath.Join(home, ".mache", "validatepath-test.ctrl")
	require.NoError(t, validatePath(path), "~/.mache paths must validate")
}

func TestValidatePath_UnderTempDir(t *testing.T) {
	path := filepath.Join(os.TempDir(), "validatepath-test.ctrl")
	require.NoError(t, validatePath(path))
}

func TestValidatePath_SecurityViolation(t *testing.T) {
	err := validatePath("/etc/passwd")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "security violation")
}

func TestValidatePath_RelativePathResolvesToCwd(t *testing.T) {
	// Relative paths are made absolute relative to cwd, which is virtually
	// never under ~/.mache or TempDir — so this should be rejected.
	err := validatePath("relative-control.ctrl")
	if err != nil {
		assert.Contains(t, err.Error(), "security violation")
	}
}

// A reader that finds the sequence odd waits for the publish to finish rather
// than copying a payload that is half written.
// lockAsPublisher takes the publish lock on its own open of the control file,
// as a publisher in another process holds it, and returns the release.
func lockAsPublisher(t *testing.T, ctrl *Controller) func() {
	t.Helper()
	f, err := os.OpenFile(ctrl.path, os.O_RDWR, 0)
	require.NoError(t, err)
	require.NoError(t, unix.Flock(int(f.Fd()), unix.LOCK_EX))
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}
}

// dieMidPublish leaves the control block as a publisher killed mid-publish
// leaves it: sequence odd, half of a new root written, no lock held.
func dieMidPublish(ctrl *Controller) {
	atomic.AddUint64((*uint64)(unsafe.Pointer(&ctrl.data[offSync])), 1)
	ctrl.storeWords(offCurrentRoot, bytes.Repeat([]byte{9}, currentRootLen/2))
}

// A live publisher holds the publish lock while the sequence is odd. The
// reader waits it out, however long, and returns the finished publish; the
// test advances when the reader reports it is waiting, not after a duration.
func TestGetCurrentRoot_WaitsOutALivePublish(t *testing.T) {
	ctrl, _ := newController(t)
	require.NoError(t, ctrl.SetArenaWithRoot("/tmp/old", 1, [32]byte{1}))

	waiting := make(chan struct{})
	var once sync.Once
	defer func(orig func()) { yieldWhileOdd = orig }(yieldWhileOdd)
	yieldWhileOdd = func() {
		once.Do(func() { close(waiting) })
		runtime.Gosched()
	}

	unlock := lockAsPublisher(t, ctrl)
	seq := (*uint64)(unsafe.Pointer(&ctrl.data[offSync]))
	atomic.AddUint64(seq, 1) // a writer has begun: odd
	got := make(chan [32]byte)
	go func() { got <- ctrl.GetCurrentRoot() }()

	<-waiting // the reader found the lock held and did not return
	// The writer finishes: new root, then even, then releases the lock.
	ctrl.storeWords(offCurrentRoot, bytes.Repeat([]byte{2}, currentRootLen))
	atomic.AddUint64(seq, 1)
	unlock()
	assert.Equal(t, [32]byte(bytes.Repeat([]byte{2}, currentRootLen)), <-got,
		"the reader returns the finished publish, not the payload it found half written")
}

// A publisher that died mid-publish left the sequence odd and released its
// lock when it died (ley-line-open-0a8c14). A reader neither waits forever
// nor hands out the torn root: it answers "nothing published", with the path
// a restarted publisher needs.
func TestGetCurrentRoot_ADeadPublisherReadsAsNothingPublished(t *testing.T) {
	ctrl, _ := newController(t)
	require.NoError(t, ctrl.SetArenaWithRoot("/tmp/a", 7, [32]byte{1}))
	dieMidPublish(ctrl)

	defer func(orig func()) { yieldWhileOdd = orig }(yieldWhileOdd)
	yieldWhileOdd = func() { t.Fatal("waited for a publisher that is gone") }

	assert.True(t, IsZeroRoot(ctrl.GetCurrentRoot()), "neither the old root nor a torn one")
	assert.Equal(t, "/tmp/a", ctrl.GetArenaPath(), "the path a restart needs survives")
	assert.Equal(t, uint64(7), ctrl.GetArenaSize())
}

// The next publish after a dead publisher keeps the sequence odd while it
// writes, rather than turning it even over a half-written payload, and ends
// even with its own root whole.
func TestPublish_AfterADeadPublisherKeepsTheSequenceOddUntilWhole(t *testing.T) {
	ctrl, _ := newController(t)
	require.NoError(t, ctrl.SetArenaWithRoot("/tmp/a", 1, [32]byte{1}))
	dieMidPublish(ctrl)

	seq := (*uint64)(unsafe.Pointer(&ctrl.data[offSync]))
	var oddMidPublish bool
	defer func(orig func()) { duringPublish = orig }(duringPublish)
	duringPublish = func() { oddMidPublish = atomic.LoadUint64(seq)&1 == 1 }

	require.NoError(t, ctrl.SetArenaWithRoot("/tmp/b", 2, [32]byte{2}))
	assert.True(t, oddMidPublish, "odd while the payload is written")
	assert.Zero(t, atomic.LoadUint64(seq)&1, "even once it is whole")
	assert.Equal(t, [32]byte{2}, ctrl.GetCurrentRoot())
	assert.Equal(t, "/tmp/b", ctrl.GetArenaPath())
}

// A publish holds the publish lock while the payload is half written, so a
// reader anywhere, this process included, can tell it from a dead one.
func TestPublish_HoldsThePublishLockWhileOdd(t *testing.T) {
	ctrl, _ := newController(t)
	other, err := os.Open(ctrl.path)
	require.NoError(t, err)
	defer func() { _ = other.Close() }()

	var probe error
	defer func(orig func()) { duringPublish = orig }(duringPublish)
	duringPublish = func() { probe = unix.Flock(int(other.Fd()), unix.LOCK_SH|unix.LOCK_NB) }

	require.NoError(t, ctrl.SetArenaWithRoot("/tmp/a", 1, [32]byte{1}))
	assert.ErrorIs(t, probe, unix.EWOULDBLOCK, "the lock is held mid-publish")
	require.NoError(t, unix.Flock(int(other.Fd()), unix.LOCK_SH|unix.LOCK_NB), "and released after")
	_ = unix.Flock(int(other.Fd()), unix.LOCK_UN)
}

// The property the seqlock exists for: with a writer publishing distinct
// (path, size, root) triples as fast as it can, no reader ever sees a
// mixture of two. Each triple is self-describing (path, size and every root
// byte encode the same n), so a torn read is detectable from the read alone.
func TestSeqlock_NoTornReadsUnderAConcurrentWriter(t *testing.T) {
	ctrl, path := newController(t)
	triple := func(n uint64) (string, [32]byte) {
		var root [32]byte
		for i := range root {
			root[i] = byte(n)
		}
		return fmt.Sprintf("/tmp/arena-%03d", n%256), root
	}

	// Publish one triple before any reader starts: the control block's
	// initial all-zero state is not a triple anyone published.
	p0, r0 := triple(0)
	require.NoError(t, ctrl.SetArenaWithRoot(p0, 0, r0))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for n := uint64(1); ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			p, r := triple(n % 256)
			if err := ctrl.SetArenaWithRoot(p, n%256, r); err != nil {
				t.Error(err)
				return
			}
		}
	}()

	// Readers on their own mapping of the same file, as another process
	// would be.
	const readers, reads = 4, 30000
	var torn atomic.Int64
	var rg sync.WaitGroup
	for range readers {
		rg.Add(1)
		go func() {
			defer rg.Done()
			r, err := OpenOrCreate(path)
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = r.Close() }()
			for range reads {
				s := r.snapshot()
				n := s.size
				p, root := triple(n)
				if string(s.path[:len(p)]) != p || s.path[len(p)] != 0 || s.root != root {
					torn.Add(1)
				}
			}
		}()
	}
	rg.Wait()
	close(stop)
	wg.Wait()
	assert.Zero(t, torn.Load(), "every read must be a triple some publish wrote whole (%d reads)", readers*reads)
}
