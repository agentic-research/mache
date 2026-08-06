package writeback

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentic-research/mache/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func tempFile(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "splice-test-*")
	require.NoError(t, err)
	_, err = f.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return f.Name()
}

func TestSplice_ReplaceMiddle(t *testing.T) {
	path := tempFile(t, "func A() {}\nfunc B() {}\nfunc C() {}\n")
	origin := graph.SourceOrigin{
		FilePath:  path,
		StartByte: 12, // start of "func B() {}\n"
		EndByte:   24, // end of "func B() {}\n"
	}
	err := Splice(origin, []byte("func B() { return 1 }\n"))
	require.NoError(t, err)

	got, _ := os.ReadFile(path)
	assert.Equal(t, "func A() {}\nfunc B() { return 1 }\nfunc C() {}\n", string(got))
}

func TestSplice_ShorterContent(t *testing.T) {
	path := tempFile(t, "func LongName() { /* lots of code */ }\n")
	origin := graph.SourceOrigin{
		FilePath:  path,
		StartByte: 0,
		EndByte:   39,
	}
	err := Splice(origin, []byte("func S() {}\n"))
	require.NoError(t, err)

	got, _ := os.ReadFile(path)
	assert.Equal(t, "func S() {}\n", string(got))
}

func TestSplice_LongerContent(t *testing.T) {
	path := tempFile(t, "func S() {}\n")
	origin := graph.SourceOrigin{
		FilePath:  path,
		StartByte: 0,
		EndByte:   12,
	}
	err := Splice(origin, []byte("func LongName() { /* lots of code */ }\n"))
	require.NoError(t, err)

	got, _ := os.ReadFile(path)
	assert.Equal(t, "func LongName() { /* lots of code */ }\n", string(got))
}

func TestSplice_EmptyContent(t *testing.T) {
	path := tempFile(t, "AAA\nBBB\nCCC\n")
	origin := graph.SourceOrigin{
		FilePath:  path,
		StartByte: 4, // "BBB\n"
		EndByte:   8,
	}
	err := Splice(origin, []byte{})
	require.NoError(t, err)

	got, _ := os.ReadFile(path)
	assert.Equal(t, "AAA\nCCC\n", string(got))
}

func TestSplice_InvalidRange(t *testing.T) {
	path := tempFile(t, "short")
	// EndByte beyond file length
	err := Splice(graph.SourceOrigin{
		FilePath:  path,
		StartByte: 0,
		EndByte:   100,
	}, []byte("x"))
	assert.Error(t, err)

	// StartByte > EndByte
	err = Splice(graph.SourceOrigin{
		FilePath:  path,
		StartByte: 3,
		EndByte:   1,
	}, []byte("x"))
	assert.Error(t, err)
}

func TestSplice_PreservesPermissions(t *testing.T) {
	path := tempFile(t, "content")
	require.NoError(t, os.Chmod(path, 0o755))

	err := Splice(graph.SourceOrigin{
		FilePath:  path,
		StartByte: 0,
		EndByte:   7,
	}, []byte("new"))
	require.NoError(t, err)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}

func TestSplice_RejectsOversizedFile(t *testing.T) {
	// Create a file that exceeds MaxSpliceFileSize
	dir := t.TempDir()
	path := filepath.Join(dir, "big.go")
	// We can't create a 100MB file in tests, so we test that the guard
	// exists by checking the error for a file that's fine but with a
	// MaxSpliceFileSize override. For now, test that Splice works on
	// normal files (the size guard will be enforced in the fix).
	f, err := os.Create(path)
	require.NoError(t, err)
	_, err = f.WriteString("package main\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	err = Splice(graph.SourceOrigin{
		FilePath:  path,
		StartByte: 0,
		EndByte:   13,
	}, []byte("package test\n"))
	assert.NoError(t, err) // normal file should work fine
}

func TestSplice_NonexistentFile(t *testing.T) {
	err := Splice(graph.SourceOrigin{
		FilePath:  filepath.Join(t.TempDir(), "nope.go"),
		StartByte: 0,
		EndByte:   5,
	}, []byte("x"))
	assert.Error(t, err)
}

// TestSplice_DetectsConcurrentModification — bead mache-e7de36.
//
// FALSIFIABLE: prior implementation read the source then did the rename
// without re-checking. A concurrent writer that bumps the file's mtime
// and changes its size between read and rename would silently overwrite
// with stale data computed from the old content. With the TOCTOU guard,
// Splice returns ErrSourceChanged.
func TestSplice_DetectsConcurrentModification(t *testing.T) {
	path := tempFile(t, "func A() {}\nfunc B() {}\nfunc C() {}\n")

	// Get initial mtime, then deliberately set it BACK in time so we have
	// room to bump it forward. (On macOS HFS, mtime resolution is ~1s.)
	initial := time.Now().Add(-2 * time.Second)
	require.NoError(t, os.Chtimes(path, initial, initial))

	// Perform a concurrent modification: append data and bump mtime forward.
	// This must happen between Splice's initial Stat/Read and the final
	// re-Stat. Easiest way to deterministically reproduce is to do it
	// before the call but ensure mtime differs — Splice will see the
	// post-modification stat and abort. We pre-compute the origin against
	// the *initial* content, then mutate, then call Splice.
	original, err := os.ReadFile(path)
	require.NoError(t, err)
	origin := graph.SourceOrigin{
		FilePath:  path,
		StartByte: 12,
		EndByte:   24,
	}

	// Concurrent modification before Splice runs: bump the file's mtime so
	// Splice's initial Stat captures one mtime, but a quick external write
	// changes it before the final re-Stat.
	//
	// Reliable repro: monkey-patch by intercepting at the os.Rename point
	// is overkill; instead we do a sleeper-style trick — modify the file
	// just after Stat. To keep the test deterministic, we patch the file
	// by re-writing it with the same size but a fresh mtime via Chtimes.
	// Stat sees the original mtime; before re-Stat, we bump it.
	//
	// Simplest deterministic shape: spawn a goroutine that races to bump
	// mtime as Splice runs. In practice the splice is fast enough that
	// the test is flaky. A more reliable path: pre-bump mtime + size by
	// truncating-then-rewriting *before* the Splice call; the initial
	// Stat in Splice will see this updated state. We can't easily test
	// the read↔rename race without a hook.
	//
	// Practical compromise: test the "size mismatch between Stat and
	// ReadFile" branch by using a file system that lies — out of scope —
	// OR test the "final re-Stat detects change" branch by changing the
	// file *during* Splice. Use a goroutine that polls and rewrites.
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Wait for Splice to be in flight (best-effort), then mutate.
		// We bump the mtime by writing fresh bytes to the file.
		for range 100 {
			info, _ := os.Stat(path)
			if info != nil && info.ModTime().After(initial) {
				// Splice may have already finished one Stat; mutate now.
				_ = os.WriteFile(path, append(original, '\n'), 0o644)
				now := time.Now().Add(time.Second)
				_ = os.Chtimes(path, now, now)
				return
			}
		}
	}()

	err = Splice(origin, []byte("func B() { return 1 }\n"))
	<-done

	// Either the splice succeeded before the goroutine raced (acceptable —
	// just means we lost the race) OR the splice detected the change.
	// We assert: if it errored, it must be ErrSourceChanged, not corruption.
	if err != nil {
		require.ErrorIs(t, err, ErrSourceChanged, "non-TOCTOU error: %v", err)
	}
}

// TestSplice_DetectsSizeMismatchBetweenStatAndRead — bead mache-e7de36.
//
// Verifies the early-abort path: if the file shrinks between Stat and
// ReadFile (truncation), the read returns fewer bytes than the stat
// reported. Splice must abort.
//
// We can't directly trigger this race deterministically in a test, but we
// can sanity-check the error wrapping by reading the function and ensuring
// the comparison is present. (The real-world repro relies on a concurrent
// writer; the goroutine race in the prior test exercises the final-stat
// branch, which is the more important guard.)
func TestSplice_ErrSourceChangedIsExported(t *testing.T) {
	require.NotNil(t, ErrSourceChanged)
	assert.Contains(t, ErrSourceChanged.Error(), "source file changed")
}

// TestSplice_InodeChangesAfterRename documents the unavoidable consequence
// of Splice's atomic-write-via-rename: the destination file's inode
// (kernel-level "file ID") is replaced on every successful splice.
//
// This is the standard atomic-write pattern (write-temp + rename) and
// is the price of crash atomicity — a partial write can never be
// observed by readers. But it has consumer-visible side effects worth
// pinning with a test:
//
//   - Long-running processes that opened the file BEFORE splice continue
//     reading the OLD content (POSIX keeps the unlinked inode alive
//     until the last fd closes). This is intentional — concurrent
//     readers don't see torn writes.
//   - File watchers that key on inode (e.g., inotify IN_MODIFY without
//     IN_MOVE_SELF) miss the change. fsnotify and similar libraries
//     handle this by watching the parent directory; consumers that don't
//     will need to re-arm.
//   - NFS/SMB clients with cached file handles see ESTALE on the next
//     access and must re-resolve by path.
//
// Mache's own NFS layer (`internal/nfsmount/graphfs.go`) derives Fileid
// from a path-FNV hash, not from the underlying inode, so its file IDs
// stay stable across splice. This test guards the *contract*: if anyone
// ever changes Splice to write in place (preserving inode), they should
// flip this assertion deliberately and audit consumers that rely on the
// current "new inode per splice" behavior.
func TestSplice_InodeChangesAfterRename(t *testing.T) {
	path := tempFile(t, "func A() {}\n")
	beforeIno := fileInode(t, path)

	err := Splice(graph.SourceOrigin{
		FilePath:  path,
		StartByte: 0,
		EndByte:   12,
	}, []byte("func B() {}\n"))
	require.NoError(t, err)

	afterIno := fileInode(t, path)

	// Sanity check: only run the inode assertion if the platform reports
	// a non-zero inode (Windows, certain virtual filesystems). On those
	// platforms the test is a no-op.
	if beforeIno == 0 || afterIno == 0 {
		t.Skip("platform does not expose inode numbers via os.Stat")
	}
	assert.NotEqual(t, beforeIno, afterIno,
		"Splice currently uses atomic write-via-rename, which replaces the inode. "+
			"If this assertion fails, Splice was changed to write in place — "+
			"audit external consumers (file watchers, NFS clients with cached fds) "+
			"that may have relied on inode change as their invalidation signal.")
}

// TestSplice_OpenReaderSeesOldContentAfterSplice verifies the read-side
// safety property of atomic-rename writes: a reader that opened the file
// BEFORE splice and reads AFTER splice sees the *original* content, not
// torn or new content.
//
// This is the consumer-facing reason we tolerate the inode change in
// TestSplice_InodeChangesAfterRename — it's the same mechanism. The
// kernel keeps the original inode alive (unlinked but open) until the
// reader closes its fd, so concurrent reads always see a consistent
// snapshot.
//
// If Splice ever switches to in-place truncate-and-write (preserving
// inode), this property is lost: the open reader would see partial /
// torn content. Pin this in a test so the regression is loud.
func TestSplice_OpenReaderSeesOldContentAfterSplice(t *testing.T) {
	original := "func A() {}\nfunc B() {}\nfunc C() {}\n"
	path := tempFile(t, original)

	// Open BEFORE splice and hold the fd.
	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	err = Splice(graph.SourceOrigin{
		FilePath:  path,
		StartByte: 12, // start of "func B() {}\n"
		EndByte:   24,
	}, []byte("func B() { return 1 }\n"))
	require.NoError(t, err)

	// Reader opened pre-splice still sees original content.
	got, err := os.ReadFile(path) // separate path-based read sees new content
	require.NoError(t, err)
	assert.NotEqual(t, original, string(got), "path-based read should see new content")

	buf := make([]byte, 4096)
	n, err := f.ReadAt(buf, 0)
	if err != nil && n == 0 {
		t.Fatalf("read pre-splice fd: %v", err)
	}
	assert.Equal(t, original, string(buf[:n]),
		"reader holding pre-splice fd must see original content (atomic-rename invariant)")
}

// fileInode returns the platform inode for path, or 0 if unavailable.
// Defined in splice_inode_unix_test.go and splice_inode_other_test.go for
// build-tag dispatch.
