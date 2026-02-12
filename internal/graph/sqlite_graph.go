package graph

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/agentic-research/mache/api"
	_ "modernc.org/sqlite"
)

// TemplateRenderer renders a Go text/template string with the given values map.
type TemplateRenderer func(tmpl string, values map[string]any) (string, error)

// SQLiteGraph implements Graph by querying the source SQLite database directly.
// No index copy, no ingestion step. The source DB's B+ tree IS the index.
// Directory structure is derived lazily from schema + DB on first access, then cached.
type SQLiteGraph struct {
	db     *sql.DB
	dbPath string
	schema *api.Topology
	render TemplateRenderer
	levels []*schemaLevel

	// Lazy scan: one pass per root node populates all directory caches
	scanOnce sync.Map // root name → *sync.Once
	scanErr  sync.Map // root name → error

	// Directory children (populated by scan, then read-only)
	dirChildren sync.Map // dir path (string) → []string (sorted child full paths)

	// Record mapping: leaf directory path → record table ID
	recordIDs sync.Map // dir path (string) → string (record ID)

	// Rendered content cache (FIFO-bounded)
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

// OpenSQLiteGraph opens a read-only connection to the source DB and compiles the schema.
// Mount time is instant — no scanning until the first directory access.
func OpenSQLiteGraph(dbPath string, schema *api.Topology, render TemplateRenderer) (*SQLiteGraph, error) {
	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", dbPath, err)
	}

	db.SetMaxOpenConns(4)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}

	return &SQLiteGraph{
		db:           db,
		dbPath:       dbPath,
		schema:       schema,
		render:       render,
		levels:       compileLevels(schema),
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

	// File node — render content to get accurate size
	if fileLeaf != nil {
		content, err := g.resolveContent(id, segments, fileLeaf)
		if err != nil {
			return nil, err
		}
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

// Close closes the underlying database connection.
func (g *SQLiteGraph) Close() error {
	return g.db.Close()
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

// scanRoot performs a single-pass scan of all DB records to build the directory
// tree for one root node. After this, all ListChildren calls are cache lookups.
func (g *SQLiteGraph) scanRoot(rootName string) error {
	level := g.findRootLevel(rootName)
	if level == nil {
		return fmt.Errorf("root %q not found in schema", rootName)
	}

	childSets := make(map[string]map[string]bool)
	recIDs := make(map[string]string)

	// Ensure root node always has an entry (even if DB is empty)
	childSets[rootName] = make(map[string]bool)

	rows, err := g.db.Query("SELECT id, record FROM results")
	if err != nil {
		return fmt.Errorf("scan query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	count := 0
	for rows.Next() {
		var recordID, raw string
		if err := rows.Scan(&recordID, &raw); err != nil {
			continue
		}
		var parsed any
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			continue
		}
		values, ok := parsed.(map[string]any)
		if !ok {
			continue
		}

		g.buildPaths(level, values, rootName, recordID, childSets, recIDs)
		count++
		if count%100000 == 0 {
			fmt.Printf("\rScanning %d records...", count)
		}
	}
	if count >= 100000 {
		fmt.Printf("\rScanned %d records.\n", count)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("scan rows: %w", err)
	}

	// Convert sets → sorted slices and store
	for parent, childSet := range childSets {
		sorted := make([]string, 0, len(childSet))
		for c := range childSet {
			sorted = append(sorted, c)
		}
		sort.Strings(sorted)
		g.dirChildren.Store(parent, sorted)
	}
	for path, id := range recIDs {
		g.recordIDs.Store(path, id)
	}

	return nil
}

// buildPaths walks the schema children for one record, accumulating directory
// entries and record-ID mappings. Called once per DB row during scan.
func (g *SQLiteGraph) buildPaths(level *schemaLevel, values map[string]any, parentPath, recordID string, childSets map[string]map[string]bool, recIDs map[string]string) {
	for _, child := range level.children {
		name, err := g.render(child.nameRaw, values)
		if err != nil || name == "" {
			continue
		}

		childPath := parentPath + "/" + name

		if childSets[parentPath] == nil {
			childSets[parentPath] = make(map[string]bool)
		}
		childSets[parentPath][childPath] = true

		// Recurse into deeper directory levels
		if len(child.children) > 0 {
			g.buildPaths(child, values, childPath, recordID, childSets, recIDs)
		}

		// Leaf directory: add file children and record mapping
		if len(child.files) > 0 {
			recIDs[childPath] = recordID
			if childSets[childPath] == nil {
				childSets[childPath] = make(map[string]bool)
			}
			for _, f := range child.files {
				childSets[childPath][childPath+"/"+f.Name] = true
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
