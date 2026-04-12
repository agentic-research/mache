package graph

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/agentic-research/mache/api"
	_ "modernc.org/sqlite"
)

// WritableGraph is a read-write SQLite backend for arena-mode mounts.
// Read methods delegate to NodesTableReader (shared with SQLiteGraph).
// Write methods mutate the nodes table and flush to the arena.
type WritableGraph struct {
	ntr     *NodesTableReader // all read operations
	dbPath  string            // temp file path (the writable master)
	schema  *api.Topology
	flusher *ArenaFlusher
	mu      sync.RWMutex
}

// OpenWritableGraph opens a writable connection to the master .db.
// The DB must have a nodes table (created by mache build).
func OpenWritableGraph(masterDBPath string, schema *api.Topology, render TemplateRenderer, flusher *ArenaFlusher) (*WritableGraph, error) {
	db, err := sql.Open("sqlite", masterDBPath)
	if err != nil {
		return nil, fmt.Errorf("open writable db %s: %w", masterDBPath, err)
	}
	db.SetMaxOpenConns(2)

	// journal_mode=DELETE: after commit, the .db file IS the serialized form.
	if _, err := db.Exec("PRAGMA journal_mode=DELETE"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set journal mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA synchronous=NORMAL"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set synchronous: %w", err)
	}

	// Verify nodes table exists
	var count int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='nodes'").Scan(&count); err != nil || count == 0 {
		_ = db.Close()
		return nil, fmt.Errorf("nodes table not found in %s", masterDBPath)
	}

	tableName := schema.Table
	if tableName == "" {
		tableName = "results"
	}

	return &WritableGraph{
		ntr:     NewNodesTableReader(db, tableName, render, compileLevels(schema), 0o644, 0o755, 2048),
		dbPath:  masterDBPath,
		schema:  schema,
		flusher: flusher,
	}, nil
}

// ---------------------------------------------------------------------------
// Read methods — delegate to NodesTableReader
// ---------------------------------------------------------------------------

func (g *WritableGraph) GetNode(id string) (*Node, error)         { return g.ntr.GetNode(id) }
func (g *WritableGraph) ListChildren(id string) ([]string, error) { return g.ntr.ListChildren(id) }
func (g *WritableGraph) ListChildStats(id string) ([]NodeStat, error) {
	return g.ntr.ListChildStats(id)
}

func (g *WritableGraph) ReadContent(id string, buf []byte, offset int64) (int, error) {
	return g.ntr.ReadContent(id, buf, offset)
}
func (g *WritableGraph) GetCallers(token string) ([]*Node, error) { return g.ntr.GetCallers(token) }
func (g *WritableGraph) GetCallees(id string) ([]*Node, error)    { return nil, nil }
func (g *WritableGraph) Invalidate(id string)                     { g.ntr.Invalidate(id) }
func (g *WritableGraph) Act(id, action, payload string) (*ActionResult, error) {
	return nil, ErrActNotSupported
}

// ---------------------------------------------------------------------------
// Write methods
// ---------------------------------------------------------------------------

// UpdateRecord updates a file node's content in the nodes table.
func (g *WritableGraph) UpdateRecord(id string, content []byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	id = NormalizeID(id)

	now := time.Now().UnixNano()
	result, err := g.ntr.DB().Exec(
		"UPDATE nodes SET record = ?, size = ?, mtime = ? WHERE id = ?",
		string(content), len(content), now, id,
	)
	if err != nil {
		return fmt.Errorf("update record %s: %w", id, err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}

	g.ntr.Invalidate(id)
	return nil
}

// Flush requests a coalesced arena flush. Non-blocking.
func (g *WritableGraph) Flush() {
	if g.flusher != nil {
		g.flusher.RequestFlush()
	}
}

// FlushNow performs a synchronous arena flush.
func (g *WritableGraph) FlushNow() error {
	if g.flusher == nil {
		return nil
	}
	return g.flusher.FlushNow()
}

// Close closes the database connection.
func (g *WritableGraph) Close() error {
	return g.ntr.DB().Close()
}

// DBPath returns the path to the writable master database.
func (g *WritableGraph) DBPath() string {
	return g.dbPath
}

// Verify interface compliance at compile time.
var _ Graph = (*WritableGraph)(nil)
