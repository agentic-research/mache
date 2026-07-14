package ingest

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// seedSyntheticASTDB creates the three leyline tables (_ast, node_child,
// node_content) and inserts a Go function_declaration with name/parameters/
// body field children plus a fieldless `func` keyword child, mirroring the
// verified leyline v0.7.5 data model (field is NULL for fieldless children;
// node_child includes anonymous tokens; _ast holds only named nodes).
func seedSyntheticASTDB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "synthetic.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	ddl := []string{
		`CREATE TABLE _ast (
			node_id TEXT PRIMARY KEY,
			source_id TEXT NOT NULL,
			node_kind TEXT NOT NULL,
			start_byte INTEGER NOT NULL,
			end_byte INTEGER NOT NULL,
			start_row INTEGER NOT NULL,
			start_col INTEGER NOT NULL,
			end_row INTEGER NOT NULL,
			end_col INTEGER NOT NULL,
			node_hash BLOB REFERENCES node_content(node_hash)
		)`,
		`CREATE TABLE node_child (
			parent_hash BLOB NOT NULL REFERENCES node_content(node_hash),
			ordinal INTEGER NOT NULL,
			child_hash BLOB NOT NULL REFERENCES node_content(node_hash),
			field TEXT,
			PRIMARY KEY (parent_hash, ordinal)
		)`,
		`CREATE TABLE node_content (
			node_hash BLOB PRIMARY KEY,
			node_tag INTEGER NOT NULL,
			kind TEXT NOT NULL,
			raw_kind TEXT NOT NULL,
			lang TEXT NOT NULL,
			token TEXT,
			arity INTEGER NOT NULL
		)`,
	}
	for _, stmt := range ddl {
		_, err := db.Exec(stmt)
		require.NoError(t, err)
	}

	hashFunc := []byte("h-func")
	hashKw := []byte("h-kw")
	hashName := []byte("h-name")
	hashParams := []byte("h-params")
	hashBody := []byte("h-body")
	hashPyDef := []byte("h-pydef")

	contentRows := []struct {
		hash    []byte
		kind    string
		rawKind string
		lang    string
	}{
		{hashFunc, "function", "function_declaration", "go"},
		{hashKw, "keyword", "func", "go"},
		{hashName, "identifier", "identifier", "go"},
		{hashParams, "parameters", "parameter_list", "go"},
		{hashBody, "block", "block", "go"},
		{hashPyDef, "function", "function_definition", "python"},
	}
	for _, r := range contentRows {
		_, err := db.Exec(
			`INSERT INTO node_content (node_hash, node_tag, kind, raw_kind, lang, token, arity) VALUES (?, 0, ?, ?, ?, NULL, 0)`,
			r.hash, r.kind, r.rawKind, r.lang,
		)
		require.NoError(t, err)
	}

	// _ast holds named nodes only: the function_declaration and its three
	// named children — NOT the anonymous `func` keyword. A second source
	// (other.py) exercises the sourceLike filter.
	astRows := []struct {
		nodeID   string
		sourceID string
		kind     string
		start    int
		end      int
		hash     []byte
	}{
		{"main.go/function_declaration", "main.go", "function_declaration", 0, 40, hashFunc},
		{"main.go/function_declaration/identifier", "main.go", "identifier", 5, 10, hashName},
		{"main.go/function_declaration/parameter_list", "main.go", "parameter_list", 10, 12, hashParams},
		{"main.go/function_declaration/block", "main.go", "block", 13, 40, hashBody},
		{"other.py/function_definition", "other.py", "function_definition", 0, 20, hashPyDef},
	}
	for _, r := range astRows {
		_, err := db.Exec(
			`INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col, node_hash)
			 VALUES (?, ?, ?, ?, ?, 0, 0, 0, 0, ?)`,
			r.nodeID, r.sourceID, r.kind, r.start, r.end, r.hash,
		)
		require.NoError(t, err)
	}

	// Children of the function_declaration: the fieldless `func` keyword
	// (field NULL, like real leyline output) plus three field-carrying
	// children.
	childRows := []struct {
		parent  []byte
		ordinal int
		child   []byte
		field   any
	}{
		{hashFunc, 0, hashKw, nil},
		{hashFunc, 1, hashName, "name"},
		{hashFunc, 2, hashParams, "parameters"},
		{hashFunc, 3, hashBody, "body"},
	}
	for _, r := range childRows {
		_, err := db.Exec(
			`INSERT INTO node_child (parent_hash, ordinal, child_hash, field) VALUES (?, ?, ?, ?)`,
			r.parent, r.ordinal, r.child, r.field,
		)
		require.NoError(t, err)
	}

	return db
}

func TestFlattenASTDB_Synthetic(t *testing.T) {
	db := seedSyntheticASTDB(t)

	records, err := FlattenASTDB(db, "", 0)
	require.NoError(t, err)
	require.Len(t, records, 5)

	// Document order: function_declaration (start 0, widest span) first,
	// then its named children by start byte, then other.py's node.
	assert.Equal(t, map[string]any{
		"type":                  "function_declaration",
		"has_name":              true,
		"field_name_type":       "identifier",
		"has_parameters":        true,
		"field_parameters_type": "parameter_list",
		"has_body":              true,
		"field_body_type":       "block",
	}, records[0])
	assert.Equal(t, map[string]any{"type": "identifier"}, records[1])
	assert.Equal(t, map[string]any{"type": "parameter_list"}, records[2])
	assert.Equal(t, map[string]any{"type": "block"}, records[3])
	assert.Equal(t, map[string]any{"type": "function_definition"}, records[4])
}

func TestFlattenASTDB_SourceFilter(t *testing.T) {
	db := seedSyntheticASTDB(t)

	goRecords, err := FlattenASTDB(db, "%.go", 0)
	require.NoError(t, err)
	require.Len(t, goRecords, 4)
	for _, rec := range goRecords {
		assert.NotEqual(t, "function_definition", rec.(map[string]any)["type"])
	}

	pyRecords, err := FlattenASTDB(db, "%.py", 0)
	require.NoError(t, err)
	require.Len(t, pyRecords, 1)
	assert.Equal(t, map[string]any{"type": "function_definition"}, pyRecords[0])

	none, err := FlattenASTDB(db, "%.rs", 0)
	require.NoError(t, err)
	assert.Empty(t, none)
}

func TestFlattenASTDB_Limit(t *testing.T) {
	db := seedSyntheticASTDB(t)

	records, err := FlattenASTDB(db, "", 2)
	require.NoError(t, err)
	require.Len(t, records, 2)
	// The limit bounds records, not join rows: the first record is the
	// full multi-field function_declaration, not a truncated fragment.
	assert.Equal(t, "function_declaration", records[0].(map[string]any)["type"])
	assert.Equal(t, true, records[0].(map[string]any)["has_body"])
	assert.Equal(t, map[string]any{"type": "identifier"}, records[1])

	// Limit larger than the population returns everything.
	all, err := FlattenASTDB(db, "", 100)
	require.NoError(t, err)
	assert.Len(t, all, 5)
}
