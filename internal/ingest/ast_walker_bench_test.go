package ingest

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// seedManyCalls populates a fresh _ast database with `nCalls` Go function
// calls, half qualified (fmt.Println-style) and half bare (Helper-style).
// Returns the *sql.DB and the source path to pass to ExtractCalls.
//
// The shape is intentionally similar to what `leyline parse` produces for
// real Go source: each call gets a unique id with the kind chain encoded.
// Takes testing.TB so the growth-class GATE
// (ast_walker_growth_test.go) can seed the same shape the benchmark
// measures — one fixture, so the gate cannot drift from what is
// benchmarked (mache-c0537f).
func seedManyCalls(tb testing.TB, dir string, nCalls int) *sql.DB {
	tb.Helper()
	dbPath := filepath.Join(dir, "bench.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		tb.Fatal(err)
	}

	if _, err := db.Exec(`
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY, parent_id TEXT, name TEXT NOT NULL,
			kind INTEGER NOT NULL, size INTEGER DEFAULT 0,
			mtime INTEGER NOT NULL, record_id TEXT, record TEXT,
			source_file TEXT
		);
		CREATE TABLE _ast (
			node_id TEXT PRIMARY KEY, source_id TEXT NOT NULL,
			node_kind TEXT NOT NULL, start_byte INTEGER NOT NULL,
			end_byte INTEGER NOT NULL,
			start_row INTEGER, start_col INTEGER,
			end_row INTEGER, end_col INTEGER
		);
		CREATE INDEX idx_ast_source ON _ast(source_id);
		CREATE INDEX idx_ast_kind_source ON _ast(node_kind, source_id);
		CREATE INDEX idx_parent_name ON nodes(parent_id, name);
		CREATE TABLE _source (id TEXT PRIMARY KEY, language TEXT NOT NULL, content BLOB NOT NULL);

		INSERT INTO _source VALUES ('main.go', 'go', '');
	`); err != nil {
		tb.Fatal(err)
	}

	tx, err := db.Begin()
	if err != nil {
		tb.Fatal(err)
	}
	run := func(query string, args ...any) {
		tb.Helper()
		if _, err := tx.Exec(query, args...); err != nil {
			tb.Fatalf("seed: %v\nquery=%s\nargs=%v", err, query, args)
		}
	}
	for i := range nCalls {
		callID := fmt.Sprintf("call_expression_%d", i)
		if i%2 == 0 {
			selID := callID + "/selector_expression"
			pkgID := selID + "/identifier"
			fldID := selID + "/field_identifier"
			pkgName := fmt.Sprintf("pkg%d", i/2)
			fnName := fmt.Sprintf("Func%d", i/2)
			run("INSERT INTO nodes (id, parent_id, name, kind, mtime, record) VALUES (?, '', ?, 1, 0, '')", callID, callID)
			run("INSERT INTO nodes (id, parent_id, name, kind, mtime, record) VALUES (?, ?, 'selector_expression', 1, 0, '')", selID, callID)
			run("INSERT INTO nodes (id, parent_id, name, kind, mtime, record) VALUES (?, ?, 'identifier', 0, 0, ?)", pkgID, selID, pkgName)
			run("INSERT INTO nodes (id, parent_id, name, kind, mtime, record) VALUES (?, ?, 'field_identifier', 0, 0, ?)", fldID, selID, fnName)
			run("INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col) VALUES (?, 'main.go', 'call_expression', ?, 0, 0, 0, 0, 0)", callID, i*100)
			run("INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col) VALUES (?, 'main.go', 'selector_expression', ?, 0, 0, 0, 0, 0)", selID, i*100)
			run("INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col) VALUES (?, 'main.go', 'identifier', ?, 0, 0, 0, 0, 0)", pkgID, i*100)
			run("INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col) VALUES (?, 'main.go', 'field_identifier', ?, 0, 0, 0, 0, 0)", fldID, i*100+5)
		} else {
			identID := callID + "/identifier"
			fnName := fmt.Sprintf("Bare%d", i/2)
			run("INSERT INTO nodes (id, parent_id, name, kind, mtime, record) VALUES (?, '', ?, 1, 0, '')", callID, callID)
			run("INSERT INTO nodes (id, parent_id, name, kind, mtime, record) VALUES (?, ?, 'identifier', 0, 0, ?)", identID, callID, fnName)
			run("INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col) VALUES (?, 'main.go', 'call_expression', ?, 0, 0, 0, 0, 0)", callID, i*100)
			run("INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col) VALUES (?, 'main.go', 'identifier', ?, 0, 0, 0, 0, 0)", identID, i*100)
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
	return db
}

func BenchmarkASTWalker_ExtractCalls(b *testing.B) {
	for _, n := range []int{10, 100, 500} {
		b.Run(fmt.Sprintf("calls=%d", n), func(b *testing.B) {
			db := seedManyCalls(b, b.TempDir(), n)
			defer func() { _ = db.Close() }()

			w := NewASTWalker(db)
			// Warm-up to amortize one-time SQL planning.
			if _, err := w.ExtractCalls("main.go", "go"); err != nil {
				b.Fatal(err)
			}

			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				calls, err := w.ExtractCalls("main.go", "go")
				if err != nil {
					b.Fatal(err)
				}
				if len(calls) == 0 {
					b.Fatal("no calls extracted")
				}
			}
		})
	}
}

func BenchmarkASTWalker_ExtractQualifiedCalls(b *testing.B) {
	for _, n := range []int{10, 100, 500} {
		b.Run(fmt.Sprintf("calls=%d", n), func(b *testing.B) {
			db := seedManyCalls(b, b.TempDir(), n)
			defer func() { _ = db.Close() }()

			w := NewASTWalker(db)
			if _, err := w.ExtractQualifiedCalls("main.go", "go"); err != nil {
				b.Fatal(err)
			}

			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				calls, err := w.ExtractQualifiedCalls("main.go", "go")
				if err != nil {
					b.Fatal(err)
				}
				if len(calls) == 0 {
					b.Fatal("no calls extracted")
				}
			}
		})
	}
}
