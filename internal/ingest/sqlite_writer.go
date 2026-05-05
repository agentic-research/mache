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
	db           *sql.DB
	tx           *sql.Tx
	stmtNode     *sql.Stmt
	stmtRef      *sql.Stmt // For adding refs
	stmtDef      *sql.Stmt // For adding defs
	stmtFile     *sql.Stmt // For recording file metadata (incremental index)
	stmtCoverage *sql.Stmt // For recording per-source _index_coverage rows
	batchSize    int
	count        int
	firstErr     error // first insert/batch error, surfaced by Close
	mu           sync.Mutex
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
		node_id TEXT,
		PRIMARY KEY (token, node_id)
	) WITHOUT ROWID;

	CREATE TABLE IF NOT EXISTS file_index (
		path TEXT PRIMARY KEY,
		mod_time INTEGER NOT NULL,
		size INTEGER NOT NULL
	);

	-- _index_coverage records which producer indexed each source file at
	-- which fidelity level, per ADR-0013. Consumers that need to
	-- distinguish "no binding row exists" from "no binding row was looked
	-- for" join against this. PRIMARY KEY (source_id, producer) means a
	-- re-index replaces the prior coverage row for that producer.
	--
	-- fidelity values: 'mention' (tree-sitter), 'binding' (LSP),
	-- 'reachability' (future SSA / call-graph).
	-- complete: 1 if the indexer claims full coverage of the source,
	-- 0 if it gave up partway (out-of-memory, timeout, parse errors
	-- prevented analysis, etc.).
	CREATE TABLE IF NOT EXISTS _index_coverage (
		source_id TEXT NOT NULL,
		producer TEXT NOT NULL,
		fidelity TEXT NOT NULL,
		indexed_at INTEGER NOT NULL,
		complete INTEGER NOT NULL,
		PRIMARY KEY (source_id, producer)
	) WITHOUT ROWID;

	-- v_defs / v_refs: canonical views per ADR-0013 Step 3. Consumers
	-- query these instead of node_defs / node_refs directly so they're
	-- producer-agnostic — when LSP-resolved rows land (Step 1, sister
	-- bead ley-line-453f7e), the view definition expands with a
	-- UNION ALL and consumer SQL doesn't change.
	--
	-- Today the views surface mention-fidelity rows only; Step 1 adds
	-- referrer_node_id + ref_token columns to _lsp_refs so the binding
	-- rows can be unioned in trivially. The fidelity column is the
	-- forward-looking marker — currently always 'mention' from these
	-- producers.
	CREATE VIEW IF NOT EXISTS v_defs AS
		SELECT token, node_id, 'mention' AS fidelity FROM node_defs;

	CREATE VIEW IF NOT EXISTS v_refs AS
		SELECT node_id AS referrer_node_id,
		       token,
		       NULL  AS target_node_id,
		       NULL  AS ref_uri,
		       NULL  AS ref_line,
		       'mention' AS fidelity
		FROM node_refs;
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

	w.stmtDef, err = w.tx.Prepare(`INSERT OR IGNORE INTO node_defs (token, node_id) VALUES (?, ?)`)
	if err != nil {
		return err
	}

	w.stmtFile, err = w.tx.Prepare(`INSERT OR REPLACE INTO file_index (path, mod_time, size) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}

	w.stmtCoverage, err = w.tx.Prepare(
		`INSERT OR REPLACE INTO _index_coverage (source_id, producer, fidelity, indexed_at, complete)
		 VALUES (?, ?, ?, ?, ?)`,
	)
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
	if w.stmtCoverage != nil {
		_ = w.stmtCoverage.Close()
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

// RecordIndexCoverage stores a coverage row for a (source_id, producer)
// pair, per ADR-0013 Step 2. The fidelity argument is one of "mention",
// "binding", or "reachability"; complete=true means the indexer claims
// full coverage of source_id at that fidelity. A re-index replaces the
// prior row for the same (source_id, producer) — different producers
// keep distinct rows.
func (w *SQLiteWriter) RecordIndexCoverage(sourceID, producer, fidelity string, indexedAt time.Time, complete bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	completeInt := 0
	if complete {
		completeInt = 1
	}
	_, err := w.stmtCoverage.Exec(sourceID, producer, fidelity, indexedAt.UnixNano(), completeInt)
	if err != nil {
		log.Printf("SQLiteWriter: record coverage failed for %s/%s: %v", sourceID, producer, err)
		if w.firstErr == nil {
			w.firstErr = fmt.Errorf("record coverage %s/%s: %w", sourceID, producer, err)
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

// CanonicalViewsDDL is the SQL that creates v_defs / v_refs.
// Exposed so consumers that opened a .db from a different writer
// (e.g. LLO-built .db without these views, or an older mache build)
// can install the views on demand. NewSQLiteWriter runs the same
// DDL inline — repeated execution is safe via CREATE VIEW IF NOT
// EXISTS.
//
// Once Step 1 (sister bead ley-line-453f7e) ships and _lsp_refs /
// _lsp_defs gain the referrer_node_id / ref_token / def_token
// columns, the view bodies extend with UNION ALL clauses pulling
// the binding-fidelity rows. Consumer SQL doesn't change at that
// point.
const CanonicalViewsDDL = `
CREATE VIEW IF NOT EXISTS v_defs AS
	SELECT token, node_id, 'mention' AS fidelity FROM node_defs;

CREATE VIEW IF NOT EXISTS v_refs AS
	SELECT node_id AS referrer_node_id,
	       token,
	       NULL  AS target_node_id,
	       NULL  AS ref_uri,
	       NULL  AS ref_line,
	       'mention' AS fidelity
	FROM node_refs;
`

// EnsureCanonicalViews installs v_defs / v_refs on an existing
// database connection. Idempotent — uses CREATE VIEW IF NOT EXISTS.
// Useful for callers that opened a .db produced by something other
// than mache's writer (e.g. an LLO build that hasn't been migrated
// yet, or a pre-Step-3 mache .db on disk).
func EnsureCanonicalViews(db *sql.DB) error {
	if _, err := db.Exec(CanonicalViewsDDL); err != nil {
		return fmt.Errorf("ensure canonical views: %w", err)
	}
	return nil
}

// CoverageEntry is one (source_id, producer) row from _index_coverage.
// A consumer that needs to distinguish "no binding row exists for this
// source" from "no producer at the binding level ever indexed this
// source" joins against this — see ADR-0013 wedge case 1.
type CoverageEntry struct {
	Producer  string    // 'tree-sitter' | 'lsp' | future
	Fidelity  string    // 'mention' | 'binding' | 'reachability'
	IndexedAt time.Time // when the indexer wrote the row
	Complete  bool      // indexer claims full coverage of source_id at this fidelity
}

// LoadIndexCoverage reads the _index_coverage table from an existing
// index database. Returns a map keyed by source_id whose values are the
// per-producer entries that touched that source. Returns (nil, nil)
// when the table doesn't exist — the caller treats that as "no
// coverage info; assume any absence is unknown."
func LoadIndexCoverage(dbPath string) (map[string][]CoverageEntry, error) {
	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	var tableName string
	err = db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='table' AND name='_index_coverage'",
	).Scan(&tableName)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(
		"SELECT source_id, producer, fidelity, indexed_at, complete FROM _index_coverage",
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string][]CoverageEntry)
	for rows.Next() {
		var sourceID, producer, fidelity string
		var indexedAtNano int64
		var completeInt int
		if err := rows.Scan(&sourceID, &producer, &fidelity, &indexedAtNano, &completeInt); err != nil {
			return nil, err
		}
		out[sourceID] = append(out[sourceID], CoverageEntry{
			Producer:  producer,
			Fidelity:  fidelity,
			IndexedAt: time.Unix(0, indexedAtNano),
			Complete:  completeInt != 0,
		})
	}
	return out, rows.Err()
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

	// Store full os.FileMode for fidelity. kind column uses the wire
	// constants NodeKindFile/NodeKindDir.
	kind := graph.NodeKindFile
	if n.Mode.IsDir() {
		kind = graph.NodeKindDir
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

func (w *SQLiteWriter) AddFileChildren(parent *graph.Node, files []*graph.Node) {
	for _, f := range files {
		w.AddNode(f)
	}
	// Update parent (SQLiteWriter stores parent_id in child rows, so
	// the parent's Children field is not persisted — just re-store parent).
	w.AddNode(parent)
}

func (w *SQLiteWriter) DeleteFileNodes(filePath string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Delete refs and defs for nodes originating from this file,
	// then delete the nodes themselves using the indexed source_file column.
	_, _ = w.tx.Exec(`DELETE FROM node_refs WHERE node_id IN (
		SELECT id FROM nodes WHERE source_file = ?
	)`, filePath)
	_, _ = w.tx.Exec(`DELETE FROM node_defs WHERE node_id IN (
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
	if kind == graph.NodeKindDir {
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

func (w *SQLiteWriter) ListChildStats(id string) ([]graph.NodeStat, error) {
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
