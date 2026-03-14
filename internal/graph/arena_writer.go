package graph

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/agentic-research/mache/internal/control"
)

// ArenaFlusher writes a serialized .db file into the double-buffered arena
// and atomically flips the header so readers see the new version.
//
// Flush is O(N) where N = DB size — the entire .db is copied to the inactive
// buffer on every flush. To amortize this cost, callers should use
// RequestFlush (coalesced) instead of FlushNow (synchronous) for writes.
// The coalescing goroutine batches rapid writes into a single flush per
// tick interval (default 100ms), so N concurrent agent writes within a
// tick produce 1 flush instead of N.
//
// Benchmarked on M3 Max (BenchmarkArenaFlush):
//
//	108 KB  → ~4ms  (fsync-dominated floor)
//	  1 MB  → ~5ms
//	 10 MB  → ~10-25ms
//
// Flush uses page-level diff: only changed SQLite pages are written to
// the inactive buffer, avoiding a full-DB copy on every swap.
type ArenaFlusher struct {
	arenaPath    string
	masterDBPath string
	ctrl         *control.Controller

	// Coalescing state
	mu       sync.Mutex
	dirty    bool
	flushErr error // last flush error, readable via LastError()
	tick     *time.Ticker
	stopCh   chan struct{}
	stopped  bool
}

// NewArenaFlusher creates a flusher that targets the given arena file
// and updates the control block on each flush. The masterDBPath is the
// writable temp file that WritableGraph mutates.
//
// Call Start() to begin the coalescing goroutine, and Close() to stop
// it and perform a final flush.
func NewArenaFlusher(arenaPath, masterDBPath string, ctrl *control.Controller) *ArenaFlusher {
	return &ArenaFlusher{
		arenaPath:    arenaPath,
		masterDBPath: masterDBPath,
		ctrl:         ctrl,
		stopCh:       make(chan struct{}),
	}
}

// Start begins the coalescing goroutine that flushes at most once per
// interval when dirty. Safe to call multiple times (idempotent).
func (f *ArenaFlusher) Start(interval time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tick != nil {
		return // already started
	}
	f.tick = time.NewTicker(interval)
	go f.coalesceLoop()
}

func (f *ArenaFlusher) coalesceLoop() {
	for {
		select {
		case <-f.tick.C:
			f.mu.Lock()
			if f.dirty {
				f.dirty = false
				f.mu.Unlock()
				if err := f.flushInternal(); err != nil {
					f.mu.Lock()
					f.flushErr = err
					f.mu.Unlock()
					log.Printf("arena flush: %v", err)
				}
			} else {
				f.mu.Unlock()
			}
		case <-f.stopCh:
			return
		}
	}
}

// RequestFlush marks the flusher as dirty. The coalescing goroutine will
// perform the actual flush on the next tick. Non-blocking, O(1).
func (f *ArenaFlusher) RequestFlush() {
	f.mu.Lock()
	f.dirty = true
	f.mu.Unlock()
}

// FlushNow performs a synchronous flush. Use for final flush on unmount
// or when the caller needs to guarantee the arena is up-to-date.
func (f *ArenaFlusher) FlushNow() error {
	f.mu.Lock()
	f.dirty = false
	f.mu.Unlock()
	return f.flushInternal()
}

// LastError returns the last error from the coalescing goroutine.
func (f *ArenaFlusher) LastError() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.flushErr
}

// Close stops the coalescing goroutine and performs a final synchronous
// flush if dirty.
func (f *ArenaFlusher) Close() error {
	f.mu.Lock()
	if f.stopped {
		f.mu.Unlock()
		return nil
	}
	f.stopped = true
	wasDirty := f.dirty
	f.dirty = false
	if f.tick != nil {
		f.tick.Stop()
		close(f.stopCh)
	}
	f.mu.Unlock()

	if wasDirty {
		return f.flushInternal()
	}
	return nil
}

// flushInternal reads the master .db file, diffs it page-by-page against
// the inactive arena buffer, writes only changed pages, flips the active
// buffer index, increments the sequence, and updates the control block.
func (f *ArenaFlusher) flushInternal() error {
	dbBytes, err := os.ReadFile(f.masterDBPath)
	if err != nil {
		return fmt.Errorf("read master db: %w", err)
	}

	af, err := os.OpenFile(f.arenaPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open arena: %w", err)
	}
	defer func() { _ = af.Close() }()

	info, err := af.Stat()
	if err != nil {
		return fmt.Errorf("stat arena: %w", err)
	}

	header, err := ReadArenaHeader(af)
	if err != nil {
		return fmt.Errorf("read arena header: %w", err)
	}

	// Calculate buffer geometry
	bufferSize := (info.Size() - ArenaHeaderSize) / 2
	if int64(len(dbBytes)) > bufferSize {
		return fmt.Errorf("db size %d exceeds arena buffer size %d", len(dbBytes), bufferSize)
	}

	// Write to inactive buffer using page-level diff
	inactive := uint8(1) - header.ActiveBuffer
	inactiveOffset := int64(ArenaHeaderSize) + int64(inactive)*bufferSize

	pageSize := dbPageSize(dbBytes)

	// Read existing inactive buffer for comparison
	oldBuf := make([]byte, bufferSize)
	if _, err := af.ReadAt(oldBuf, inactiveOffset); err != nil {
		return fmt.Errorf("read inactive buffer: %w", err)
	}

	// Write only changed pages within DB content
	for off := int64(0); off < int64(len(dbBytes)); off += int64(pageSize) {
		end := off + int64(pageSize)
		if end > int64(len(dbBytes)) {
			end = int64(len(dbBytes))
		}
		if !bytes.Equal(dbBytes[off:end], oldBuf[off:end]) {
			if _, err := af.WriteAt(dbBytes[off:end], inactiveOffset+off); err != nil {
				return fmt.Errorf("write page at offset %d: %w", off, err)
			}
		}
	}

	// Zero-fill only non-zero pages in the remainder (beyond DB content)
	for off := int64(len(dbBytes)); off < bufferSize; off += int64(pageSize) {
		end := off + int64(pageSize)
		if end > bufferSize {
			end = bufferSize
		}
		if !isZeroPage(oldBuf[off:end]) {
			zeros := make([]byte, end-off)
			if _, err := af.WriteAt(zeros, inactiveOffset+off); err != nil {
				return fmt.Errorf("zero-pad at offset %d: %w", off, err)
			}
		}
	}

	// Flip header: active_buffer ^= 1, sequence++
	header.ActiveBuffer = inactive
	header.Sequence++
	if err := WriteArenaHeader(af, header); err != nil {
		return fmt.Errorf("write arena header: %w", err)
	}

	if err := af.Sync(); err != nil {
		return fmt.Errorf("sync arena: %w", err)
	}

	// Update control block so ley-line detects the change
	if f.ctrl != nil {
		if err := f.ctrl.SetArena(f.arenaPath, uint64(info.Size()), header.Sequence); err != nil {
			return fmt.Errorf("update control block: %w", err)
		}
	}

	return nil
}

// dbPageSize extracts the SQLite page size from the database file header.
// Returns 4096 if the header is missing or invalid.
func dbPageSize(db []byte) int {
	if len(db) < 100 {
		return 4096
	}
	ps := int(binary.BigEndian.Uint16(db[16:18]))
	if ps == 1 {
		return 65536 // SQLite convention: 1 means 65536
	}
	if ps < 512 || ps > 65536 || ps&(ps-1) != 0 {
		return 4096
	}
	return ps
}

// isZeroPage reports whether all bytes in b are zero.
func isZeroPage(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}
