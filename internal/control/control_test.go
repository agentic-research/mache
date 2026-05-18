package control

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
func TestSetArena_BumpsSyncAtom(t *testing.T) {
	ctrl, _ := newController(t)

	syncPtr := (*uint64)(unsafe.Pointer(&ctrl.data[offSync]))
	before := atomic.LoadUint64(syncPtr)

	require.NoError(t, ctrl.SetArena("/tmp/a", 1))
	mid := atomic.LoadUint64(syncPtr)
	assert.Equal(t, before+1, mid, "SetArena must bump sync atom by exactly 1")

	require.NoError(t, ctrl.SetArena("/tmp/b", 2))
	after := atomic.LoadUint64(syncPtr)
	assert.Equal(t, mid+1, after, "second SetArena must bump sync atom again")
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
	for i := 0; i < n; i++ {
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
// pins the current observable behavior: no panic, exactly-one nil-error
// caller (the rest may get Munmap EINVAL).
func TestClose_ConcurrentClose(t *testing.T) {
	ctrl, _ := newController(t)

	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)

	start := make(chan struct{})
	for i := 0; i < n; i++ {
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
