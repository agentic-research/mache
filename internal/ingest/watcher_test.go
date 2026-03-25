package ingest

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWatcher_Debounce(t *testing.T) {
	tmpDir := t.TempDir()

	var callCount atomic.Int32
	var lastPath string
	var mu sync.Mutex

	onChange := func(path string) {
		callCount.Add(1)
		mu.Lock()
		lastPath = path
		mu.Unlock()
	}
	onDelete := func(path string) {}

	w, err := NewWatcher(tmpDir, onChange, onDelete, WithDebounce(50*time.Millisecond))
	require.NoError(t, err)
	defer w.Stop()

	// Write rapid changes to a single Go file.
	goFile := filepath.Join(tmpDir, "main.go")
	for i := range 5 {
		err := os.WriteFile(goFile, []byte("package main // v"+string(rune('0'+i))), 0o644)
		require.NoError(t, err)
		time.Sleep(10 * time.Millisecond) // faster than debounce
	}

	// Wait for debounce to settle.
	time.Sleep(200 * time.Millisecond)

	// Should have coalesced into a single callback.
	count := callCount.Load()
	assert.Equal(t, int32(1), count, "rapid writes should produce exactly 1 callback, got %d", count)

	mu.Lock()
	assert.Equal(t, goFile, lastPath)
	mu.Unlock()
}

func TestWatcher_IgnoresGitDir(t *testing.T) {
	tmpDir := t.TempDir()

	var callCount atomic.Int32
	onChange := func(path string) {
		callCount.Add(1)
	}
	onDelete := func(path string) {}

	w, err := NewWatcher(tmpDir, onChange, onDelete, WithDebounce(20*time.Millisecond))
	require.NoError(t, err)
	defer w.Stop()

	// Create a .git directory with a file inside.
	gitDir := filepath.Join(tmpDir, ".git")
	require.NoError(t, os.MkdirAll(gitDir, 0o755))
	err = os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main"), 0o644)
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(0), callCount.Load(), ".git files should be ignored")
}

func TestWatcher_IgnoresHiddenFiles(t *testing.T) {
	tmpDir := t.TempDir()

	var callCount atomic.Int32
	onChange := func(path string) {
		callCount.Add(1)
	}
	onDelete := func(path string) {}

	w, err := NewWatcher(tmpDir, onChange, onDelete, WithDebounce(20*time.Millisecond))
	require.NoError(t, err)
	defer w.Stop()

	// Write a hidden file.
	err = os.WriteFile(filepath.Join(tmpDir, ".hidden.go"), []byte("package main"), 0o644)
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(0), callCount.Load(), "hidden files should be ignored")
}

func TestWatcher_IgnoresNonSourceExtensions(t *testing.T) {
	tmpDir := t.TempDir()

	var callCount atomic.Int32
	onChange := func(path string) {
		callCount.Add(1)
	}
	onDelete := func(path string) {}

	w, err := NewWatcher(tmpDir, onChange, onDelete, WithDebounce(20*time.Millisecond))
	require.NoError(t, err)
	defer w.Stop()

	// Write a .txt file (not a source extension).
	err = os.WriteFile(filepath.Join(tmpDir, "notes.txt"), []byte("hello"), 0o644)
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(0), callCount.Load(), "non-source extensions should be ignored")
}

func TestWatcher_DeleteCallback(t *testing.T) {
	tmpDir := t.TempDir()

	var deletedPath string
	var deleteMu sync.Mutex
	var deleteCount atomic.Int32

	onChange := func(path string) {}
	onDelete := func(path string) {
		deleteCount.Add(1)
		deleteMu.Lock()
		deletedPath = path
		deleteMu.Unlock()
	}

	// Create a file before starting the watcher.
	goFile := filepath.Join(tmpDir, "remove_me.go")
	require.NoError(t, os.WriteFile(goFile, []byte("package main"), 0o644))

	w, err := NewWatcher(tmpDir, onChange, onDelete, WithDebounce(20*time.Millisecond))
	require.NoError(t, err)
	defer w.Stop()

	// Remove the file.
	require.NoError(t, os.Remove(goFile))

	time.Sleep(100 * time.Millisecond)

	assert.Equal(t, int32(1), deleteCount.Load(), "delete should fire once")
	deleteMu.Lock()
	assert.Equal(t, goFile, deletedPath)
	deleteMu.Unlock()
}

func TestWatcher_NewSubdirectory(t *testing.T) {
	tmpDir := t.TempDir()

	var callCount atomic.Int32
	var lastPath string
	var mu sync.Mutex

	onChange := func(path string) {
		callCount.Add(1)
		mu.Lock()
		lastPath = path
		mu.Unlock()
	}
	onDelete := func(path string) {}

	w, err := NewWatcher(tmpDir, onChange, onDelete, WithDebounce(20*time.Millisecond))
	require.NoError(t, err)
	defer w.Stop()

	// Create a new subdirectory with a source file.
	subDir := filepath.Join(tmpDir, "pkg")
	require.NoError(t, os.MkdirAll(subDir, 0o755))

	// Small delay to let the watcher pick up the new dir.
	time.Sleep(50 * time.Millisecond)

	goFile := filepath.Join(subDir, "lib.go")
	require.NoError(t, os.WriteFile(goFile, []byte("package pkg"), 0o644))

	time.Sleep(200 * time.Millisecond)

	assert.GreaterOrEqual(t, callCount.Load(), int32(1), "should detect file in new subdirectory")
	mu.Lock()
	assert.Equal(t, goFile, lastPath)
	mu.Unlock()
}

func TestWatcher_StopIsIdempotent(t *testing.T) {
	tmpDir := t.TempDir()
	w, err := NewWatcher(tmpDir, func(string) {}, func(string) {})
	require.NoError(t, err)

	w.Stop()
	w.Stop() // should not panic
}

func TestIsSourceFile(t *testing.T) {
	assert.True(t, isSourceFile("main.go"))
	assert.True(t, isSourceFile("/a/b/c.py"))
	assert.True(t, isSourceFile("app.tsx"))
	assert.True(t, isSourceFile("data.json"))
	assert.True(t, isSourceFile("schema.yaml"))
	assert.True(t, isSourceFile("config.toml"))
	assert.True(t, isSourceFile("infra.tf"))
	assert.True(t, isSourceFile("lib.rs"))
	assert.True(t, isSourceFile("mix.exs"))

	assert.True(t, isSourceFile("readme.md")) // markdown now supported
	assert.False(t, isSourceFile("notes.txt"))
	assert.False(t, isSourceFile("data.csv"))
	assert.False(t, isSourceFile("image.png"))
	assert.False(t, isSourceFile("binary.exe"))
}

func TestShouldIgnorePath(t *testing.T) {
	w := &Watcher{rootDir: "/repo"}

	assert.True(t, w.shouldIgnorePath("/repo/.git/HEAD"))
	assert.True(t, w.shouldIgnorePath("/repo/.hidden"))
	assert.True(t, w.shouldIgnorePath(".DS_Store"))

	assert.False(t, w.shouldIgnorePath("/repo/main.go"))
	assert.False(t, w.shouldIgnorePath("/repo/internal/pkg/file.go"))
}

func TestShouldIgnoreDir(t *testing.T) {
	w := &Watcher{rootDir: "/repo"}

	// ShouldSkipDir canonical list
	assert.True(t, w.shouldIgnoreDir("/repo/node_modules"))
	assert.True(t, w.shouldIgnoreDir("/repo/target"))
	assert.True(t, w.shouldIgnoreDir("/repo/dist"))
	assert.True(t, w.shouldIgnoreDir("/repo/build"))
	assert.True(t, w.shouldIgnoreDir("/repo/__pycache__"))
	assert.True(t, w.shouldIgnoreDir("/repo/.git"))
	assert.True(t, w.shouldIgnoreDir("/repo/.hidden"))

	assert.False(t, w.shouldIgnoreDir("/repo/internal"))
	assert.False(t, w.shouldIgnoreDir("/repo/cmd"))
	assert.False(t, w.shouldIgnoreDir("/repo/pkg"))
}

func TestWatcher_VendorIgnored(t *testing.T) {
	dir := t.TempDir()
	var called int32

	w, err := NewWatcher(dir, func(path string) {
		atomic.AddInt32(&called, 1)
	}, nil, WithDebounce(20*time.Millisecond))
	require.NoError(t, err)
	defer w.Stop()

	// Create vendor directory and write a .go file inside it
	vendorDir := filepath.Join(dir, "vendor")
	require.NoError(t, os.MkdirAll(vendorDir, 0o755))
	time.Sleep(50 * time.Millisecond) // let fsnotify settle

	require.NoError(t, os.WriteFile(filepath.Join(vendorDir, "dep.go"), []byte("package dep"), 0o644))
	time.Sleep(100 * time.Millisecond)

	assert.Equal(t, int32(0), atomic.LoadInt32(&called), "vendor/ files should be ignored")
}

// TestWatcher_TargetIgnored is a regression test for the FD leak where the
// watcher did not skip build artifact directories like target/ (Rust), dist/,
// build/. On macOS (kqueue), each watched directory consumes an FD, so watching
// a Rust target/ tree with 11K+ subdirectories leaked thousands of FDs.
func TestWatcher_TargetIgnored(t *testing.T) {
	dir := t.TempDir()
	var called int32

	w, err := NewWatcher(dir, func(path string) {
		atomic.AddInt32(&called, 1)
	}, nil, WithDebounce(20*time.Millisecond))
	require.NoError(t, err)
	defer w.Stop()

	// Simulate Rust build artifacts: target/debug/deps/
	for _, subdir := range []string{
		"target/debug/deps",
		"target/debug/build/somecrate-abc123",
		"target/release/deps",
		"dist/assets",
		"build/output",
	} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, subdir), 0o755))
	}
	time.Sleep(50 * time.Millisecond)

	// Write files that would match sourceExtensions
	for _, f := range []string{
		"target/debug/deps/main.rs",
		"target/debug/build/somecrate-abc123/build-script-build",
		"dist/assets/app.js",
		"build/output/lib.go",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, f), []byte("// code"), 0o644))
	}
	time.Sleep(100 * time.Millisecond)

	assert.Equal(t, int32(0), atomic.LoadInt32(&called),
		"files in target/, dist/, build/ should be ignored by watcher")
}

// TestWatcher_GitignoreSkipsDirs verifies that the watcher respects .gitignore
// rules passed via WithGitignore, preventing FD exhaustion from project-specific
// build output directories that aren't in the hardcoded skip list.
func TestWatcher_GitignoreSkipsDirs(t *testing.T) {
	dir := t.TempDir()

	// Create a .gitignore that ignores a custom build dir
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("custom_build/\n"), 0o644))
	gi := LoadGitignore(dir)
	require.NotNil(t, gi)

	var called int32
	w, err := NewWatcher(dir, func(path string) {
		atomic.AddInt32(&called, 1)
	}, nil, WithDebounce(20*time.Millisecond), WithGitignore(gi))
	require.NoError(t, err)
	defer w.Stop()

	// Create the gitignored directory and write source files
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "custom_build", "out"), 0o755))
	time.Sleep(50 * time.Millisecond)

	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "custom_build", "out", "gen.go"),
		[]byte("package gen"), 0o644))
	time.Sleep(100 * time.Millisecond)

	assert.Equal(t, int32(0), atomic.LoadInt32(&called),
		"gitignored directories should not trigger watcher callbacks")

	// Verify non-ignored files still trigger callbacks
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644))
	time.Sleep(100 * time.Millisecond)

	assert.Equal(t, int32(1), atomic.LoadInt32(&called),
		"non-ignored source files should still trigger callbacks")
}
