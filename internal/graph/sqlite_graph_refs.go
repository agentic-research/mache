package graph

import (
	"bytes"
	"database/sql"
	"fmt"
	"log"
	"strings"

	"github.com/RoaringBitmap/roaring"
)

// ---------------------------------------------------------------------------
// Definitions (defs): token → defining construct nodeIDs.
//
// Two backing stores share one query API:
//   - In-memory g.defs: populated by AddDef during ingestion (legacy path).
//   - SQL node_defs table: present on pre-built .db files (nodes-table path).
//
// LookupDef / SearchDefs check in-memory first, then fall through to SQL.
// ---------------------------------------------------------------------------

// AddDef records that a construct (dirID) defines the given token.
func (g *SQLiteGraph) AddDef(token, dirID string) error {
	g.pendingMu.Lock()
	defer g.pendingMu.Unlock()
	if g.defs == nil {
		g.defs = make(map[string][]string)
	}
	g.defs[token] = append(g.defs[token], dirID)
	return nil
}

// DefsMap returns a snapshot of the token→dirIDs definition map.
func (g *SQLiteGraph) DefsMap() map[string][]string {
	g.pendingMu.Lock()
	defer g.pendingMu.Unlock()
	cp := make(map[string][]string, len(g.defs))
	for k, v := range g.defs {
		cp[k] = append([]string(nil), v...)
	}
	return cp
}

// SearchDefs returns up to `limit` token→nodeIDs entries whose
// token matches the SQL LIKE pattern. Used by `search role=definition`
// to avoid materializing the full defs map.
//
// For pre-built .db files the in-memory g.defs map is empty; this
// pushes the filter down to SQL against node_defs. Mirrors LookupDef's
// fallback structure: in-memory first (with sqlLikeMatch-style check),
// then SQL. Bead mache-9cba08.
func (g *SQLiteGraph) SearchDefs(pattern string, limit int) map[string][]string {
	if limit <= 0 {
		limit = 100
	}
	out := make(map[string][]string)

	g.pendingMu.Lock()
	for token, ids := range g.defs {
		if !likeMatch(pattern, token) {
			continue
		}
		cp := make([]string, len(ids))
		copy(cp, ids)
		out[token] = cp
		if len(out) >= limit {
			g.pendingMu.Unlock()
			return out
		}
	}
	g.pendingMu.Unlock()

	if !g.useNodesTable || g.db == nil {
		return out
	}
	rows, err := g.db.Query(
		"SELECT token, node_id FROM node_defs WHERE token LIKE ? LIMIT ?",
		pattern, limit-len(out),
	)
	if err != nil {
		return out
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var token, nodeID string
		if scanErr := rows.Scan(&token, &nodeID); scanErr != nil {
			continue
		}
		// In-memory had precedence — don't override an existing entry.
		if _, exists := out[token]; exists {
			continue
		}
		out[token] = append(out[token], nodeID)
		if len(out) >= limit {
			return out
		}
	}
	return out
}

// likeMatch is the in-memory equivalent of SQL LIKE for SearchDefs's
// in-memory pass. Supports % (any-chars) and _ (single-char). Kept
// in-package to avoid pulling the cmd/-level helper across boundaries.
func likeMatch(pattern, value string) bool {
	// Empty pattern matches nothing (consistent with sqlLikeMatch).
	if pattern == "" {
		return false
	}
	return likeMatchRec(pattern, value)
}

func likeMatchRec(pattern, value string) bool {
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '%':
			rest := pattern[i+1:]
			if rest == "" {
				return true
			}
			for j := 0; j <= len(value); j++ {
				if likeMatchRec(rest, value[j:]) {
					return true
				}
			}
			return false
		case '_':
			if len(value) == 0 {
				return false
			}
			value = value[1:]
		default:
			if len(value) == 0 || value[0] != pattern[i] {
				return false
			}
			value = value[1:]
		}
	}
	return len(value) == 0
}

// LookupDef returns the dir IDs that define `token` without
// snapshotting the entire defs map. Returns nil for unknown tokens.
// Mirrors MemoryStore.LookupDef; see that method's docstring.
//
// For pre-built DBs (useNodesTable path) the in-memory g.defs map is
// usually empty because data lives in the `node_defs` SQL table. Falls
// back to the SQL query on in-memory miss so find_definition works
// against ley-line-parsed databases. Matches the SQL pattern used by
// GetCallees for parity.
func (g *SQLiteGraph) LookupDef(token string) []string {
	g.pendingMu.Lock()
	ids, ok := g.defs[token]
	g.pendingMu.Unlock()
	if ok {
		return append([]string(nil), ids...)
	}

	if !g.useNodesTable || g.db == nil {
		return nil
	}
	rows, err := g.db.Query("SELECT node_id FROM node_defs WHERE token = ?", token)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	var dirIDs []string
	for rows.Next() {
		var dirID string
		if scanErr := rows.Scan(&dirID); scanErr == nil && dirID != "" {
			dirIDs = append(dirIDs, dirID)
		}
	}
	return dirIDs
}

// ---------------------------------------------------------------------------
// References (refs): token → list of caller nodeIDs.
//
// Two storage paths:
//   - Legacy path: sidecar refsDB stores roaring bitmaps (token→bitmap)
//     plus file_ids (file path→uint32). AddRef accumulates in memory;
//     FlushRefs writes one transaction.
//   - Nodes-table path: refs already live in main DB's node_refs table.
//     AddRef/FlushRefs are no-ops; reads go directly to NodesTableReader.
// ---------------------------------------------------------------------------

// AddRef accumulates a reference in-memory. No SQL is issued until FlushRefs.
// This eliminates the read-modify-write cycle per call — all bitmap mutations
// happen in RAM, and FlushRefs writes them in a single transaction.
// Not used for nodes-table path (refs already in main DB from mache build).
func (g *SQLiteGraph) AddRef(token, nodeID string) error {
	if g.useNodesTable {
		return nil // refs already in main DB
	}
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
// transaction. Call once after ingestion is complete. Batches all inserts
// (len(fileIDMap) + len(pendingRefs)) into one transaction.
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
// For nodes-table path: queries main DB's node_refs (token, node_id) directly.
// For legacy path: reads roaring bitmaps from the sidecar refs database.
func (g *SQLiteGraph) GetCallers(token string) ([]*Node, error) {
	if g.useNodesTable {
		return g.getCallersFromMainDB(token)
	}
	return g.getCallersFromSidecar(token)
}

// getCallersFromMainDB queries the main DB's node_refs table directly.
// node_refs schema: (token TEXT, node_id TEXT) — written by mache build.
func (g *SQLiteGraph) getCallersFromMainDB(token string) ([]*Node, error) {
	return g.ntr.GetCallers(token)
}

// getCallersFromSidecar reads roaring bitmaps from the sidecar .refs.db.
func (g *SQLiteGraph) getCallersFromSidecar(token string) ([]*Node, error) {
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
		nodes = append(nodes, &Node{
			ID:   path,
			Mode: 0o444,
		})
	}
	return nodes, nil
}

// QueryRefs executes a SQL query against the refs database.
// For nodes-table path: queries the main DB (node_refs has (token, node_id)).
// For legacy path: queries the sidecar (includes mache_refs virtual table).
func (g *SQLiteGraph) QueryRefs(query string, args ...any) (*sql.Rows, error) {
	if g.useNodesTable {
		return g.db.Query(query, args...)
	}
	return g.refsDB.Query(query, args...)
}

// RefsMap returns a token→nodeIDs map for community detection.
// For the nodes-table path, queries node_refs (token, node_id).
// For the legacy bitmap path, decodes bitmaps and resolves file IDs.
func (g *SQLiteGraph) RefsMap() map[string][]string {
	if g.useNodesTable {
		return g.refsMapFromNodesTable()
	}
	return g.refsMapFromBitmaps()
}

func (g *SQLiteGraph) refsMapFromNodesTable() map[string][]string {
	rows, err := g.db.Query("SELECT token, node_id FROM node_refs")
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()

	refs := map[string][]string{}
	for rows.Next() {
		var token, nodeID string
		if err := rows.Scan(&token, &nodeID); err != nil {
			continue
		}
		refs[token] = append(refs[token], nodeID)
	}
	return refs
}

func (g *SQLiteGraph) refsMapFromBitmaps() map[string][]string {
	if g.refsDB == nil {
		return nil
	}

	// Build reverse file ID lookup
	fileRows, err := g.refsDB.Query("SELECT id, path FROM file_ids")
	if err != nil {
		return nil
	}
	idToPath := map[uint32]string{}
	for fileRows.Next() {
		var id uint32
		var path string
		if err := fileRows.Scan(&id, &path); err != nil {
			continue
		}
		idToPath[id] = path
	}
	_ = fileRows.Close()

	// Read bitmaps
	refRows, err := g.refsDB.Query("SELECT token, bitmap FROM node_refs")
	if err != nil {
		return nil
	}
	defer func() { _ = refRows.Close() }()

	refs := map[string][]string{}
	for refRows.Next() {
		var token string
		var blob []byte
		if err := refRows.Scan(&token, &blob); err != nil {
			continue
		}
		bm := roaring.New()
		if _, err := bm.ReadFrom(bytes.NewReader(blob)); err != nil {
			continue
		}
		for _, id := range bm.ToArray() {
			if path, ok := idToPath[id]; ok {
				refs[token] = append(refs[token], path)
			}
		}
	}
	return refs
}
