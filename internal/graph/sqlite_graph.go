package graph

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/RoaringBitmap/roaring"
	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/refsvtab"
	_ "modernc.org/sqlite"
)

// TemplateRenderer renders a Go text/template string with the given values map.
type TemplateRenderer func(tmpl string, values map[string]any) (string, error)

// SQLiteGraph implements Graph by querying the source SQLite database directly.
// No index copy, no ingestion step — the source DB's B+ tree IS the index.
//
// Design: directory structure is derived lazily from schema + DB on first access,
// then cached in sync.Maps for lock-free concurrent reads from FUSE callbacks.
//
// The scan is single-threaded and streaming: one sequential pass over all records,
// rendering name templates to build parent→child path relationships. This avoids
// the deadlock risk and channel overhead of a worker pool — SQLite sequential
// reads are I/O-bound and template rendering for name fields is cheap.
//
// Memory model after scan:
//   - dirChildren: sorted []string slices (one per directory), read-only post-scan
//   - recordIDs: leaf dir path → DB row ID, for on-demand content resolution
//   - contentCache: FIFO-bounded rendered content (avoids re-fetching hot files)
//
// Content is never loaded during scan — only on FUSE read via resolveContent,
// which does a primary key lookup + template render + FIFO cache.
//
// Cross-references (token → file bitmap) are stored in a sidecar database
// (<dbpath>.refs.db) to keep the source DB immutable. Refs are accumulated
// in-memory during ingestion and flushed once via FlushRefs.
type SQLiteGraph struct {
	db     *sql.DB
	dbPath string
	schema *api.Topology
	render TemplateRenderer
	levels []*schemaLevel // compiled schema tree, immutable after construction

	// Sidecar database for cross-reference index (node_refs + file_ids tables).
	// Kept separate from source DB to preserve immutability of Venturi data.
	refsDB *sql.DB
	dbID   string // unique ID for vtab registry

	// Lazy scan: one pass per root node populates dirChildren + recordIDs.
	// sync.Once ensures exactly one scan per root, even under concurrent FUSE access.
	scanOnce sync.Map // root name → *sync.Once
	scanErr  sync.Map // root name → error (sticky: if scan fails, all lookups fail)

	// Directory children — populated by scanRoot, then read-only.
	// Values are sorted []string for O(log n) binary search in isChild.
	dirChildren sync.Map // dir path (string) → []string (sorted child full paths)

	// Record mapping: leaf directory path → results table primary key.
	// Used by resolveContent to fetch the JSON blob on demand.
	recordIDs sync.Map // dir path (string) → string (record ID)

	// In-memory ref accumulator: token → bitmap of file IDs.
	// Populated by AddRef during ingestion, written to refsDB by FlushRefs.
	flushOnce   sync.Once
	pendingMu   sync.Mutex
	pendingRefs map[string]*roaring.Bitmap
	nextFileID  uint32
	fileIDMap   map[string]uint32 // path → file ID (in-memory during ingestion)

	// Size cache: file path → rendered byte length.
	// Populated on first GetNode (which renders content), used on subsequent
	// GetNode calls to return a lightweight Node without re-rendering.
	// Unlike contentCache (FIFO-bounded), sizeCache is unbounded — int64 values
	// are tiny and we need them to survive across large directory traversals.
	sizeCache sync.Map // file path (string) → int64

	// Rendered content cache (FIFO-bounded, protects against hot-file storms)
	contentMu    sync.Mutex
	contentCache map[string][]byte
	contentKeys  []string
	maxContent   int
}

// schemaLevel is a compiled representation of one level in the schema tree.
type schemaLevel struct {
	nameRaw    string
	selector   string
	isStatic   bool
	staticName string
	children   []*schemaLevel
	files      []api.Leaf
	depth      int
}

// EagerScan pre-scans all root nodes so no FUSE callback ever blocks on a scan.
// Call this before mounting — fuse-t's NFS transport times out if a callback takes >2s.
func (g *SQLiteGraph) EagerScan() error {
	for _, l := range g.levels {
		if l.isStatic {
			if err := g.ensureScanned(l.staticName); err != nil {
				return err
			}
		}
	}
	return nil
}

// OpenSQLiteGraph opens a connection to the source DB and compiles the schema.
func OpenSQLiteGraph(dbPath string, schema *api.Topology, render TemplateRenderer) (*SQLiteGraph, error) {
	// Source DB opened read-only — Venturi data is immutable.
	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", dbPath, err)
	}
	db.SetMaxOpenConns(4)

	// Sidecar DB for cross-reference index (token→bitmap, path→fileID).
	// Kept separate so we never write to the source Venturi database.
	refsPath := dbPath + ".refs.db"
	// Wipe stale sidecar — refs are a derived index, rebuilt each run.
	// The in-memory nextFileID counter starts at 0 on every open; if a
	// previous .refs.db exists with IDs 0..N mapped to different paths,
	// INSERT OR IGNORE silently drops the new mappings.
	_ = os.Remove(refsPath) // best-effort cleanup

	// Register the mache_refs vtab module globally before opening refsDB.
	// sql.Open is lazy (no connection until first query), so registering
	// before the first Exec ensures the new connection sees the module.
	refsMod, err := refsvtab.Register()
	if err != nil {
		_ = db.Close() // ignore error
		return nil, err
	}

	refsDB, err := sql.Open("sqlite", refsPath)
	if err != nil {
		_ = db.Close() // ignore error
		return nil, fmt.Errorf("open refs db %s: %w", refsPath, err)
	}
	// Allow 2 connections: one for normal queries, one for vtab Filter callbacks.
	// The mache_refs vtab's xFilter runs inside the SQLite engine on the outer
	// connection; it needs a second connection to query node_refs/file_ids.
	// WAL mode ensures concurrent readers don't conflict.
	refsDB.SetMaxOpenConns(2)

	if _, err := refsDB.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = db.Close()     // ignore error
		_ = refsDB.Close() // ignore error
		return nil, fmt.Errorf("set WAL mode on refs db: %w", err)
	}

	_, err = refsDB.Exec(`
		CREATE TABLE IF NOT EXISTS node_refs (
			token TEXT PRIMARY KEY,
			bitmap BLOB
		);
		CREATE TABLE IF NOT EXISTS file_ids (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			path TEXT UNIQUE NOT NULL
		);
	`)
	if err != nil {
		_ = db.Close()     // ignore error
		_ = refsDB.Close() // ignore error
		return nil, fmt.Errorf("create index tables: %w", err)
	}

	// Point the vtab module at this refsDB and create the virtual table.
	dbID := fmt.Sprintf("sqlite_%d", time.Now().UnixNano())
	refsMod.RegisterDB(dbID, refsDB)

	query := fmt.Sprintf("CREATE VIRTUAL TABLE IF NOT EXISTS mache_refs USING mache_refs(%s)", dbID)
	if _, err := refsDB.Exec(query); err != nil {
		refsMod.UnregisterDB(dbID)
		_ = db.Close()     // ignore error
		_ = refsDB.Close() // ignore error
		return nil, fmt.Errorf("create mache_refs vtab: %w", err)
	}

	return &SQLiteGraph{
		db:           db,
		dbPath:       dbPath,
		schema:       schema,
		render:       render,
		levels:       compileLevels(schema),
		refsDB:       refsDB,
		dbID:         dbID,
		pendingRefs:  make(map[string]*roaring.Bitmap),
		fileIDMap:    make(map[string]uint32),
		contentCache: make(map[string][]byte),
		maxContent:   2048,
	}, nil
}

func compileLevels(schema *api.Topology) []*schemaLevel {
	var out []*schemaLevel
	for _, node := range schema.Nodes {
		out = append(out, compileOneLevel(node, 0))
	}
	return out
}

func compileOneLevel(node api.Node, depth int) *schemaLevel {
	l := &schemaLevel{
		nameRaw:  node.Name,
		selector: node.Selector,
		files:    node.Files,
		depth:    depth,
	}
	if !strings.Contains(node.Name, "{{") {
		l.isStatic = true
		l.staticName = node.Name
	}
	for _, child := range node.Children {
		l.children = append(l.children, compileOneLevel(child, depth+1))
	}
	return l
}

// ---------------------------------------------------------------------------
// Graph interface
// ---------------------------------------------------------------------------

func (g *SQLiteGraph) GetNode(id string) (*Node, error) {
	if len(id) > 0 && id[0] == '/' {
		id = id[1:]
	}
	if id == "" {
		return &Node{ID: "", Mode: os.ModeDir | 0o555}, nil
	}

	segments := strings.Split(id, "/")
	level, fileLeaf := g.walkSchema(segments)
	if level == nil {
		return nil, ErrNotFound
	}

	// File node — return cached size if available (avoids content render for stat).
	// First call renders content and populates both contentCache and sizeCache.
	// Subsequent calls return a lightweight node with ContentRef (no SQL/render).
	if fileLeaf != nil {
		if cachedSize, ok := g.sizeCache.Load(id); ok {
			return &Node{
				ID:   id,
				Mode: 0o444,
				Ref:  &ContentRef{ContentLen: cachedSize.(int64)},
			}, nil
		}
		content, err := g.resolveContent(id, segments, fileLeaf)
		if err != nil {
			return nil, err
		}
		g.sizeCache.Store(id, int64(len(content)))
		return &Node{ID: id, Mode: 0o444, Data: content}, nil
	}

	// Directory node — verify it actually exists in the DB
	rootName := segments[0]
	if err := g.ensureScanned(rootName); err != nil {
		return nil, err
	}

	// Root schema nodes always exist
	if len(segments) == 1 {
		if g.findRootLevel(rootName) != nil {
			return &Node{ID: id, Mode: os.ModeDir | 0o555}, nil
		}
		return nil, ErrNotFound
	}

	// Deeper levels: check if parent lists this path as a child
	parentPath := strings.Join(segments[:len(segments)-1], "/")
	if g.isChild(parentPath, id) {
		return &Node{ID: id, Mode: os.ModeDir | 0o555}, nil
	}
	return nil, ErrNotFound
}

func (g *SQLiteGraph) ListChildren(id string) ([]string, error) {
	if len(id) > 0 && id[0] == '/' {
		id = id[1:]
	}

	// Root: return schema root names
	if id == "" {
		var roots []string
		for _, l := range g.levels {
			if l.isStatic {
				roots = append(roots, l.staticName)
			}
		}
		return roots, nil
	}

	segments := strings.Split(id, "/")
	if err := g.ensureScanned(segments[0]); err != nil {
		return nil, err
	}

	if v, ok := g.dirChildren.Load(id); ok {
		return v.([]string), nil
	}
	return nil, ErrNotFound
}

func (g *SQLiteGraph) ReadContent(id string, buf []byte, offset int64) (int, error) {
	if len(id) > 0 && id[0] == '/' {
		id = id[1:]
	}

	segments := strings.Split(id, "/")
	_, fileLeaf := g.walkSchema(segments)
	if fileLeaf == nil {
		return 0, ErrNotFound
	}

	content, err := g.resolveContent(id, segments, fileLeaf)
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

// AddRef accumulates a reference in-memory. No SQL is issued until FlushRefs.
// This eliminates the read-modify-write cycle per call — all bitmap mutations
// happen in RAM, and FlushRefs writes them in a single transaction.
func (g *SQLiteGraph) AddRef(token, nodeID string) error {
	g.pendingMu.Lock()
	defer g.pendingMu.Unlock()

	fid, ok := g.fileIDMap[nodeID]
	if !ok {
		fid = g.nextFileID
		g.nextFileID++
		g.fileIDMap[nodeID] = fid
	}

	bm, ok := g.pendingRefs[token]
	if !ok {
		bm = roaring.New()
		g.pendingRefs[token] = bm
	}
	bm.Add(fid)
	return nil
}

// FlushRefs writes all accumulated refs to the sidecar database in a single
// transaction. Call once after ingestion is complete. This replaces the old
// per-call AddRef write path, reducing N*M SQL round-trips to exactly
// len(fileIDMap) + len(pendingRefs) inserts in one transaction.
//
// Guarded by sync.Once — safe to call multiple times; only the first call
// performs the flush. This prevents the double-call bug where a second flush
// would reset nextFileID to 0, causing ID collisions in file_ids.
func (g *SQLiteGraph) FlushRefs() error {
	var flushErr error
	g.flushOnce.Do(func() {
		flushErr = g.flushRefsInternal()
	})
	return flushErr
}

func (g *SQLiteGraph) flushRefsInternal() error {
	g.pendingMu.Lock()
	refs := g.pendingRefs
	fileIDs := g.fileIDMap
	g.pendingMu.Unlock()

	if len(refs) == 0 {
		return nil
	}

	tx, err := g.refsDB.Begin()
	if err != nil {
		return fmt.Errorf("begin refs flush: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // safe to ignore

	// Write file_ids
	fileStmt, err := tx.Prepare("INSERT OR IGNORE INTO file_ids (id, path) VALUES (?, ?)")
	if err != nil {
		return fmt.Errorf("prepare file_ids insert: %w", err)
	}
	defer func() { _ = fileStmt.Close() }() // safe to ignore

	for path, id := range fileIDs {
		if _, err := fileStmt.Exec(id, path); err != nil {
			return fmt.Errorf("insert file_id %s: %w", path, err)
		}
	}

	// Write bitmaps
	refStmt, err := tx.Prepare("INSERT OR REPLACE INTO node_refs (token, bitmap) VALUES (?, ?)")
	if err != nil {
		return fmt.Errorf("prepare node_refs insert: %w", err)
	}
	defer func() { _ = refStmt.Close() }() // safe to ignore

	var buf bytes.Buffer
	for token, bm := range refs {
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

// GetCallers returns the list of files (nodes) that reference the given token.
// Reads from the sidecar refs database.
func (g *SQLiteGraph) GetCallers(token string) ([]*Node, error) {
	var blob []byte
	err := g.refsDB.QueryRow("SELECT bitmap FROM node_refs WHERE token = ?", token).Scan(&blob)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	rb := roaring.New()
	if err := rb.UnmarshalBinary(blob); err != nil {
		return nil, fmt.Errorf("unmarshal bitmap: %w", err)
	}

	var fileIDs []uint32
	it := rb.Iterator()
	for it.HasNext() {
		fileIDs = append(fileIDs, it.Next())
	}
	if len(fileIDs) == 0 {
		return nil, nil
	}

	args := make([]any, len(fileIDs))
	placeholders := make([]string, len(fileIDs))
	for i, id := range fileIDs {
		args[i] = id
		placeholders[i] = "?"
	}

	query := fmt.Sprintf("SELECT path FROM file_ids WHERE id IN (%s)", strings.Join(placeholders, ","))
	rows, err := g.refsDB.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query file paths: %w", err)
	}
	defer func() { _ = rows.Close() }() // safe to ignore

	var nodes []*Node
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			log.Printf("GetCallers: skip row scan: %v", err)
			continue
		}
		// Return lightweight node — content resolved on demand by FUSE Read.
		// The Graph interface doesn't require Data to be populated here.
		nodes = append(nodes, &Node{
			ID:   path,
			Mode: 0o444,
		})
	}
	return nodes, nil
}

// Invalidate evicts cached size and content for a node.
// Must be called after write-back modifies a file's content to prevent
// stale size/data from being served on the next Getattr or Read.
func (g *SQLiteGraph) Invalidate(id string) {
	g.sizeCache.Delete(id)
	g.contentMu.Lock()
	delete(g.contentCache, id)
	g.contentMu.Unlock()
}

// QueryRefs executes a SQL query against the refs sidecar database,
// which includes the mache_refs virtual table.
func (g *SQLiteGraph) QueryRefs(query string, args ...any) (*sql.Rows, error) {
	return g.refsDB.Query(query, args...)
}

// Close closes both the source and sidecar database connections.
func (g *SQLiteGraph) Close() error {
	// Unregister from vtab module to prevent leaks/races
	if mod, err := refsvtab.Register(); err == nil && mod != nil {
		mod.UnregisterDB(g.dbID)
	}

	err := g.db.Close()
	if g.refsDB != nil {
		if err2 := g.refsDB.Close(); err == nil {
			err = err2
		}
	}
	return err
}

// ---------------------------------------------------------------------------
// Schema walking
// ---------------------------------------------------------------------------

// walkSchema maps a path to its schema level and (if a file) leaf definition.
// Returns (level, nil) for directories, (level, &leaf) for files, (nil, nil) for invalid paths.
func (g *SQLiteGraph) walkSchema(segments []string) (*schemaLevel, *api.Leaf) {
	if len(segments) == 0 {
		return nil, nil
	}

	var root *schemaLevel
	for _, l := range g.levels {
		if l.isStatic && l.staticName == segments[0] {
			root = l
			break
		}
	}
	if root == nil {
		return nil, nil
	}
	if len(segments) == 1 {
		return root, nil
	}

	current := root
	for i := 1; i < len(segments); i++ {
		seg := segments[i]

		// Check if this segment matches a file at the current level
		for j := range current.files {
			fname := current.files[j].Name
			if !strings.Contains(fname, "{{") && fname == seg {
				return current, &current.files[j]
			}
		}

		// Descend to child level (single child pattern per level)
		if len(current.children) == 0 {
			return nil, nil
		}
		current = current.children[0]
	}

	return current, nil
}

// ---------------------------------------------------------------------------
// Lazy scanning
// ---------------------------------------------------------------------------

func (g *SQLiteGraph) ensureScanned(rootName string) error {
	val, _ := g.scanOnce.LoadOrStore(rootName, &sync.Once{})
	var err error
	val.(*sync.Once).Do(func() {
		err = g.scanRoot(rootName)
		if err != nil {
			g.scanErr.Store(rootName, err)
		}
	})
	if err != nil {
		return err
	}
	if v, ok := g.scanErr.Load(rootName); ok {
		return v.(error)
	}
	return nil
}

// --- Scan types ---

type scanResult struct {
	entries  []pathEntry
	leafDirs []leafMapping
}

type pathEntry struct {
	parent string
	child  string
}

type leafMapping struct {
	dirPath  string
	recordID string
}

// --- Field extraction from name templates ---

// fieldRefRe matches Go template field references like .item.cve.id
var fieldRefRe = regexp.MustCompile(`\.(\w+(?:\.\w+)*)`)

// collectNameTemplates gathers all dynamic name template strings from the schema tree.
func collectNameTemplates(level *schemaLevel) []string {
	var tmpls []string
	var walk func(*schemaLevel)
	walk = func(l *schemaLevel) {
		if !l.isStatic {
			tmpls = append(tmpls, l.nameRaw)
		}
		for _, c := range l.children {
			walk(c)
		}
	}
	walk(level)
	return tmpls
}

// extractFieldPaths pulls dotted field references from Go templates.
// e.g. "{{slice .item.cve.id 4 8}}" → ["item.cve.id"]
func extractFieldPaths(templates []string) []string {
	seen := make(map[string]bool)
	for _, tmpl := range templates {
		for _, m := range fieldRefRe.FindAllStringSubmatch(tmpl, -1) {
			seen[m[1]] = true
		}
	}
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// buildScanQuery builds a SELECT using json_extract for only the fields
// needed by name templates. Avoids transferring and parsing full record JSON.
func buildScanQuery(fieldPaths []string) string {
	cols := make([]string, 0, len(fieldPaths)+1)
	cols = append(cols, "id")
	for _, fp := range fieldPaths {
		cols = append(cols, fmt.Sprintf("json_extract(record, '$.%s')", fp))
	}
	return "SELECT " + strings.Join(cols, ", ") + " FROM results"
}

// setNestedField builds a nested map from a dotted path.
// e.g. setNestedField(m, "item.cve.id", "CVE-2024-0001")
//
//	→ m["item"]["cve"]["id"] = "CVE-2024-0001"
func setNestedField(m map[string]any, dottedPath, value string) {
	parts := strings.Split(dottedPath, ".")
	current := m
	for i, part := range parts {
		if i == len(parts)-1 {
			current[part] = value
		} else {
			if v, ok := current[part]; ok {
				current = v.(map[string]any)
			} else {
				next := make(map[string]any)
				current[part] = next
				current = next
			}
		}
	}
}

// --- Scan implementation ---

// flushBatchSize is the number of records between batch flushes to sync.Map.
// Keeps transient working-map memory bounded for large cross-reference scans.
const flushBatchSize = 50000

// scanRoot performs a single-pass streaming scan of all DB records to build the
// directory tree for one root node. Uses json_extract to push field extraction
// into SQLite, avoiding Go-side JSON parsing of the full record blob.
//
// Safety properties:
//   - Read-only transaction: snapshot isolation prevents partial results if the
//     source DB is being written to concurrently.
//   - Batch flush: every flushBatchSize records, accumulated slices are sorted,
//     deduped, and merged into sync.Map, then the working map is cleared. This
//     bounds transient memory for 10M+ node cross-reference scans.
//   - Error counting: scan/render failures are counted and logged rather than
//     silently swallowed, so data drops are visible.
//
// Why single-threaded: The previous worker-pool implementation (NumCPU goroutines +
// channels) was designed for CPU-bound template rendering, but profiling showed the
// bottleneck is SQLite I/O, not rendering. Name templates are simple field lookups
// (e.g. "{{.item.cve.id}}") that render in <1μs. The channel/goroutine overhead
// actually hurt throughput and introduced deadlock risk. If a future schema uses
// expensive template functions (regex, crypto), re-add parallelism — but measure first.
func (g *SQLiteGraph) scanRoot(rootName string) error {
	level := g.findRootLevel(rootName)
	if level == nil {
		return fmt.Errorf("root %q not found in schema", rootName)
	}

	// Analyze schema to find which fields the name templates need
	fieldPaths := extractFieldPaths(collectNameTemplates(level))
	query := buildScanQuery(fieldPaths)

	// Read-only transaction for snapshot consistency — if the source DB is
	// being written to during scan, we get a consistent point-in-time view.
	tx, err := g.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin scan tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // safe to ignore

	rows, err := tx.Query(query)
	if err != nil {
		return fmt.Errorf("scan query: %w", err)
	}
	defer func() { _ = rows.Close() }() // safe to ignore

	// Accumulate children as slices directly (no intermediate bool sets).
	// Flushed to sync.Map every flushBatchSize records to bound memory.
	childSlices := make(map[string][]string)
	recIDs := make(map[string]string)
	childSlices[rootName] = nil // ensure root exists even if DB is empty

	// Reusable per-row scan buffers — allocated once, reused every iteration
	nCols := len(fieldPaths) + 1
	scanVals := make([]sql.NullString, nCols)
	scanPtrs := make([]any, nCols)
	for i := range scanVals {
		scanPtrs[i] = &scanVals[i]
	}
	fields := make([]string, len(fieldPaths))

	// Reusable result buffer for collectPathEntries
	var result scanResult

	count := 0
	scanErrs := 0
	nullSkips := 0
	for rows.Next() {
		if err := rows.Scan(scanPtrs...); err != nil {
			scanErrs++
			continue
		}

		// Check for NULL fields (records missing required template values)
		skip := false
		for i := range fieldPaths {
			if !scanVals[i+1].Valid {
				skip = true
				break
			}
			fields[i] = scanVals[i+1].String
		}
		if skip {
			nullSkips++
			continue
		}

		// Build minimal values map and render schema path tree
		values := make(map[string]any)
		for i, path := range fieldPaths {
			setNestedField(values, path, fields[i])
		}

		result.entries = result.entries[:0]
		result.leafDirs = result.leafDirs[:0]
		g.collectPathEntries(level, values, rootName, scanVals[0].String, &result)

		for _, e := range result.entries {
			childSlices[e.parent] = append(childSlices[e.parent], e.child)
		}
		for _, l := range result.leafDirs {
			recIDs[l.dirPath] = l.recordID
		}

		count++
		if count%100000 == 0 {
			fmt.Printf("\rScanning %d records...", count)
		}

		// Batch flush: merge accumulated data into sync.Map to bound memory
		if count%flushBatchSize == 0 {
			flushChildSlices(childSlices, &g.dirChildren)
			for path, id := range recIDs {
				g.recordIDs.Store(path, id)
			}
			// Clear working maps but keep root entry
			childSlices = make(map[string][]string)
			childSlices[rootName] = nil
			recIDs = make(map[string]string)
		}
	}
	if count >= 100000 {
		fmt.Printf("\rScanned %d records.\n", count)
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("scan rows: %w", err)
	}

	// Log skipped rows so data drops are visible
	if scanErrs > 0 || nullSkips > 0 {
		log.Printf("scan %q: %d records processed, %d scan errors, %d null-skipped",
			rootName, count, scanErrs, nullSkips)
	}

	// Final flush of remaining data
	flushChildSlices(childSlices, &g.dirChildren)
	for path, id := range recIDs {
		g.recordIDs.Store(path, id)
	}

	return nil
}

// flushChildSlices sorts, deduplicates, and merges accumulated child slices
// into the sync.Map. For keys already in the map (from a previous batch),
// the new children are merged and re-deduped.
func flushChildSlices(slices map[string][]string, target *sync.Map) {
	for parent, children := range slices {
		// Sort and compact this batch
		sort.Strings(children)
		j := 0
		for i, c := range children {
			if i == 0 || c != children[i-1] {
				children[j] = c
				j++
			}
		}
		deduped := children[:j]

		// Merge with any existing entries from prior batches
		if existing, ok := target.Load(parent); ok {
			prev := existing.([]string)
			merged := make([]string, 0, len(prev)+len(deduped))
			merged = append(merged, prev...)
			merged = append(merged, deduped...)
			sort.Strings(merged)
			k := 0
			for i, c := range merged {
				if i == 0 || c != merged[i-1] {
					merged[k] = c
					k++
				}
			}
			deduped = merged[:k]
		}

		target.Store(parent, deduped)
	}
}

// collectPathEntries walks the schema children for one record, producing
// parent→child entries and leaf directory→recordID mappings.
func (g *SQLiteGraph) collectPathEntries(level *schemaLevel, values map[string]any, parentPath, recordID string, result *scanResult) {
	for _, child := range level.children {
		name, err := g.render(child.nameRaw, values)
		if err != nil || name == "" {
			continue
		}

		childPath := parentPath + "/" + name
		result.entries = append(result.entries, pathEntry{parent: parentPath, child: childPath})

		// Recurse into deeper directory levels
		if len(child.children) > 0 {
			g.collectPathEntries(child, values, childPath, recordID, result)
		}

		// Leaf directory: add file children and record mapping
		if len(child.files) > 0 {
			result.leafDirs = append(result.leafDirs, leafMapping{dirPath: childPath, recordID: recordID})
			for _, f := range child.files {
				result.entries = append(result.entries, pathEntry{parent: childPath, child: childPath + "/" + f.Name})
			}
		}
	}
}

func (g *SQLiteGraph) findRootLevel(name string) *schemaLevel {
	for _, l := range g.levels {
		if l.isStatic && l.staticName == name {
			return l
		}
	}
	return nil
}

// isChild checks whether childPath appears in the cached children of parentPath.
func (g *SQLiteGraph) isChild(parentPath, childPath string) bool {
	v, ok := g.dirChildren.Load(parentPath)
	if !ok {
		return false
	}
	// Binary search on sorted children
	children := v.([]string)
	i := sort.SearchStrings(children, childPath)
	return i < len(children) && children[i] == childPath
}

// ---------------------------------------------------------------------------
// Content resolution
// ---------------------------------------------------------------------------

func (g *SQLiteGraph) resolveContent(filePath string, segments []string, leaf *api.Leaf) ([]byte, error) {
	// Check cache
	g.contentMu.Lock()
	if c, ok := g.contentCache[filePath]; ok {
		g.contentMu.Unlock()
		return c, nil
	}
	g.contentMu.Unlock()

	// Find parent directory's record ID
	parentPath := strings.Join(segments[:len(segments)-1], "/")
	if err := g.ensureScanned(segments[0]); err != nil {
		return nil, err
	}

	ridVal, ok := g.recordIDs.Load(parentPath)
	if !ok {
		return nil, ErrNotFound
	}
	recordID := ridVal.(string)

	// Fetch record from source DB (primary key lookup — instant)
	var raw string
	if err := g.db.QueryRow("SELECT record FROM results WHERE id = ?", recordID).Scan(&raw); err != nil {
		return nil, fmt.Errorf("fetch record %s: %w", recordID, err)
	}

	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("parse record %s: %w", recordID, err)
	}
	values, _ := parsed.(map[string]any)

	rendered, err := g.render(leaf.ContentTemplate, values)
	if err != nil {
		return nil, fmt.Errorf("render %s: %w", filePath, err)
	}

	content := []byte(rendered)

	// Cache (FIFO eviction)
	g.contentMu.Lock()
	if len(g.contentCache) >= g.maxContent {
		evict := g.contentKeys[0]
		g.contentKeys = g.contentKeys[1:]
		delete(g.contentCache, evict)
	}
	g.contentCache[filePath] = content
	g.contentKeys = append(g.contentKeys, filePath)
	g.contentMu.Unlock()

	return content, nil
}

// Verify interface compliance at compile time.
var _ Graph = (*SQLiteGraph)(nil)

// Verify fs.FileMode usage (directories need ModeDir bit set).
var _ fs.FileMode = os.ModeDir
