package graph

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"time"

	"github.com/RoaringBitmap/roaring"
	"github.com/agentic-research/mache/internal/refsvtab"
	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// MemoryStore SQL query support (in-memory SQLite sidecar)
// ---------------------------------------------------------------------------

// InitRefsDB opens an in-memory SQLite database with the same schema as
// SQLiteGraph's sidecar (node_refs + file_ids + mache_refs vtab).
// Must be called before FlushRefs. Safe to call multiple times (idempotent).
func (s *MemoryStore) InitRefsDB() error {
	if s.refsDB != nil {
		return nil
	}

	refsMod, err := refsvtab.Register()
	if err != nil {
		return err
	}

	// Use a temp file (not :memory:) because the vtab's xFilter runs inside
	// the SQLite engine on the outer connection and needs a SECOND pool
	// connection to query node_refs/file_ids. With :memory:, each connection
	// gets its own isolated database. A temp file + WAL mode lets both
	// connections see the same tables — same pattern as SQLiteGraph's .refs.db.
	tmpFile, err := os.CreateTemp("", "mache-refs-*.db")
	if err != nil {
		return fmt.Errorf("create temp refs db: %w", err)
	}
	refsPath := tmpFile.Name()
	_ = tmpFile.Close()

	db, err := sql.Open("sqlite", refsPath)
	if err != nil {
		_ = os.Remove(refsPath) // cleanup temp file
		return fmt.Errorf("open refs db: %w", err)
	}
	// Allow 2 connections: one for normal queries, one for vtab Filter callbacks.
	// WAL mode ensures concurrent readers don't conflict.
	db.SetMaxOpenConns(2)

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = db.Close()          // ignore close error
		_ = os.Remove(refsPath) // cleanup temp file
		return fmt.Errorf("set WAL mode on refs db: %w", err)
	}

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS node_refs (
			token TEXT PRIMARY KEY,
			bitmap BLOB
		);
		CREATE TABLE IF NOT EXISTS file_ids (
			id INTEGER PRIMARY KEY,
			path TEXT UNIQUE NOT NULL
		);
	`)
	if err != nil {
		_ = db.Close()          // ignore close error
		_ = os.Remove(refsPath) // cleanup temp file
		return fmt.Errorf("create refs tables: %w", err)
	}

	// Generate a unique ID for this DB connection to register with the vtab module.
	// This allows multiple MemoryStore instances (e.g. tests) to coexist without
	// race conditions on a single global refsDB pointer.
	dbID := fmt.Sprintf("mem_%d", time.Now().UnixNano())
	refsMod.RegisterDB(dbID, db)

	// Declare vtab with the unique ID as an argument.
	// The Create method in refs_module.go will look up the DB using this ID.
	query := fmt.Sprintf("CREATE VIRTUAL TABLE IF NOT EXISTS mache_refs USING mache_refs(%s)", dbID)
	if _, err := db.Exec(query); err != nil {
		refsMod.UnregisterDB(dbID)
		_ = db.Close()          // ignore close error
		_ = os.Remove(refsPath) // cleanup temp file
		return fmt.Errorf("create mache_refs vtab: %w", err)
	}

	s.refsDB = db
	s.refsDBPath = refsPath
	s.dbID = dbID
	return nil
}

// FlushRefs writes all accumulated refs (from AddRef) into the in-memory
// SQLite sidecar as roaring bitmaps. Guarded by sync.Once — safe to call
// multiple times; only the first call performs the flush.
func (s *MemoryStore) FlushRefs() error {
	s.flushOnce.Do(func() {
		s.flushErr = s.flushRefsInternal()
	})
	return s.flushErr
}

func (s *MemoryStore) flushRefsInternal() error {
	if s.refsDB == nil {
		return fmt.Errorf("refsDB not initialized: call InitRefsDB first")
	}

	s.mu.RLock()
	refs := s.refs
	s.mu.RUnlock()

	if len(refs) == 0 {
		return nil
	}

	// Build file ID map from all unique paths
	fileIDMap := make(map[string]uint32)
	var nextID uint32
	for _, paths := range refs {
		for _, p := range paths {
			if _, ok := fileIDMap[p]; !ok {
				fileIDMap[p] = nextID
				nextID++
			}
		}
	}

	// Build roaring bitmaps per token
	bitmaps := make(map[string]*roaring.Bitmap, len(refs))
	for token, paths := range refs {
		bm := roaring.New()
		for _, p := range paths {
			bm.Add(fileIDMap[p])
		}
		bitmaps[token] = bm
	}

	// Write both tables in a single transaction
	tx, err := s.refsDB.Begin()
	if err != nil {
		return fmt.Errorf("begin refs flush: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // safe to ignore (no-op if committed)

	fileStmt, err := tx.Prepare("INSERT OR IGNORE INTO file_ids (id, path) VALUES (?, ?)")
	if err != nil {
		return fmt.Errorf("prepare file_ids insert: %w", err)
	}
	defer func() { _ = fileStmt.Close() }() // safe to ignore

	for path, id := range fileIDMap {
		if _, err := fileStmt.Exec(id, path); err != nil {
			return fmt.Errorf("insert file_id %s: %w", path, err)
		}
	}

	refStmt, err := tx.Prepare("INSERT OR REPLACE INTO node_refs (token, bitmap) VALUES (?, ?)")
	if err != nil {
		return fmt.Errorf("prepare node_refs insert: %w", err)
	}
	defer func() { _ = refStmt.Close() }() // safe to ignore

	var buf bytes.Buffer
	for token, bm := range bitmaps {
		buf.Reset()
		if _, err := bm.WriteTo(&buf); err != nil {
			return fmt.Errorf("serialize bitmap for %s: %w", token, err)
		}
		if _, err := refStmt.Exec(token, buf.Bytes()); err != nil {
			return fmt.Errorf("insert ref %s: %w", token, err)
		}
	}

	return tx.Commit()
}

// QueryRefs executes a SQL query against the in-memory refs database,
// which includes the mache_refs virtual table.
func (s *MemoryStore) QueryRefs(query string, args ...any) (*sql.Rows, error) {
	if s.refsDB == nil {
		return nil, fmt.Errorf("refsDB not initialized: call InitRefsDB first")
	}
	return s.refsDB.Query(query, args...)
}

// Close closes the refs database and removes the temp file.
func (s *MemoryStore) Close() error {
	if s.refsDB != nil {
		// Unregister from vtab module to prevent leaks/races
		if mod, err := refsvtab.Register(); err == nil && mod != nil {
			mod.UnregisterDB(s.dbID)
		}

		err := s.refsDB.Close()
		if s.refsDBPath != "" {
			_ = os.Remove(s.refsDBPath) // best-effort cleanup
			_ = os.Remove(s.refsDBPath + "-wal")
			_ = os.Remove(s.refsDBPath + "-shm")
		}
		return err
	}
	return nil
}
