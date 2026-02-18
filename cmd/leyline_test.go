package cmd

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/agentic-research/mache/api"
	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY,
			parent_id TEXT,
			name TEXT NOT NULL,
			kind INTEGER NOT NULL,
			size INTEGER DEFAULT 0,
			mtime INTEGER NOT NULL,
			record_id TEXT,
			record JSON
		);
		CREATE INDEX idx_parent_name ON nodes(parent_id, name);

		CREATE TABLE node_refs (
			token TEXT,
			node_id TEXT,
			PRIMARY KEY (token, node_id)
		) WITHOUT ROWID;

		CREATE TABLE node_defs (
			token TEXT,
			dir_id TEXT,
			PRIMARY KEY (token, dir_id)
		) WITHOUT ROWID;

		-- Function directories
		INSERT INTO nodes VALUES ('functions', '', 'functions', 1, 0, 1000, NULL, NULL);
		INSERT INTO nodes VALUES ('functions/HandleRequest', 'functions', 'HandleRequest', 1, 0, 2000, NULL, NULL);
		INSERT INTO nodes VALUES ('functions/HandleRequest/source', 'functions/HandleRequest', 'source', 0, 20, 3000, NULL, 'func HandleRequest(){}');
		INSERT INTO nodes VALUES ('functions/ProcessOrder', 'functions', 'ProcessOrder', 1, 0, 2000, NULL, NULL);
		INSERT INTO nodes VALUES ('functions/ProcessOrder/source', 'functions/ProcessOrder', 'source', 0, 20, 3000, NULL, 'func ProcessOrder(){}');

		-- ProcessOrder calls HandleRequest
		INSERT INTO node_refs VALUES ('HandleRequest', 'functions/ProcessOrder/source');

		-- HandleRequest is defined in functions/HandleRequest
		INSERT INTO node_defs VALUES ('HandleRequest', 'functions/HandleRequest');
	`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestMaterializeCallers(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}

	if err := materializeCallers(tx, 9999); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Verify callers/ dir was created
	var kind int
	err = db.QueryRow(`SELECT kind FROM nodes WHERE id = 'functions/HandleRequest/callers'`).Scan(&kind)
	if err != nil {
		t.Fatalf("callers dir not found: %v", err)
	}
	if kind != 1 {
		t.Fatalf("expected callers to be dir (kind=1), got %d", kind)
	}

	// Verify caller entry: ProcessOrder
	var record string
	err = db.QueryRow(`SELECT record FROM nodes WHERE id = 'functions/HandleRequest/callers/ProcessOrder'`).Scan(&record)
	if err != nil {
		t.Fatalf("caller entry not found: %v", err)
	}
	if record != "functions/ProcessOrder/source" {
		t.Fatalf("expected record to point to caller node, got %q", record)
	}
}

func TestMaterializeCallersNoRefs(t *testing.T) {
	// If node_refs table doesn't exist, materializeCallers should be a no-op.
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY, parent_id TEXT, name TEXT NOT NULL,
			kind INTEGER NOT NULL, size INTEGER DEFAULT 0,
			mtime INTEGER NOT NULL, record JSON
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}

	// Should not error when node_refs doesn't exist
	if err := materializeCallers(tx, 9999); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestMaterializeSchemaJSON(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	schema := &api.Topology{
		Version: "v1",
		Nodes: []api.Node{
			{Name: "functions", Selector: "(function_declaration)"},
		},
	}

	// Use a temp file since materializeVirtuals opens by path
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"

	// Dump in-memory DB to file
	_, err := db.Exec(`VACUUM INTO ?`, dbPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := materializeVirtuals(dbPath, schema, false); err != nil {
		t.Fatal(err)
	}

	// Re-open and verify
	fileDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fileDB.Close() }()

	var record string
	err = fileDB.QueryRow(`SELECT record FROM nodes WHERE id = '_schema.json'`).Scan(&record)
	if err != nil {
		t.Fatalf("_schema.json not found: %v", err)
	}

	// Verify it's valid JSON with correct content
	var parsed api.Topology
	if err := json.Unmarshal([]byte(record), &parsed); err != nil {
		t.Fatalf("invalid JSON in _schema.json: %v", err)
	}
	if parsed.Version != "v1" {
		t.Fatalf("expected version v1, got %s", parsed.Version)
	}
	if len(parsed.Nodes) != 1 || parsed.Nodes[0].Name != "functions" {
		t.Fatalf("unexpected schema nodes: %+v", parsed.Nodes)
	}

	// Verify PROMPT.txt was NOT created (agentMode=false)
	var count int
	err = fileDB.QueryRow(`SELECT COUNT(*) FROM nodes WHERE id = 'PROMPT.txt'`).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("PROMPT.txt should not exist when agentMode is false")
	}
}

func TestMaterializePromptTxt(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	schema := &api.Topology{Version: "v1"}

	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"

	_, err := db.Exec(`VACUUM INTO ?`, dbPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := materializeVirtuals(dbPath, schema, true); err != nil {
		t.Fatal(err)
	}

	fileDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fileDB.Close() }()

	var record string
	err = fileDB.QueryRow(`SELECT record FROM nodes WHERE id = 'PROMPT.txt'`).Scan(&record)
	if err != nil {
		t.Fatalf("PROMPT.txt not found: %v", err)
	}
	if record == "" {
		t.Fatal("PROMPT.txt should have content")
	}
}

func TestExtractFuncName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"functions/ProcessOrder/source", "ProcessOrder"},
		{"types/MyStruct/source", "MyStruct"},
		{"source", ""},
		{"a/b", "a"},
		{"", ""},
	}
	for _, tt := range tests {
		got := extractFuncName(tt.input)
		if got != tt.want {
			t.Errorf("extractFuncName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
