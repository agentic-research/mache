package graph

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/agentic-research/mache/api"
	_ "modernc.org/sqlite"
)

// WritableGraph is a read-write SQLite backend for arena-mode mounts.
// It opens the master .db in read-write mode and supports mutations
// that are flushed back to the arena via ArenaFlusher.
//
// Read methods use the same nodes-table queries as SQLiteGraph's fast path.
// Write methods mutate the nodes table directly (UPDATE/INSERT/DELETE).
// Flush serializes the entire .db back to the double-buffered arena.
type WritableGraph struct {
	db        *sql.DB
	dbPath    string // temp file path (the writable master)
	schema    *api.Topology
	render    TemplateRenderer
	levels    []*schemaLevel
	flusher   *ArenaFlusher
	mu        sync.RWMutex
	tableName string

	// Content cache for reads (same pattern as SQLiteGraph)
	contentMu    sync.Mutex
	contentCache map[string][]byte
	contentKeys  []string
	maxContent   int

	sizeCache sync.Map
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
	// No WAL files to worry about — os.ReadFile returns valid SQLite bytes.
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
		db:           db,
		dbPath:       masterDBPath,
		schema:       schema,
		render:       render,
		levels:       compileLevels(schema),
		flusher:      flusher,
		tableName:    tableName,
		contentCache: make(map[string][]byte),
		maxContent:   2048,
	}, nil
}

// ---------------------------------------------------------------------------
// Graph interface (read methods)
// ---------------------------------------------------------------------------

func (g *WritableGraph) GetNode(id string) (*Node, error) {
	if len(id) > 0 && id[0] == '/' {
		id = id[1:]
	}
	if id == "" {
		return &Node{ID: "", Mode: os.ModeDir | 0o555}, nil
	}

	var kind, size int
	var mtimeNano int64
	var recordID sql.NullString
	err := g.db.QueryRow("SELECT kind, size, mtime, record_id FROM nodes WHERE id = ?", id).Scan(&kind, &size, &mtimeNano, &recordID)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	mode := os.FileMode(0o644) // writable files
	if kind == 1 {
		mode = os.ModeDir | 0o755
	}

	node := &Node{
		ID:      id,
		Mode:    mode,
		ModTime: time.Unix(0, mtimeNano),
	}

	if kind == 0 {
		// File node — set up content ref for lazy resolution
		if cachedSize, ok := g.sizeCache.Load(id); ok {
			node.Ref = &ContentRef{ContentLen: cachedSize.(int64)}
			return node, nil
		}
		node.Ref = &ContentRef{ContentLen: int64(size)}
		g.sizeCache.Store(id, int64(size))
	}
	return node, nil
}

func (g *WritableGraph) ListChildren(id string) ([]string, error) {
	if len(id) > 0 && id[0] == '/' {
		id = id[1:]
	}

	var rows *sql.Rows
	var err error
	if id == "" {
		rows, err = g.db.Query("SELECT name FROM nodes WHERE parent_id = '' OR parent_id IS NULL ORDER BY name")
	} else {
		rows, err = g.db.Query("SELECT name FROM nodes WHERE parent_id = ? ORDER BY name", id)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var children []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		children = append(children, name)
	}
	return children, nil
}

func (g *WritableGraph) ReadContent(id string, buf []byte, offset int64) (int, error) {
	if len(id) > 0 && id[0] == '/' {
		id = id[1:]
	}

	content, err := g.resolveContent(id)
	if err != nil {
		return 0, err
	}

	if offset >= int64(len(content)) {
		return 0, nil
	}
	end := offset + int64(len(buf))
	if end > int64(len(content)) {
		end = int64(len(content))
	}
	return copy(buf, content[offset:end]), nil
}

// resolveContent reads file content. Checks the nodes.record column first
// (populated by mache build or UpdateRecord), then falls back to template
// rendering from the source record via record_id.
func (g *WritableGraph) resolveContent(id string) ([]byte, error) {
	// Check cache
	g.contentMu.Lock()
	if c, ok := g.contentCache[id]; ok {
		g.contentMu.Unlock()
		return c, nil
	}
	g.contentMu.Unlock()

	// Try reading directly from the record column (works for inline content
	// written by mache build, or content updated via UpdateRecord)
	var record sql.NullString
	var recordID sql.NullString
	err := g.db.QueryRow("SELECT record, record_id FROM nodes WHERE id = ?", id).Scan(&record, &recordID)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	var content []byte
	if record.Valid && record.String != "" {
		// Direct content in record column
		content = []byte(record.String)
	} else if recordID.Valid && recordID.String != "" {
		// Fall back to template rendering from source record
		content, err = g.renderFromRecord(id, recordID.String)
		if err != nil {
			return nil, err
		}
	} else {
		return nil, ErrNotFound
	}

	// Cache (FIFO eviction)
	g.contentMu.Lock()
	if len(g.contentCache) >= g.maxContent {
		evict := g.contentKeys[0]
		g.contentKeys = g.contentKeys[1:]
		delete(g.contentCache, evict)
	}
	g.contentCache[id] = content
	g.contentKeys = append(g.contentKeys, id)
	g.contentMu.Unlock()

	return content, nil
}

// renderFromRecord fetches a record by ID and renders content via template.
func (g *WritableGraph) renderFromRecord(filePath, recordID string) ([]byte, error) {
	var raw string
	if err := g.db.QueryRow("SELECT record FROM "+g.tableName+" WHERE id = ?", recordID).Scan(&raw); err != nil {
		return nil, fmt.Errorf("fetch record %s: %w", recordID, err)
	}

	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("parse record %s: %w", recordID, err)
	}
	values, _ := parsed.(map[string]any)

	segments := strings.Split(filePath, "/")
	_, fileLeaf := walkSchemaLevels(g.levels, segments)
	if fileLeaf == nil {
		return nil, fmt.Errorf("no schema leaf for %s", filePath)
	}

	rendered, err := g.render(fileLeaf.ContentTemplate, values)
	if err != nil {
		return nil, fmt.Errorf("render %s: %w", filePath, err)
	}
	return []byte(rendered), nil
}

func (g *WritableGraph) GetCallers(token string) ([]*Node, error) {
	rows, err := g.db.Query("SELECT node_id FROM node_refs WHERE token = ?", token)
	if err != nil {
		return nil, fmt.Errorf("query node_refs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var nodes []*Node
	for rows.Next() {
		var nodeID string
		if err := rows.Scan(&nodeID); err != nil {
			log.Printf("GetCallers: skip row scan: %v", err)
			continue
		}
		nodes = append(nodes, &Node{
			ID:   nodeID,
			Mode: 0o644,
		})
	}
	return nodes, nil
}

func (g *WritableGraph) Invalidate(id string) {
	g.sizeCache.Delete(id)
	g.contentMu.Lock()
	delete(g.contentCache, id)
	g.contentMu.Unlock()
}

// ---------------------------------------------------------------------------
// Write methods
// ---------------------------------------------------------------------------

// UpdateRecord updates a file node's content in the nodes table.
// The content is stored as an opaque blob in the record column.
func (g *WritableGraph) UpdateRecord(id string, content []byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if len(id) > 0 && id[0] == '/' {
		id = id[1:]
	}

	now := time.Now().UnixNano()
	result, err := g.db.Exec(
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

	// Evict caches
	g.Invalidate(id)
	return nil
}

// Flush requests a coalesced arena flush. Non-blocking — the actual I/O
// happens on the flusher's background tick. Use FlushNow for synchronous.
func (g *WritableGraph) Flush() {
	if g.flusher != nil {
		g.flusher.RequestFlush()
	}
}

// FlushNow performs a synchronous arena flush. Use on unmount.
func (g *WritableGraph) FlushNow() error {
	if g.flusher == nil {
		return nil
	}
	return g.flusher.FlushNow()
}

// Close closes the database connection.
func (g *WritableGraph) Close() error {
	return g.db.Close()
}

// DBPath returns the path to the writable master database.
func (g *WritableGraph) DBPath() string {
	return g.dbPath
}

// Verify interface compliance at compile time.
var _ Graph = (*WritableGraph)(nil)
