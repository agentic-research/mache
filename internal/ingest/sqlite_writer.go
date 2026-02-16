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

// SQLiteWriter implements IngestionTarget for the new high-performance schema.
type SQLiteWriter struct {
	db        *sql.DB
	tx        *sql.Tx
	stmtNode  *sql.Stmt
	stmtRef   *sql.Stmt // For adding refs
	batchSize int
	count     int
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
		record JSON
	);
	CREATE INDEX IF NOT EXISTS idx_parent_name ON nodes(parent_id, name);

	CREATE TABLE IF NOT EXISTS node_refs (
		token TEXT,
		node_id TEXT,
		PRIMARY KEY (token, node_id)
	) WITHOUT ROWID;
	`
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	w := &SQLiteWriter{
		db:        db,
		batchSize: 10000,
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
		INSERT OR REPLACE INTO nodes (id, parent_id, name, kind, size, mtime, record_id, record)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}

	w.stmtRef, err = w.tx.Prepare(`INSERT OR IGNORE INTO node_refs (token, node_id) VALUES (?, ?)`)
	return err
}

func (w *SQLiteWriter) commitTx() error {
	if w.stmtNode != nil {
		_ = w.stmtNode.Close()
	}
	if w.stmtRef != nil {
		_ = w.stmtRef.Close()
	}
	if err := w.tx.Commit(); err != nil {
		return err
	}
	return nil
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

	// 2. Kind (0=File, 1=Dir for simple boolean, or use os.FileMode bits)
	// We'll use 1 for Dir, 0 for File to match simple boolean logic,
	// or we can store the full Mode. Let's store full Mode for fidelity?
	// The requirement said "kind (file vs directory type)".
	// Storing Mode is safer.
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

	// 4. Serialize Record (properties)
	var record []byte
	if len(n.Properties) > 0 {
		record, _ = json.Marshal(n.Properties)
	}

	// 5. Insert
	_, err := w.stmtNode.Exec(
		n.ID,
		parentID,
		name,
		kind,
		n.ContentSize(),
		n.ModTime.UnixNano(),
		recordID,
		record,
	)
	if err != nil {
		log.Printf("SQLiteWriter: insert failed for %s: %v", n.ID, err)
	}

	w.count++
	if w.count >= w.batchSize {
		if err := w.commitTx(); err != nil {
			log.Printf("SQLiteWriter: commit failed: %v", err)
		}
		if err := w.beginTx(); err != nil {
			log.Printf("SQLiteWriter: begin failed: %v", err)
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

func (w *SQLiteWriter) DeleteFileNodes(filePath string) {
	// Not implemented for batch writer (usually used for fresh ingest)
}

func (w *SQLiteWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.commitTx(); err != nil {
		_ = w.db.Close()
		return err
	}

	// Create indices after bulk load for speed
	if _, err := w.db.Exec(`CREATE INDEX IF NOT EXISTS idx_parent_name ON nodes(parent_id, name)`); err != nil {
		log.Printf("SQLiteWriter: index creation failed: %v", err)
	}

	return w.db.Close()
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

func (w *SQLiteWriter) Invalidate(id string) {
	// No-op
}

// Interface compliance
var _ IngestionTarget = (*SQLiteWriter)(nil)
