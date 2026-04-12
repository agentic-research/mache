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
)

// NodeKindFile and NodeKindDir are the kind values in the nodes table.
const (
	NodeKindFile = 0
	NodeKindDir  = 1
)

// NodesTableReader provides read methods for the nodes-table schema.
// Shared by SQLiteGraph (nodes-table fast path) and WritableGraph.
//
// Parameterized by FileMode/DirMode so the same SQL queries produce
// read-only (0o444/0o555) or writable (0o644/0o755) nodes.
//
// The caller owns the *sql.DB lifecycle — NodesTableReader holds a
// reference but does not close it.
type NodesTableReader struct {
	db        *sql.DB
	tableName string           // source records table ("results" or schema.Table)
	render    TemplateRenderer // for record_id fallback rendering
	levels    []*schemaLevel   // compiled schema levels
	fileMode  os.FileMode      // permission for file nodes
	dirMode   os.FileMode      // permission for dir nodes
	sizeCache sync.Map         // file path → int64
	cache     *ContentCache    // FIFO-bounded rendered content
}

// DB returns the underlying database connection.
// Used by WritableGraph.UpdateRecord for write operations.
func (r *NodesTableReader) DB() *sql.DB { return r.db }

// NewNodesTableReader creates a reader for the nodes-table schema.
func NewNodesTableReader(db *sql.DB, tableName string, render TemplateRenderer,
	levels []*schemaLevel, fileMode, dirMode os.FileMode, cacheSize int,
) *NodesTableReader {
	return &NodesTableReader{
		db:        db,
		tableName: tableName,
		render:    render,
		levels:    levels,
		fileMode:  fileMode,
		dirMode:   dirMode,
		cache:     NewContentCache(cacheSize),
	}
}

// GetNode returns a node by ID from the nodes table.
func (r *NodesTableReader) GetNode(id string) (*Node, error) {
	id = NormalizeID(id)
	if id == "" {
		return &Node{ID: "", Mode: os.ModeDir | r.dirMode}, nil
	}

	var kind, size int
	var mtimeNano int64
	var recordID sql.NullString
	err := r.db.QueryRow("SELECT kind, size, mtime, record_id FROM nodes WHERE id = ?", id).
		Scan(&kind, &size, &mtimeNano, &recordID)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	mode := r.fileMode
	if kind == NodeKindDir {
		mode = os.ModeDir | r.dirMode
	}

	node := &Node{
		ID:      id,
		Mode:    mode,
		ModTime: time.Unix(0, mtimeNano),
	}

	if kind == NodeKindFile {
		if cachedSize, ok := r.sizeCache.Load(id); ok {
			node.Ref = &ContentRef{ContentLen: cachedSize.(int64)}
			return node, nil
		}
		node.Ref = &ContentRef{ContentLen: int64(size)}
		r.sizeCache.Store(id, int64(size))
	}
	return node, nil
}

// ListChildren returns child IDs for a directory from the nodes table.
func (r *NodesTableReader) ListChildren(id string) ([]string, error) {
	id = NormalizeID(id)

	var rows *sql.Rows
	var err error
	if id == "" {
		rows, err = r.db.Query("SELECT id FROM nodes WHERE parent_id = '' OR parent_id IS NULL ORDER BY name")
	} else {
		rows, err = r.db.Query("SELECT id FROM nodes WHERE parent_id = ? ORDER BY name", id)
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
	return children, rows.Err()
}

// ListChildStats returns stat snapshots for all children without rendering content.
func (r *NodesTableReader) ListChildStats(id string) ([]NodeStat, error) {
	id = NormalizeID(id)

	var rows *sql.Rows
	var err error
	if id == "" {
		rows, err = r.db.Query("SELECT id, kind, size, mtime FROM nodes WHERE parent_id = '' OR parent_id IS NULL ORDER BY name")
	} else {
		rows, err = r.db.Query("SELECT id, kind, size, mtime FROM nodes WHERE parent_id = ? ORDER BY name", id)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var stats []NodeStat
	for rows.Next() {
		var childID string
		var kind, size int
		var mtimeNano int64
		if err := rows.Scan(&childID, &kind, &size, &mtimeNano); err != nil {
			return nil, err
		}
		stats = append(stats, NodeStat{
			ID:          childID,
			IsDir:       kind == NodeKindDir,
			ContentSize: int64(size),
			ModTime:     time.Unix(0, mtimeNano),
			HasOrigin:   false,
		})
	}
	return stats, rows.Err()
}

// ReadContent resolves content and copies into buf at offset.
func (r *NodesTableReader) ReadContent(id string, buf []byte, offset int64) (int, error) {
	id = NormalizeID(id)
	content, err := r.resolveContent(id)
	if err != nil {
		return 0, err
	}
	return SliceContent(content, buf, offset), nil
}

// resolveContent reads file content. Checks cache, then nodes.record column
// (inline content), then falls back to template rendering via record_id.
func (r *NodesTableReader) resolveContent(id string) ([]byte, error) {
	if c, ok := r.cache.Get(id); ok {
		return c, nil
	}

	var record sql.NullString
	var recordID sql.NullString
	err := r.db.QueryRow("SELECT record, record_id FROM nodes WHERE id = ?", id).
		Scan(&record, &recordID)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	var content []byte
	if record.Valid && record.String != "" {
		content = []byte(record.String)
	} else if recordID.Valid && recordID.String != "" {
		content, err = r.renderFromRecord(id, recordID.String)
		if err != nil {
			return nil, err
		}
	} else {
		return nil, ErrNotFound
	}

	r.cache.Put(id, content)
	return content, nil
}

// renderFromRecord fetches a record by ID and renders content via template.
func (r *NodesTableReader) renderFromRecord(filePath, recordID string) ([]byte, error) {
	var raw string
	if err := r.db.QueryRow("SELECT record FROM "+r.tableName+" WHERE id = ?", recordID).Scan(&raw); err != nil {
		return nil, fmt.Errorf("fetch record %s: %w", recordID, err)
	}

	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("parse record %s: %w", recordID, err)
	}
	values, _ := parsed.(map[string]any)

	segments := strings.Split(filePath, "/")
	_, fileLeaf := walkSchemaLevels(r.levels, segments)
	if fileLeaf == nil {
		return nil, fmt.Errorf("no schema leaf for %s", filePath)
	}

	rendered, err := r.render(fileLeaf.ContentTemplate, values)
	if err != nil {
		return nil, fmt.Errorf("render %s: %w", filePath, err)
	}
	return []byte(rendered), nil
}

// GetCallers returns nodes that reference the given token via node_refs table.
func (r *NodesTableReader) GetCallers(token string) ([]*Node, error) {
	rows, err := r.db.Query("SELECT node_id FROM node_refs WHERE token = ?", token)
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
			Mode: r.fileMode,
		})
	}
	return nodes, nil
}

// Invalidate evicts cached content and size for a node.
func (r *NodesTableReader) Invalidate(id string) {
	r.sizeCache.Delete(id)
	r.cache.Delete(id)
}
