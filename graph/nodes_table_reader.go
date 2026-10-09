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

// NodeKindFile and NodeKindDir are the integer values stored in the nodes
// table's `kind` column (the wire format produced by mache build /
// leyline parse). They are NOT a substitute for *Node.Mode at runtime —
// Mode uses fs.FileMode and carries the IsDir() predicate. The kind
// constants are used by ingestion writers when populating the column,
// by readers when interpreting it, and as a comment marker next to
// bare-int SQL literals like `kind = 1` (SQL can't reference Go
// constants directly).
//
// These values are part of the on-disk schema and must not change.
//
// Bead mache-e7e9e5.
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
	db         *sql.DB
	tableName  string           // source records table ("results" or schema.Table)
	render     TemplateRenderer // for record_id fallback rendering
	levels     []*schemaLevel   // compiled schema levels
	fileMode   os.FileMode      // permission for file nodes
	dirMode    os.FileMode      // permission for dir nodes
	sizeCache  sync.Map         // file path → int64
	cache      *ContentCache    // FIFO-bounded rendered content
	hasContext bool             // nodes table carries the context column (mache-b8fe72)
	hasProps   bool             // nodes table carries the props column (mache-90b89b)
	hasAST     bool             // db carries ley-line-open's _ast table, so nodes can be located in source (mache-e57065)
	stmts      readerStmts      // compiled once, reused per call (mache-3063fb)
}

// readerStmts are every statement the reader issues, compiled on first use.
//
// All of them are fixed-shape: they depend only on tableName, a constant
// pattern, and the column flags above, every one of which is settled at
// construction. So none needs to be rebuilt per call — which is what the
// reader used to do, compiling the same SQL afresh on every GetNode.
type readerStmts struct {
	node, children, childrenRoot, stats, statsRoot, content, record, callers *preparedStmt
}

func (s readerStmts) all() []*preparedStmt {
	return []*preparedStmt{
		s.node, s.children, s.childrenRoot, s.stats, s.statsRoot, s.content, s.record, s.callers,
	}
}

// ColumnExists reports whether table has a column named col. Readers use it
// to stay compatible with nodes tables written before a column was added
// (e.g. `context`, mache-b8fe72) or produced by leyline, and writers use it
// to decide whether an ALTER is needed.
//
// Deliberately pragma_table_xinfo, NOT pragma_table_info: table_info omits
// GENERATED columns entirely, so it answers "missing" for a column that is
// present, readable, indexed, and returned by SELECT *. ley-line-open already
// ships one (`source_blobs.byte_len` STORED) and projection-v4 makes
// `nodes.parent_id` another (mache-bc6ca3). Under table_info a reader would
// silently downgrade to a compatibility path for a column it can read, and a
// writer would emit an ALTER that fails with "duplicate column name". xinfo
// reports the same rows as info plus generated and hidden ones, so no existing
// probe changes its answer.
func ColumnExists(db *sql.DB, table, col string) bool {
	rows, err := db.Query("SELECT 1 FROM pragma_table_xinfo(?) WHERE name = ?", table, col)
	if err != nil {
		return false
	}
	defer func() { _ = rows.Close() }()
	return rows.Next()
}

// DB returns the underlying database connection.
// Used by WritableGraph for write operations and Close.
func (r *NodesTableReader) DB() *sql.DB { return r.db }

// NewNodesTableReader creates a reader for the nodes-table schema.
func NewNodesTableReader(db *sql.DB, tableName string, render TemplateRenderer,
	levels []*schemaLevel, fileMode, dirMode os.FileMode, cacheSize int,
) *NodesTableReader {
	r := &NodesTableReader{
		db:         db,
		tableName:  tableName,
		render:     render,
		levels:     levels,
		fileMode:   fileMode,
		dirMode:    dirMode,
		cache:      NewContentCache(cacheSize),
		hasContext: ColumnExists(db, "nodes", "context"),
		hasProps:   ColumnExists(db, "nodes", "props"),
		// _ast is ley-line-open's; a standalone mache-built .db has no such
		// table and simply yields nodes without a source location.
		hasAST: ColumnExists(db, "_ast", "start_row"),
	}
	// After the flags: nodeSelect reads them, and the statement text is fixed
	// from here on.
	r.stmts = readerStmts{
		node:         newPreparedStmt(r.nodeSelect()),
		children:     newPreparedStmt("SELECT id FROM nodes WHERE parent_id = ? ORDER BY name"),
		childrenRoot: newPreparedStmt("SELECT id FROM nodes WHERE (parent_id = '' OR parent_id IS NULL) AND id != '' ORDER BY name"),
		stats:        newPreparedStmt("SELECT id, kind, size, mtime FROM nodes WHERE parent_id = ? ORDER BY name"),
		statsRoot:    newPreparedStmt("SELECT id, kind, size, mtime FROM nodes WHERE (parent_id = '' OR parent_id IS NULL) AND id != '' ORDER BY name"),
		content:      newPreparedStmt("SELECT record, record_id FROM nodes WHERE id = ?"),
		record:       newPreparedStmt("SELECT record FROM " + tableName + " WHERE id = ?"),
		callers: newPreparedStmt(
			"SELECT node_id FROM node_refs WHERE token = ? AND node_id NOT LIKE '" + sentinelSQLPattern + "'"),
	}
	return r
}

// Close releases the reader's compiled statements. It does NOT close the
// *sql.DB, which the caller owns; call it before closing the db so the
// statements are released against a live connection.
func (r *NodesTableReader) Close() {
	for _, s := range r.stmts.all() {
		s.close()
	}
}

// GetNode returns a node by ID from the nodes table.
func (r *NodesTableReader) GetNode(id string) (*Node, error) {
	id = NormalizeID(id)
	if id == "" {
		return &Node{ID: "", Mode: os.ModeDir | r.dirMode}, nil
	}

	var row nodeScan
	stmt, err := r.stmts.node.get(r.db)
	if err != nil {
		return nil, err
	}
	err = stmt.QueryRow(id).Scan(row.scanTargets(r)...)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	mode := r.fileMode
	if row.kind == NodeKindDir {
		mode = os.ModeDir | r.dirMode
	}

	node := &Node{
		ID:      id,
		Mode:    mode,
		ModTime: time.Unix(0, row.mtimeNano),
		Context: row.context,
	}

	// Mirrors SQLiteWriter.GetNode: a dir node's `record` is its marshaled
	// Properties. A dir node with inline Data instead won't unmarshal into the
	// map shape — that's expected, and leaves Properties nil.
	node.Properties = DecodeProps(row.props)

	node.Origin = row.loc.origin()

	if row.kind == NodeKindFile {
		if cachedSize, ok := r.sizeCache.Load(id); ok {
			node.Ref = &ContentRef{ContentLen: cachedSize.(int64)}
			return node, nil
		}
		node.Ref = &ContentRef{ContentLen: int64(row.size)}
		r.sizeCache.Store(id, int64(row.size))
	}
	return node, nil
}

// ListChildren returns child IDs for a directory from the nodes table.
//
// Empty-result contract: when the directory has no children, the
// returned slice is nil (not an empty slice). All callers must handle
// nil correctly — len(nil) is 0, range over nil is a no-op, and
// JSON-encoding nil produces null. Don't change to []string{} without
// auditing JSON consumers (the MCP serve_handlers list_directory
// shapes its own non-nil response).
//
// Bead mache-0375fe.
func (r *NodesTableReader) ListChildren(id string) ([]string, error) {
	id = NormalizeID(id)

	var rows *sql.Rows
	var err error
	if id == "" {
		// `id != ''` excludes the root row itself. leyline writes a root
		// node whose id AND parent_id are both empty, so without this the
		// root lists ITSELF as a child — and since ListChildren("") then
		// returns that same "" again, any consumer doing a recursive walk
		// never terminates. Found by examples/publicapi's ladder test.
		rows, err = r.query(r.stmts.childrenRoot)
	} else {
		rows, err = r.query(r.stmts.children, id)
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
		// Same self-child exclusion as ListChildren above; the two must
		// agree or a stat-based walk and an id-based walk see different trees.
		rows, err = r.query(r.stmts.statsRoot)
	} else {
		rows, err = r.query(r.stmts.stats, id)
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
			// Deliberately false even though GetNode now returns a non-nil
			// Origin for the same node (mache-e57065), which makes this the
			// one place NodeStat's "HasOrigin == (Origin != nil)" contract is
			// knowingly under-reported. Two reasons, both stronger than the
			// inconsistency:
			//
			//   1. GraphFS.statToFileInfo maps HasOrigin -> mode 0o644, so
			//      flipping it advertises every leyline-backed file as
			//      WRITABLE over NFS and routes it into the write-back
			//      pipeline (validate -> format -> splice -> ShiftOrigins),
			//      which has never been exercised against _ast-derived
			//      origins. Under-reporting fails closed; over-reporting
			//      fails into an untested write path.
			//   2. Computing it honestly means an _ast lookup PER CHILD, on
			//      the readdir hot path this method exists to keep cheap.
			//
			// Revisit together with write-back support for leyline origins,
			// not before.
			HasOrigin: false,
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
	stmt, err := r.stmts.content.get(r.db)
	if err != nil {
		return nil, err
	}
	err = stmt.QueryRow(id).Scan(&record, &recordID)
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
	stmt, err := r.stmts.record.get(r.db)
	if err != nil {
		return nil, fmt.Errorf("fetch record %s: %w", recordID, err)
	}
	if err := stmt.QueryRow(recordID).Scan(&raw); err != nil {
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
	// Skip the engine's file-level sentinel caller_ids (prefix
	// '_file_level:') — they exist so dead_code's alive CTE can
	// recognise top-level cobra RunE callbacks without polluting
	// the caller view. They aren't real callers.
	rows, err := r.query(r.stmts.callers, token)
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
	return nodes, rows.Err()
}

// Invalidate evicts cached content and size for a node.
func (r *NodesTableReader) Invalidate(id string) {
	r.sizeCache.Delete(id)
	r.cache.Delete(id)
}

// query runs a compiled statement. One helper rather than a get-then-Query pair
// repeated at every call site, so a multi-row read cannot quietly go back to
// handing database/sql a string.
func (r *NodesTableReader) query(p *preparedStmt, args ...any) (*sql.Rows, error) {
	stmt, err := p.get(r.db)
	if err != nil {
		return nil, err
	}
	return stmt.Query(args...)
}
