package ingest

import (
	"database/sql"
	"encoding/json"
	"fmt"

	_ "modernc.org/sqlite"
)

// StreamSQLite iterates over all records in a SQLite database, calling fn for each one.
// Only one parsed record is alive at a time, keeping memory usage constant.
func StreamSQLite(dbPath string, fn func(recordID string, record any) error) error {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("open sqlite %s: %w", dbPath, err)
	}
	defer func() { _ = db.Close() }() // safe to ignore

	rows, err := db.Query("SELECT id, record FROM results")
	if err != nil {
		return fmt.Errorf("query results: %w", err)
	}
	defer func() { _ = rows.Close() }() // safe to ignore

	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return fmt.Errorf("scan row: %w", err)
		}
		var parsed any
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			return fmt.Errorf("parse record json: %w", err)
		}
		if err := fn(id, parsed); err != nil {
			return err
		}
	}
	return rows.Err()
}

// StreamSQLiteRaw iterates over all records yielding raw (id, json) strings
// without parsing. Used by the parallel ingestion pipeline where workers
// handle JSON parsing on their own goroutines.
func StreamSQLiteRaw(dbPath string, fn func(id, raw string) error) error {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("open sqlite %s: %w", dbPath, err)
	}
	defer func() { _ = db.Close() }() // safe to ignore

	rows, err := db.Query("SELECT id, record FROM results")
	if err != nil {
		return fmt.Errorf("query results: %w", err)
	}
	defer func() { _ = rows.Close() }() // safe to ignore

	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return fmt.Errorf("scan row: %w", err)
		}
		if err := fn(id, raw); err != nil {
			return err
		}
	}
	return rows.Err()
}

// LoadSQLite opens a SQLite database, reads all records from the results table,
// parses each JSON record, and returns them as a slice.
// Kept for backward compatibility with tests; prefer StreamSQLite for large datasets.
func LoadSQLite(dbPath string) ([]any, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", dbPath, err)
	}
	defer func() { _ = db.Close() }() // safe to ignore

	rows, err := db.Query("SELECT record FROM results")
	if err != nil {
		return nil, fmt.Errorf("query results: %w", err)
	}
	defer func() { _ = rows.Close() }() // safe to ignore

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
