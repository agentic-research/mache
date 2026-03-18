package ingest

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/agentic-research/mache/internal/graph"
	_ "modernc.org/sqlite"
)

// defaultBatchSize is the number of inserts per transaction batch.
// Larger batches reduce commit overhead; 10K balances throughput vs memory.
const defaultBatchSize = 10_000

// SQLiteWriter implements IngestionTarget for the new high-performance schema.
type SQLiteWriter struct {
	db        *sql.DB
	tx        *sql.Tx
	stmtNode  *sql.Stmt
	stmtRef   *sql.Stmt // For adding refs
	stmtDef   *sql.Stmt // For adding defs
	stmtFile  *sql.Stmt // For recording file metadata (incremental index)
	batchSize int
	count     int
	firstErr  error // first insert/batch error, surfaced by Close
	mu        sync.Mutex
}

// NewSQLiteWriter creates a new writer and initializes the schema.
func NewSQLiteWriter(dbPath string) (*SQLiteWriter, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", dbPath, err)
	}

	// Performance tuning for bulk insert
	if _, err := db.Exec("PRAGMA synchronous = OFF"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec("PRAGMA journal_mode = MEMORY"); err != nil {
		_ = db.Close()
		return nil, err
	}

	// 1. Create Tables
	schema := `
	CREATE TABLE IF NOT EXISTS nodes (
		id TEXT PRIMARY KEY,
		parent_id TEXT,
		name TEXT NOT NULL,
		kind INTEGER NOT NULL,
		size INTEGER DEFAULT 0,
		mtime INTEGER NOT NULL,
		record_id TEXT,
		record JSON,
		source_file TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_parent_name ON nodes(parent_id, name);
	CREATE INDEX IF NOT EXISTS idx_source_file ON nodes(source_file);

	CREATE TABLE IF NOT EXISTS node_refs (
		token TEXT,
		node_id TEXT,
		PRIMARY KEY (token, node_id)
	) WITHOUT ROWID;

	CREATE TABLE IF NOT EXISTS node_defs (
		token TEXT,
		dir_id TEXT,
		PRIMARY KEY (token, dir_id)
	) WITHOUT ROWID;

	CREATE TABLE IF NOT EXISTS file_index (
		path TEXT PRIMARY KEY,
		mod_time INTEGER NOT NULL,
		size INTEGER NOT NULL
	);
	`
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	w := &SQLiteWriter{
		db:        db,
		batchSize: defaultBatchSize,
	}

	if err := w.beginTx(); err != nil {
		_ = db.Close()
		return nil, err
	}

	return w, nil
}

func (w *SQLiteWriter) beginTx() error {
	var err error
	w.tx, err = w.db.Begin()
	if err != nil {
		return err
	}
	// Prepare statement for fast inserts
	w.stmtNode, err = w.tx.Prepare(`
		INSERT OR REPLACE INTO nodes (id, parent_id, name, kind, size, mtime, record_id, record, source_file)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}

	w.stmtRef, err = w.tx.Prepare(`INSERT OR IGNORE INTO node_refs (token, node_id) VALUES (?, ?)`)
	if err != nil {
		return err
	}

	w.stmtDef, err = w.tx.Prepare(`INSERT OR IGNORE INTO node_defs (token, dir_id) VALUES (?, ?)`)
	if err != nil {
		return err
	}

	w.stmtFile, err = w.tx.Prepare(`INSERT OR REPLACE INTO file_index (path, mod_time, size) VALUES (?, ?, ?)`)
	return err
}

func (w *SQLiteWriter) commitTx() error {
	if w.stmtNode != nil {
		_ = w.stmtNode.Close()
	}
	if w.stmtRef != nil {
		_ = w.stmtRef.Close()
	}
	if w.stmtDef != nil {
		_ = w.stmtDef.Close()
	}
	if w.stmtFile != nil {
		_ = w.stmtFile.Close()
	}
	if err := w.tx.Commit(); err != nil {
		return err
	}
	return nil
}

// RecordFile stores file metadata for incremental re-ingestion.
// On subsequent mounts, files with matching (path, mod_time, size) are skipped.
func (w *SQLiteWriter) RecordFile(path string, modTime time.Time, size int64) {
	w.mu.Lock()
	defer w.mu.Unlock()

	_, err := w.stmtFile.Exec(path, modTime.UnixNano(), size)
	if err != nil {
		log.Printf("SQLiteWriter: record file failed for %s: %v", path, err)
		if w.firstErr == nil {
			w.firstErr = fmt.Errorf("record file %s: %w", path, err)
		}
	}
}

// LoadFileIndex reads the file_index table from an existing index database.
// Returns a map of path → (modTime, size) for incremental comparison.
func LoadFileIndex(dbPath string) (map[string]FileIndexEntry, error) {
	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	// Check if file_index table exists
	var tableName string
	err = db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='file_index'").Scan(&tableName)
	if err == sql.ErrNoRows {
		return nil, nil // Table doesn't exist, no cached index
	}
	if err != nil {
		return nil, err
	}

	rows, err := db.Query("SELECT path, mod_time, size FROM file_index")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	index := make(map[string]FileIndexEntry)
	for rows.Next() {
		var path string
		var modTimeNano int64
		var size int64
		if err := rows.Scan(&path, &modTimeNano, &size); err != nil {
			return nil, err
		}
		index[path] = FileIndexEntry{
			ModTime: time.Unix(0, modTimeNano),
			Size:    size,
		}
	}
	return index, rows.Err()
}

// FileIndexEntry stores cached file metadata for incremental comparison.
type FileIndexEntry struct {
	ModTime time.Time
	Size    int64
}

// AddNode writes a node to the database.
func (w *SQLiteWriter) AddNode(n *graph.Node) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// 1. Determine Parent ID and Name
	var parentID *string
	name := n.ID
	if n.ID == "" || n.ID == "." {
		// Root node
		name = "" // Or specific root name if needed
	} else {
		// Split path
		i := strings.LastIndex(n.ID, "/")
		if i != -1 {
			p := n.ID[:i]
			parentID = &p
			name = n.ID[i+1:]
		} else {
			// Top-level node (parent is root, implied or explicit?)
			// If we treat "" as root ID, then parent is "".
			p := ""
			parentID = &p
		}
	}

	// Store full os.FileMode for fidelity (kind column: 1=dir, 0=file).
	kind := 0
	if n.Mode.IsDir() {
		kind = 1
	}

	// 3. Record ID (for lazy loading)
	var recordID *string
	if n.Ref != nil {
		r := n.Ref.RecordID
		recordID = &r
	}

	// 4. Record content: prefer inline Data (rendered file content),
	// fall back to serialized Properties for metadata nodes.
	var record []byte
	if n.Data != nil {
		record = n.Data
	} else if len(n.Properties) > 0 {
		record, _ = json.Marshal(n.Properties)
	}

	// 5. Source file (for DeleteFileNodes support in incremental mode)
	var sourceFile *string
	if n.Origin != nil && n.Origin.FilePath != "" {
		sf := n.Origin.FilePath
		sourceFile = &sf
	}

	// 6. Insert
	_, err := w.stmtNode.Exec(
		n.ID,
		parentID,
		name,
		kind,
		n.ContentSize(),
		n.ModTime.UnixNano(),
		recordID,
		record,
		sourceFile,
	)
	if err != nil {
		log.Printf("SQLiteWriter: insert failed for %s: %v", n.ID, err)
		if w.firstErr == nil {
			w.firstErr = fmt.Errorf("insert %s: %w", n.ID, err)
		}
		return
	}

	w.count++
	if w.count >= w.batchSize {
		if err := w.commitTx(); err != nil {
			log.Printf("SQLiteWriter: commit failed: %v", err)
			if w.firstErr == nil {
				w.firstErr = fmt.Errorf("commit batch: %w", err)
			}
		}
		if err := w.beginTx(); err != nil {
			log.Printf("SQLiteWriter: begin failed: %v", err)
			if w.firstErr == nil {
				w.firstErr = fmt.Errorf("begin batch: %w", err)
			}
		}
		w.count = 0
	}
}

func (w *SQLiteWriter) AddRoot(n *graph.Node) {
	// Root is just a node in this flat schema
	w.AddNode(n)
}

func (w *SQLiteWriter) AddRef(token, nodeID string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	// We use the same transaction as nodes
	_, err := w.stmtRef.Exec(token, nodeID)
	return err
}

func (w *SQLiteWriter) AddDef(token, dirID string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	_, err := w.stmtDef.Exec(token, dirID)
	return err
}

func (w *SQLiteWriter) DeleteFileNodes(filePath string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Delete refs and defs for nodes originating from this file,
	// then delete the nodes themselves using the indexed source_file column.
	_, _ = w.tx.Exec(`DELETE FROM node_refs WHERE node_id IN (
		SELECT id FROM nodes WHERE source_file = ?
	)`, filePath)
	_, _ = w.tx.Exec(`DELETE FROM node_defs WHERE dir_id IN (
		SELECT id FROM nodes WHERE source_file = ?
	)`, filePath)
	_, _ = w.tx.Exec(`DELETE FROM nodes WHERE source_file = ?`, filePath)
}

func (w *SQLiteWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.commitTx(); err != nil {
		_ = w.db.Close()
		return err
	}

	if err := w.db.Close(); err != nil {
		return err
	}

	return w.firstErr
}

// --- Graph Interface Implementation (for IngestionTarget) ---

func (w *SQLiteWriter) GetNode(id string) (*graph.Node, error) {
	// Engine calls GetNode to check existence and update children.
	// Since we use parent_id in child records, we don't strictly need to
	// return the full Children list here, but we must return a node if it exists.
	// We use the current transaction to see uncommitted writes.

	var kind int
	var mtimeNano int64
	// parent_id can be NULL for root
	err := w.tx.QueryRow("SELECT kind, mtime FROM nodes WHERE id = ?", id).Scan(&kind, &mtimeNano)
	if err == sql.ErrNoRows {
		return nil, graph.ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	mode := os.FileMode(0o444)
	if kind == 1 {
		mode = os.ModeDir | 0o555
	}

	return &graph.Node{
		ID:      id,
		Mode:    mode,
		ModTime: time.Unix(0, mtimeNano),
		// Children: nil -- Engine will append to this and call AddNode,
		// but our AddNode ignores n.Children, so this is safe.
	}, nil
}

func (w *SQLiteWriter) ListChildren(id string) ([]string, error) {
	return nil, nil // Not used during ingest
}

func (w *SQLiteWriter) ReadContent(id string, buf []byte, offset int64) (int, error) {
	return 0, nil // Not used during ingest
}

func (w *SQLiteWriter) GetCallers(token string) ([]*graph.Node, error) {
	return nil, nil // Not used during ingest
}

func (w *SQLiteWriter) GetCallees(id string) ([]*graph.Node, error) {
	return nil, nil // Not used during ingest
}

func (w *SQLiteWriter) Invalidate(id string) {
	// No-op
}

func (w *SQLiteWriter) Act(id, action, payload string) (*graph.ActionResult, error) {
	return nil, graph.ErrActNotSupported
}

// Interface compliance
var _ IngestionTarget = (*SQLiteWriter)(nil)
