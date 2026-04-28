package ingest

import (
	"database/sql"
	"path/filepath"
	"sort"
	"testing"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// seedCallExtractionAST creates a minimal _ast database modeling a Go file
// shaped like the output of `leyline parse`, where node IDs encode the
// kind chain (so matchAncestry compares correctly).
//
//	package main
//	func A() {
//	    fmt.Println("x")  // qualified: pkg=fmt, call=Println
//	    Helper()          // bare: call=Helper
//	}
func seedCallExtractionAST(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "calls.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)

	_, err = db.Exec(`
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY, parent_id TEXT, name TEXT NOT NULL,
			kind INTEGER NOT NULL, size INTEGER DEFAULT 0,
			mtime INTEGER NOT NULL, record_id TEXT, record JSON,
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
		CREATE TABLE _source (id TEXT PRIMARY KEY, language TEXT NOT NULL, content BLOB NOT NULL);

		INSERT INTO _source VALUES ('main.go', 'go', '');
	`)
	require.NoError(t, err)

	type row struct {
		id, parentID string
		kind         int
		record       string
	}
	// Path scheme mirrors LLO output: each segment is the kind (with _N
	// disambiguator when siblings repeat).
	rows := []row{
		// Qualified call: fmt.Println
		{"call_expression", "", 1, ""},
		{"call_expression/selector_expression", "call_expression", 1, ""},
		{"call_expression/selector_expression/identifier", "call_expression/selector_expression", 0, "fmt"},
		{"call_expression/selector_expression/field_identifier", "call_expression/selector_expression", 0, "Println"},
		// Bare call: Helper
		{"call_expression_1", "", 1, ""},
		{"call_expression_1/identifier", "call_expression_1", 0, "Helper"},
	}
	for _, r := range rows {
		_, err := db.Exec(
			"INSERT INTO nodes (id, parent_id, name, kind, mtime, record) VALUES (?, ?, ?, ?, 0, ?)",
			r.id, r.parentID, filepath.Base(r.id), r.kind, r.record,
		)
		require.NoError(t, err)
	}

	type ast struct {
		id, kind  string
		startByte int
	}
	asts := []ast{
		// Qualified: byte ranges chosen so start_byte sort is intuitive
		{"call_expression", "call_expression", 0},
		{"call_expression/selector_expression", "selector_expression", 0},
		{"call_expression/selector_expression/identifier", "identifier", 0},
		{"call_expression/selector_expression/field_identifier", "field_identifier", 4},
		// Bare
		{"call_expression_1", "call_expression", 100},
		{"call_expression_1/identifier", "identifier", 100},
	}
	for _, a := range asts {
		_, err := db.Exec(
			"INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col) VALUES (?, 'main.go', ?, ?, 0, 0, 0, 0, 0)",
			a.id, a.kind, a.startByte,
		)
		require.NoError(t, err)
	}
	return db
}

func TestASTWalker_ExtractCalls_Go(t *testing.T) {
	db := seedCallExtractionAST(t)
	defer func() { _ = db.Close() }()

	w := NewASTWalker(db)
	calls, err := w.ExtractCalls("main.go", "go")
	require.NoError(t, err)

	sort.Strings(calls)
	assert.Equal(t, []string{"Helper", "Println"}, calls,
		"both bare and qualified calls should be returned by ExtractCalls")
}

func TestASTWalker_ExtractQualifiedCalls_Go(t *testing.T) {
	db := seedCallExtractionAST(t)
	defer func() { _ = db.Close() }()

	w := NewASTWalker(db)
	calls, err := w.ExtractQualifiedCalls("main.go", "go")
	require.NoError(t, err)

	sort.Slice(calls, func(i, j int) bool {
		if calls[i].Token != calls[j].Token {
			return calls[i].Token < calls[j].Token
		}
		return calls[i].Qualifier < calls[j].Qualifier
	})

	require.Len(t, calls, 2)
	assert.Equal(t, graph.QualifiedCall{Token: "Helper", Qualifier: ""}, calls[0],
		"bare call has no qualifier")
	assert.Equal(t, graph.QualifiedCall{Token: "Println", Qualifier: "fmt"}, calls[1],
		"qualified call captures pkg=fmt")
}

func TestASTWalker_ExtractCalls_UnknownLanguageReturnsNil(t *testing.T) {
	db := seedCallExtractionAST(t)
	defer func() { _ = db.Close() }()

	w := NewASTWalker(db)
	calls, err := w.ExtractCalls("main.go", "unknown-lang")
	require.NoError(t, err)
	assert.Nil(t, calls, "unregistered language returns nil, not error")
}

func TestASTWalker_ExtractCalls_Dedupes(t *testing.T) {
	db := seedCallExtractionAST(t)
	defer func() { _ = db.Close() }()

	// Add a second bare call to the same name "Helper" (call_expression_2).
	_, err := db.Exec(`
		INSERT INTO nodes (id, parent_id, name, kind, mtime, record)
		VALUES
		  ('call_expression_2', '', 'call_expression_2', 1, 0, ''),
		  ('call_expression_2/identifier', 'call_expression_2', 'identifier', 0, 0, 'Helper')
	`)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col)
		VALUES
		  ('call_expression_2', 'main.go', 'call_expression', 200, 0, 0, 0, 0, 0),
		  ('call_expression_2/identifier', 'main.go', 'identifier', 200, 0, 0, 0, 0, 0)
	`)
	require.NoError(t, err)

	w := NewASTWalker(db)
	calls, err := w.ExtractCalls("main.go", "go")
	require.NoError(t, err)

	helperCount := 0
	for _, c := range calls {
		if c == "Helper" {
			helperCount++
		}
	}
	assert.Equal(t, 1, helperCount, "duplicate Helper calls must be deduplicated")
}
