package graph

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
	db        *sql.DB
	dbPath    string
	tableName string // source table name (default: "results")
	schema    *api.Topology
	render    TemplateRenderer
	levels    []*schemaLevel // compiled schema tree, immutable after construction

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
	// Used ONLY by the legacy scan path (useNodesTable = false). The
	// nodes-table fast path stores sizes inside its NodesTableReader
	// (ntr.SizeCache) and never writes here. Invalidate still calls
	// .Delete on this map for both paths — it's a no-op on the fast
	// path because nothing was ever stored — but the spurious call is
	// cheap and keeps Invalidate uniform across both backends.
	// See bead mache-033cc9.
	sizeCache sync.Map // file path (string) → int64

	// FIFO-bounded rendered content cache. Same legacy-only story as
	// sizeCache above: nil on the nodes-table fast path (initialized
	// only by the legacy OpenSQLiteGraph branch), and Invalidate's
	// nil check guards the delete.
	cache *ContentCache

	// Nodes-table fast path: when non-nil, all read methods delegate here.
	// Initialized only when the DB has a "nodes" table (built by mache build).
	ntr           *NodesTableReader
	useNodesTable bool

	extractor CallExtractor
	defs      map[string][]string // symbol_name → []construct_dir_id (populated by AddDef)

	// scopedExtractor resolves calls for a single construct directly against
	// a pre-parsed `_ast` table, keyed by the construct's real ast_source_id/
	// ast_scope_id Properties rather than the graph node id. GetCallees
	// prefers this when both are available; see ScopedCallExtractor
	// (bead mache-fd9982).
	scopedExtractor ScopedCallExtractor
}

// EagerScan pre-scans all root nodes so no FUSE callback ever blocks on a scan.
// Call this before mounting — fuse-t's NFS transport times out if a callback takes >2s.
func (g *SQLiteGraph) EagerScan() error {
	if g.useNodesTable {
		return nil // No scan needed for indexed table // coverage:ignore
	} // coverage:ignore
	for _, l := range g.levels {
		if l.isStatic {
			if err := g.ensureScanned(l.staticName); err != nil {
				return err // coverage:ignore
			} // coverage:ignore
		}
	}
	return nil
}

// OpenSQLiteGraph opens a connection to the source DB and compiles the schema.
func OpenSQLiteGraph(dbPath string, schema *api.Topology, render TemplateRenderer) (*SQLiteGraph, error) {
	// Source DB opened read-only — source data is immutable.
	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", dbPath, err) // coverage:ignore
	} // coverage:ignore
	db.SetMaxOpenConns(4)

	// Check if "nodes" table exists (Fast Path)
	var count int
	useNodesTable := false
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='nodes'").Scan(&count); err == nil && count > 0 {
		useNodesTable = true
	}

	tableName := schema.Table
	if tableName == "" {
		tableName = "results"
	}

	// When the main DB has a nodes table (built by mache build), node_refs
	// is already present with (token, node_id) pairs. No sidecar needed.
	if useNodesTable {
		levels := compileLevels(schema)
		ntr := NewNodesTableReader(db, tableName, render, levels, 0o444, 0o555, 2048)
		return &SQLiteGraph{
			db:            db,
			dbPath:        dbPath,
			tableName:     tableName,
			schema:        schema,
			render:        render,
			levels:        levels,
			ntr:           ntr,
			useNodesTable: true,
		}, nil
	}

	// Legacy path: sidecar DB for cross-reference index (token→bitmap, path→fileID).
	// Kept separate so we never write to the source database.
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
		_ = db.Close()  // ignore error // coverage:ignore
		return nil, err // coverage:ignore
	} // coverage:ignore

	refsDB, err := sql.Open("sqlite", refsPath)
	if err != nil {
		_ = db.Close()                                               // ignore error // coverage:ignore
		return nil, fmt.Errorf("open refs db %s: %w", refsPath, err) // coverage:ignore
	} // coverage:ignore
	// Allow 2 connections: one for normal queries, one for vtab Filter callbacks.
	// The mache_refs vtab's xFilter runs inside the SQLite engine on the outer
	// connection; it needs a second connection to query node_refs/file_ids.
	// WAL mode ensures concurrent readers don't conflict.
	refsDB.SetMaxOpenConns(2)

	if _, err := refsDB.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = db.Close()                                             // ignore error // coverage:ignore
		_ = refsDB.Close()                                         // ignore error // coverage:ignore
		return nil, fmt.Errorf("set WAL mode on refs db: %w", err) // coverage:ignore
	} // coverage:ignore

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
		_ = db.Close()                                         // ignore error // coverage:ignore
		_ = refsDB.Close()                                     // ignore error // coverage:ignore
		return nil, fmt.Errorf("create index tables: %w", err) // coverage:ignore
	} // coverage:ignore

	// Point the vtab module at this refsDB and create the virtual table.
	dbID := fmt.Sprintf("sqlite_%d", time.Now().UnixNano())
	refsMod.RegisterDB(dbID, refsDB)

	query := fmt.Sprintf("CREATE VIRTUAL TABLE IF NOT EXISTS mache_refs USING mache_refs(%s)", dbID)
	if _, err := refsDB.Exec(query); err != nil {
		refsMod.UnregisterDB(dbID)                                // coverage:ignore
		_ = db.Close()                                            // ignore error // coverage:ignore
		_ = refsDB.Close()                                        // ignore error // coverage:ignore
		return nil, fmt.Errorf("create mache_refs vtab: %w", err) // coverage:ignore
	} // coverage:ignore

	return &SQLiteGraph{
		db:            db,
		dbPath:        dbPath,
		tableName:     tableName,
		schema:        schema,
		render:        render,
		levels:        compileLevels(schema),
		refsDB:        refsDB,
		dbID:          dbID,
		pendingRefs:   make(map[string]*roaring.Bitmap),
		fileIDMap:     make(map[string]uint32),
		cache:         NewContentCache(2048),
		useNodesTable: false,
	}, nil
}

// ---------------------------------------------------------------------------
// Graph interface
// ---------------------------------------------------------------------------

// SetCallExtractor configures the parser for on-demand callee resolution.
func (g *SQLiteGraph) SetCallExtractor(fn CallExtractor) {
	g.extractor = fn
}

// SetScopedCallExtractor configures the AST-scoped extractor GetCallees
// prefers when a construct carries ast_source_id/ast_scope_id Properties.
func (g *SQLiteGraph) SetScopedCallExtractor(fn ScopedCallExtractor) {
	g.scopedExtractor = fn
}

// DBPath returns the source .db file path. Implements the cmd-side
// dbPathProvider opt-in (see cmd/serve_find_smells.go) so canonical-
// view setup can locate the sibling .bindings.capnp event log next
// to this .db (mache-190508 step 3).
func (g *SQLiteGraph) DBPath() string { // coverage:ignore
	return g.dbPath // coverage:ignore
} // coverage:ignore

func (g *SQLiteGraph) GetNode(id string) (*Node, error) {
	if g.useNodesTable {
		return g.ntr.GetNode(id)
	}

	id = NormalizeID(id)
	if id == "" {
		return &Node{ID: "", Mode: os.ModeDir | 0o555}, nil // coverage:ignore
	} // coverage:ignore

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
			return nil, err // coverage:ignore
		} // coverage:ignore
		g.sizeCache.Store(id, int64(len(content)))
		return &Node{ID: id, Mode: 0o444, Data: content}, nil
	}

	// Directory node — verify it actually exists in the DB
	rootName := segments[0]
	if err := g.ensureScanned(rootName); err != nil {
		return nil, err // coverage:ignore
	} // coverage:ignore

	// Root schema nodes always exist
	if len(segments) == 1 {
		if g.findRootLevel(rootName) != nil {
			return &Node{ID: id, Mode: os.ModeDir | 0o555}, nil
		}
		return nil, ErrNotFound // coverage:ignore
	}

	// Deeper levels: check if parent lists this path as a child
	parentPath := strings.Join(segments[:len(segments)-1], "/")
	if g.isChild(parentPath, id) {
		return &Node{ID: id, Mode: os.ModeDir | 0o555}, nil
	}
	return nil, ErrNotFound
}

func (g *SQLiteGraph) ListChildren(id string) ([]string, error) {
	if g.useNodesTable {
		return g.ntr.ListChildren(id)
	}

	id = NormalizeID(id)

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
		return nil, err // coverage:ignore
	} // coverage:ignore

	if v, ok := g.dirChildren.Load(id); ok {
		return v.([]string), nil
	}
	return nil, ErrNotFound // coverage:ignore
}

// ListChildStats returns stat snapshots for all children of a directory.
// For the nodes-table fast path, it queries the nodes table for children
// with kind, size, and mtime — no content rendering.
// For the legacy scan path, it uses dirChildren + schema structure.
// ContentSize may be 0 for unvisited files (FUSE/NFS fall back to LOOKUP).
func (g *SQLiteGraph) ListChildStats(id string) ([]NodeStat, error) {
	if g.useNodesTable {
		return g.ntr.ListChildStats(id)
	}

	id = NormalizeID(id) // coverage:ignore

	// Legacy scan path: use dirChildren + schema to determine child types
	if id == "" { // coverage:ignore
		// Root: return schema root names as directory stats
		var stats []NodeStat         // coverage:ignore
		for _, l := range g.levels { // coverage:ignore
			if l.isStatic { // coverage:ignore
				stats = append(stats, NodeStat{ // coverage:ignore
					ID:    l.staticName, // coverage:ignore
					IsDir: true,         // coverage:ignore
				}) // coverage:ignore
			} // coverage:ignore
		}
		return stats, nil // coverage:ignore
	}

	segments := strings.Split(id, "/")                   // coverage:ignore
	if err := g.ensureScanned(segments[0]); err != nil { // coverage:ignore
		return nil, err // coverage:ignore
	} // coverage:ignore

	v, ok := g.dirChildren.Load(id) // coverage:ignore
	if !ok {                        // coverage:ignore
		return nil, ErrNotFound // coverage:ignore
	} // coverage:ignore
	children := v.([]string) // coverage:ignore

	// Determine the schema level for this directory to know about leaf files
	level, _ := g.walkSchema(segments) // coverage:ignore

	stats := make([]NodeStat, 0, len(children)) // coverage:ignore
	for _, childPath := range children {        // coverage:ignore
		childBase := filepath.Base(childPath) // coverage:ignore
		isFile := false                       // coverage:ignore

		// Check if this child name matches a file leaf at the current schema level
		if level != nil { // coverage:ignore
			for _, f := range level.files { // coverage:ignore
				fname := f.Name                                           // coverage:ignore
				if !strings.Contains(fname, "{{") && fname == childBase { // coverage:ignore
					isFile = true // coverage:ignore
					break         // coverage:ignore
				} // coverage:ignore
			}
		}

		if isFile { // coverage:ignore
			// File node — use sizeCache if available, otherwise 0
			var contentSize int64                              // coverage:ignore
			if cached, ok := g.sizeCache.Load(childPath); ok { // coverage:ignore
				contentSize = cached.(int64) // coverage:ignore
			} // coverage:ignore
			stats = append(stats, NodeStat{ // coverage:ignore
				ID:          childPath,   // coverage:ignore
				IsDir:       false,       // coverage:ignore
				ContentSize: contentSize, // coverage:ignore
				HasOrigin:   false,       // coverage:ignore
			}) // coverage:ignore
		} else { // coverage:ignore
			// Directory node
			stats = append(stats, NodeStat{ // coverage:ignore
				ID:    childPath, // coverage:ignore
				IsDir: true,      // coverage:ignore
			}) // coverage:ignore
		} // coverage:ignore
	}
	return stats, nil // coverage:ignore
}

func (g *SQLiteGraph) ReadContent(id string, buf []byte, offset int64) (int, error) {
	id = NormalizeID(id)

	segments := strings.Split(id, "/")
	_, fileLeaf := g.walkSchema(segments)

	// For nodes-table inline content, fileLeaf may be nil (schema doesn't map these paths).
	// resolveContent handles this: it reads the record column directly from the nodes table.
	if fileLeaf == nil && !g.useNodesTable {
		return 0, ErrNotFound // coverage:ignore
	} // coverage:ignore

	content, err := g.resolveContent(id, segments, fileLeaf)
	if err != nil {
		return 0, err
	}

	return SliceContent(content, buf, offset), nil
}

// Invalidate evicts cached size and content for a node.
// Must be called after write-back modifies a file's content to prevent
// stale size/data from being served on the next Getattr or Read.
func (g *SQLiteGraph) Invalidate(id string) {
	if g.ntr != nil {
		g.ntr.Invalidate(id) // nodes-table path: ntr owns sizeCache + cache
	}
	g.sizeCache.Delete(id) // legacy path sizeCache (no-op when ntr is set)
	if g.cache != nil {
		g.cache.Delete(id) // legacy path cache (nil when ntr is set)
	}
}

// DB returns the main SQLite handle. Exposed so callers can query
// non-refs tables like `_ast` / `_source` / `_lsp*` that ley-line-built
// .db files carry. Mirrors NodesTableReader.DB() — same shape, same
// purpose. The returned handle is owned by SQLiteGraph; do not Close
// it, and queries should be read-only.
func (g *SQLiteGraph) DB() *sql.DB { // coverage:ignore
	return g.db // coverage:ignore
} // coverage:ignore

// Close closes both the source and sidecar database connections.
func (g *SQLiteGraph) Close() error {
	// Unregister from vtab module to prevent leaks/races (sidecar path only)
	if g.refsDB != nil {
		if mod, err := refsvtab.Register(); err == nil && mod != nil {
			mod.UnregisterDB(g.dbID)
		}
	}

	err := g.db.Close()
	if g.refsDB != nil {
		if err2 := g.refsDB.Close(); err == nil {
			err = err2
		}
	}
	return err
}

// walkSchema maps a path to its schema level and (if a file) leaf definition.
// Returns (level, nil) for directories, (level, &leaf) for files, (nil, nil) for invalid paths.
func (g *SQLiteGraph) walkSchema(segments []string) (*schemaLevel, *api.Leaf) {
	return walkSchemaLevels(g.levels, segments)
}

// ---------------------------------------------------------------------------
// Content resolution
// ---------------------------------------------------------------------------

func (g *SQLiteGraph) resolveContent(filePath string, segments []string, leaf *api.Leaf) ([]byte, error) {
	if g.useNodesTable {
		return g.ntr.resolveContent(filePath)
	}

	// Legacy path
	if c, ok := g.cache.Get(filePath); ok {
		return c, nil // coverage:ignore
	} // coverage:ignore

	var content []byte
	{
		// Legacy mode: find parent directory's record ID
		parentPath := strings.Join(segments[:len(segments)-1], "/")
		if err := g.ensureScanned(segments[0]); err != nil {
			return nil, err // coverage:ignore
		} // coverage:ignore

		ridVal, ok := g.recordIDs.Load(parentPath)
		if !ok {
			return nil, ErrNotFound // coverage:ignore
		} // coverage:ignore
		recordID := ridVal.(string)

		// Fetch record from source DB (primary key lookup — instant)
		var raw string
		if err := g.db.QueryRow("SELECT record FROM "+g.tableName+" WHERE id = ?", recordID).Scan(&raw); err != nil {
			return nil, fmt.Errorf("fetch record %s: %w", recordID, err) // coverage:ignore
		} // coverage:ignore

		var parsed any
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			return nil, fmt.Errorf("parse record %s: %w", recordID, err) // coverage:ignore
		} // coverage:ignore
		values, _ := parsed.(map[string]any)

		rendered, err := g.render(leaf.ContentTemplate, values)
		if err != nil {
			return nil, fmt.Errorf("render %s: %w", filePath, err) // coverage:ignore
		} // coverage:ignore
		content = []byte(rendered)
	}

	g.cache.Put(filePath, content)

	return content, nil
}

// Act returns ErrActNotSupported — SQLiteGraph is a passive data graph.
func (g *SQLiteGraph) Act(id, action, payload string) (*ActionResult, error) {
	return nil, ErrActNotSupported
}

// Verify interface compliance at compile time.
var _ Graph = (*SQLiteGraph)(nil)

// Verify fs.FileMode usage (directories need ModeDir bit set).
var _ fs.FileMode = os.ModeDir
