package ingest

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher monitors a directory tree for file changes and invokes callbacks
// when source files are created/modified or deleted. It debounces rapid
// changes so that a burst of writes to the same file produces a single
// callback after a quiet period.
type Watcher struct {
	watcher   *fsnotify.Watcher
	rootDir   string
	gitignore *gitignoreMatcher

	onChange func(path string)
	onDelete func(path string)

	debounce time.Duration

	mu     sync.Mutex
	timers map[string]*time.Timer

	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// WatcherOption configures a Watcher.
type WatcherOption func(*Watcher)

// WithDebounce sets the quiet period before a change callback fires.
// Defaults to 100ms.
func WithDebounce(d time.Duration) WatcherOption {
	return func(w *Watcher) { w.debounce = d }
}

// WithGitignore configures the watcher to skip directories matching gitignore
// rules. This prevents watching build artifact directories (target/, dist/,
// node_modules/) that would otherwise consume thousands of kqueue FDs on macOS.
func WithGitignore(gi *gitignoreMatcher) WatcherOption {
	return func(w *Watcher) { w.gitignore = gi }
}

// NewWatcher creates a file watcher on rootDir. onChange is called for
// created/modified files; onDelete is called for removed files. Both
// callbacks receive the absolute file path. Hidden files, .git
// directories, and non-source extensions are ignored.
func NewWatcher(rootDir string, onChange, onDelete func(path string), opts ...WatcherOption) (*Watcher, error) {
	abs, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, err
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	w := &Watcher{
		watcher:  fsw,
		rootDir:  abs,
		onChange: onChange,
		onDelete: onDelete,
		debounce: 100 * time.Millisecond,
		timers:   make(map[string]*time.Timer),
		done:     make(chan struct{}),
	}

	for _, opt := range opts {
		opt(w)
	}

	// Walk directory tree and add all directories.
	if err := w.addDirsRecursive(abs); err != nil {
		_ = fsw.Close()
		return nil, err
	}

	w.wg.Add(1)
	go w.loop()

	return w, nil
}

// Stop shuts down the watcher. Safe to call concurrently and multiple times.
func (w *Watcher) Stop() {
	w.stopOnce.Do(func() {
		close(w.done)
		_ = w.watcher.Close()
		w.wg.Wait()

		// Cancel any pending timers.
		w.mu.Lock()
		for _, t := range w.timers {
			t.Stop()
		}
		w.timers = nil
		w.mu.Unlock()
	})
}

// loop processes fsnotify events until done is closed.
func (w *Watcher) loop() {
	defer w.wg.Done()
	for {
		select {
		case <-w.done:
			return

		case ev, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			w.handleEvent(ev)

		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("watcher error: %v", err)
		}
	}
}

// handleEvent dispatches a single fsnotify event.
func (w *Watcher) handleEvent(ev fsnotify.Event) {
	path := ev.Name

	if w.shouldIgnorePath(path) {
		return
	}

	// For newly created directories, start watching them.
	if ev.Has(fsnotify.Create) {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			if err := w.addDirsRecursive(path); err != nil {
				log.Printf("watcher: failed to watch new dir %s: %v", path, err)
			}
			return
		}
	}

	// Only fire callbacks for files with known source extensions.
	if !isSourceFile(path) {
		return
	}

	if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
		// File gone -- fire delete immediately (no debounce needed).
		w.cancelTimer(path)
		if w.onDelete != nil {
			w.onDelete(path)
		}
		return
	}

	if ev.Has(fsnotify.Create) || ev.Has(fsnotify.Write) {
		w.debouncedOnChange(path)
	}
}

// debouncedOnChange resets the debounce timer for path.
func (w *Watcher) debouncedOnChange(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.timers == nil {
		return // stopped
	}

	if t, ok := w.timers[path]; ok {
		t.Stop()
	}

	w.timers[path] = time.AfterFunc(w.debounce, func() {
		w.mu.Lock()
		// Guard against timer firing after Stop() has returned.
		// Stop() sets w.timers = nil; if we see nil, the watcher
		// is shut down and we must not call onChange.
		if w.timers == nil {
			w.mu.Unlock()
			return
		}
		delete(w.timers, path)
		w.mu.Unlock()

		// Verify file still exists before calling onChange
		// (could have been deleted between event and timer fire).
		if _, err := os.Stat(path); err == nil {
			if w.onChange != nil {
				w.onChange(path)
			}
		}
	})
}

// cancelTimer removes a pending debounce timer for path.
func (w *Watcher) cancelTimer(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.timers[path]; ok {
		t.Stop()
		delete(w.timers, path)
	}
}

// addDirsRecursive walks root and adds every non-hidden directory to the watcher.
func (w *Watcher) addDirsRecursive(root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if !d.IsDir() {
			return nil
		}
		if w.shouldIgnoreDir(path) {
			return filepath.SkipDir
		}
		return w.watcher.Add(path)
	})
}

// shouldIgnorePath returns true for paths the watcher should skip entirely.
func (w *Watcher) shouldIgnorePath(path string) bool {
	base := filepath.Base(path)

	// Hidden files and directories (e.g. .git, .DS_Store)
	if strings.HasPrefix(base, ".") {
		return true
	}

	// Check for .git anywhere in path components.
	for _, seg := range strings.Split(path, string(filepath.Separator)) {
		if seg == ".git" {
			return true
		}
	}

	// Check gitignore for files inside ignored directories.
	if w.gitignore != nil {
		rel, err := filepath.Rel(w.rootDir, path)
		if err == nil {
			rel = filepath.ToSlash(rel)
			if w.gitignore.Match(rel, false) {
				return true
			}
		}
	}

	return false
}

// shouldIgnoreDir returns true for directories the watcher should not recurse
// into. Uses ShouldSkipDir (the canonical engine skip list) as a baseline,
// then defers to gitignore rules when available — so project-specific ignores
// (target/, dist/, .terraform/, etc.) are respected automatically.
func (w *Watcher) shouldIgnoreDir(path string) bool {
	base := filepath.Base(path)

	// Canonical skip list shared with the engine (hidden dirs, target, dist, etc.)
	if ShouldSkipDir(base) {
		return true
	}

	// Gitignore: covers project-specific patterns without maintaining a list.
	if w.gitignore != nil {
		rel, err := filepath.Rel(w.rootDir, path)
		if err == nil {
			rel = filepath.ToSlash(rel)
			if w.gitignore.Match(rel, true) {
				return true
			}
		}
	}

	return false
}

// sourceExtensions is the set of file extensions the engine can ingest.
// Matches the extensions handled by langForExt plus .json.
var sourceExtensions = map[string]bool{
	".go":   true,
	".py":   true,
	".js":   true,
	".ts":   true,
	".tsx":  true,
	".sql":  true,
	".tf":   true,
	".hcl":  true,
	".yaml": true,
	".yml":  true,
	".rs":   true,
	".toml": true,
	".ex":   true,
	".exs":  true,
	".json": true,
}

// isSourceFile returns true if the path has a recognized source extension.
func isSourceFile(path string) bool {
	return sourceExtensions[filepath.Ext(path)]
}
