package ingest

import (
	"database/sql"
	"encoding/json"
	"fmt"

	_ "modernc.org/sqlite"
)

// LoadSQLite opens a SQLite database, reads all records from the results table,
// parses each JSON record, and returns them as a slice.
// Phase 0: loads everything into memory so we can measure baseline cost.
func LoadSQLite(dbPath string) ([]any, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", dbPath, err)
	}
	defer func() { _ = db.Close() }()

	rows, err := db.Query("SELECT record FROM results")
	if err != nil {
		return nil, fmt.Errorf("query results: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var records []any
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		var parsed any
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			return nil, fmt.Errorf("parse record json: %w", err)
		}
		records = append(records, parsed)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rows: %w", err)
	}

	return records, nil
}
