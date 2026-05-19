package graph

import (
	"os"
	"time"
)

// SetRefresher configures a callback invoked when a source file is stale.
// The callback should re-ingest the file and update the store.
func (s *MemoryStore) SetRefresher(fn func(filePath string) error) {
	s.refresher = fn
}

// RecordFileMtime explicitly records the mtime for a source file.
// Called after re-ingestion to update the tracked mtime.
func (s *MemoryStore) RecordFileMtime(filePath string, mtime time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fileMtimes[filePath] = mtime
}

// FileMtime returns the tracked mtime for a source file.
// Returns zero time if the file is not tracked.
func (s *MemoryStore) FileMtime(filePath string) time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.fileMtimes[filePath]
}

// IsFileStale returns true if the source file's current mtime differs
// from the tracked mtime (i.e., the file has been modified since indexing).
func (s *MemoryStore) IsFileStale(filePath string) bool {
	s.mu.RLock()
	tracked, ok := s.fileMtimes[filePath]
	s.mu.RUnlock()
	if !ok {
		return false // not tracked → not stale
	}
	info, err := os.Stat(filePath)
	if err != nil {
		return true // can't stat (deleted?) → treat as stale
	}
	return !info.ModTime().Equal(tracked)
}
