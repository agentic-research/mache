package graph

import (
	"database/sql"
	"encoding/json"
	"io/fs"
	"path"
	"time"

	_ "modernc.org/sqlite"
)

// nodeKindFile and nodeKindDir mirror the wire-format constants in
// internal/graph (NodeKindFile/NodeKindDir). Duplicated here because
// this is the public graph/ package and Go's import rules forbid
// reaching into internal/. The values are part of mache's on-disk
// schema and must stay in sync.
const (
	nodeKindFile = 0
	nodeKindDir  = 1
)

// ExportSQLite writes all nodes from a MemoryStore to a SQLite database.
// Creates the nodes table if it doesn't exist. Existing entries are overwritten.
// The resulting file uses the standard mache nodes table schema.
func ExportSQLite(store *MemoryStore, dbPath string) error {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS nodes (
		id TEXT PRIMARY KEY,
		parent_id TEXT,
		name TEXT NOT NULL,
		kind INTEGER NOT NULL,
		size INTEGER DEFAULT 0,
		mtime INTEGER NOT NULL,
		-- TEXT, not JSON: "JSON" selects no SQLite affinity and falls through to
		-- NUMERIC, which rewrites numeric-looking TEXT ('007' -> 7). This writer
		-- binds a marshalled object, so it always starts with '{' and is immune
		-- in practice — immune by payload shape, not by declaration, which is
		-- exactly the accident that hid the same bug elsewhere (mache-4b8a42).
		record TEXT
	)`); err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}

	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO nodes
		(id, parent_id, name, kind, size, mtime, record)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer func() { _ = stmt.Close() }()

	for _, rootID := range store.RootIDs() {
		if err := exportNode(store, stmt, rootID, ""); err != nil {
			_ = tx.Rollback()
			return err
		}
	}

	return tx.Commit()
}

// exportNode recursively writes a node and its children to the prepared statement.
func exportNode(store *MemoryStore, stmt *sql.Stmt, nodeID, parentID string) error {
	node, err := store.GetNode(nodeID)
	if err != nil {
		return err
	}

	kind := nodeKindFile
	if node.Mode.IsDir() {
		kind = nodeKindDir
	}

	// Build record JSON: inline data + properties.
	var record *string
	if len(node.Data) > 0 || len(node.Properties) > 0 {
		r := make(map[string]any)
		if len(node.Data) > 0 {
			r["data"] = string(node.Data)
		}
		if len(node.Properties) > 0 {
			// Properties values are already JSON (mache-90b89b); nest them
			// directly. Stringifying them here and re-encoding on import
			// double-quoted every value.
			r["properties"] = node.Properties
		}
		b, _ := json.Marshal(r)
		s := string(b)
		record = &s
	}

	if _, err := stmt.Exec(
		nodeID, parentID, path.Base(nodeID),
		kind, node.ContentSize(), node.ModTime.UnixNano(), record,
	); err != nil {
		return err
	}

	for _, childID := range node.Children {
		if err := exportNode(store, stmt, childID, nodeID); err != nil {
			return err
		}
	}
	return nil
}

// ImportSQLite reads nodes from a SQLite database into a new MemoryStore.
// The database must have a nodes table in the standard mache format.
func ImportSQLite(dbPath string) (*MemoryStore, error) {
	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	rows, err := db.Query(`SELECT id, parent_id, kind, mtime, record FROM nodes`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	type nodeRow struct {
		id       string
		parentID string
		kind     int
		mtime    int64
		record   sql.NullString
	}
	var allRows []nodeRow

	for rows.Next() {
		var r nodeRow
		if err := rows.Scan(&r.id, &r.parentID, &r.kind, &r.mtime, &r.record); err != nil {
			return nil, err
		}
		allRows = append(allRows, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	store := NewMemoryStore()

	// First pass: create all nodes.
	for _, r := range allRows {
		node := &Node{
			ID:      r.id,
			ModTime: time.Unix(0, r.mtime),
		}
		if r.kind == nodeKindDir {
			node.Mode = fs.ModeDir
			node.Children = []string{}
		}

		// Parse record JSON for inline data and properties.
		if r.record.Valid && r.record.String != "" {
			var rec struct {
				Data       string                     `json:"data"`
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if err := json.Unmarshal([]byte(r.record.String), &rec); err == nil {
				if rec.Data != "" {
					node.Data = []byte(rec.Data)
				}
				if len(rec.Properties) > 0 {
					node.Properties = rec.Properties
				}
			}
		}

		if r.parentID == "" {
			store.AddRoot(node)
		} else {
			store.AddNode(node)
		}
	}

	// Second pass: wire parent-child relationships.
	for _, r := range allRows {
		if r.parentID != "" {
			parent, err := store.GetNode(r.parentID)
			if err == nil && parent.Mode.IsDir() {
				parent.Children = append(parent.Children, r.id)
			}
		}
	}

	return store, nil
}
