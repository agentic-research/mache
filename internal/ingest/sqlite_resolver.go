package ingest

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/agentic-research/mache/internal/graph"
	_ "modernc.org/sqlite"
)

// SQLiteResolver resolves ContentRef entries by fetching records from SQLite
// and re-rendering their content templates.
type SQLiteResolver struct {
	mu  sync.Mutex
	dbs map[string]*sql.DB
}

func NewSQLiteResolver() *SQLiteResolver {
	return &SQLiteResolver{
		dbs: make(map[string]*sql.DB),
	}
}

// Resolve fetches a record from SQLite and renders its content template.
func (r *SQLiteResolver) Resolve(ref *graph.ContentRef) ([]byte, error) {
	db, err := r.getDB(ref.DBPath)
	if err != nil {
		return nil, err
	}

	var raw string
	err = db.QueryRow("SELECT record FROM results WHERE id = ?", ref.RecordID).Scan(&raw)
	if err != nil {
		return nil, fmt.Errorf("resolve record %s: %w", ref.RecordID, err)
	}

	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("parse record %s: %w", ref.RecordID, err)
	}

	values, _ := parsed.(map[string]any)
	content, err := RenderTemplate(ref.Template, values)
	if err != nil {
		return nil, fmt.Errorf("render template for %s: %w", ref.RecordID, err)
	}

	return []byte(content), nil
}

func (r *SQLiteResolver) getDB(path string) (*sql.DB, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if db, ok := r.dbs[path]; ok {
		return db, nil
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA query_only=ON"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set query_only: %w", err)
	}

	r.dbs[path] = db
	return db, nil
}

// Close closes all open database connections.
func (r *SQLiteResolver) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, db := range r.dbs {
		_ = db.Close()
	}
}
