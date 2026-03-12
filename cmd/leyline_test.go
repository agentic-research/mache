package cmd

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
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
	dbPath := filepath.Join(tmpDir, "test.db")

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
	dbPath := filepath.Join(tmpDir, "test.db")

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

// --- Schema-driven content source tests ---

// setupTestDBWithLSP extends setupTestDB with _lsp_hover and _lsp tables.
func setupTestDBWithLSP(t *testing.T) *sql.DB {
	t.Helper()
	db := setupTestDB(t)
	_, err := db.Exec(`
		CREATE TABLE _lsp_hover (
			node_id TEXT PRIMARY KEY,
			hover_text TEXT
		);
		INSERT INTO _lsp_hover VALUES ('symbols/HandleRequest', 'func HandleRequest(w http.ResponseWriter, r *http.Request)');
		INSERT INTO _lsp_hover VALUES ('symbols/ProcessOrder', 'func ProcessOrder(ctx context.Context) error');

		CREATE TABLE _lsp (
			node_id TEXT,
			symbol_kind TEXT,
			detail TEXT,
			start_line INTEGER,
			start_col INTEGER,
			end_line INTEGER,
			end_col INTEGER,
			diagnostics TEXT
		);
		INSERT INTO _lsp VALUES ('symbols/HandleRequest', 'Function', NULL, 10, 0, 20, 1, '[{"message":"unused param w","severity":2}]');

		CREATE TABLE _lsp_defs (
			node_id TEXT,
			def_uri TEXT,
			def_start_line INTEGER,
			def_start_col INTEGER,
			def_end_line INTEGER,
			def_end_col INTEGER
		);
		INSERT INTO _lsp_defs VALUES ('symbols/HandleRequest', 'file:///src/server.go', 10, 5, 10, 20);

		CREATE TABLE _lsp_refs (
			node_id TEXT,
			ref_uri TEXT,
			ref_start_line INTEGER,
			ref_start_col INTEGER,
			ref_end_line INTEGER,
			ref_end_col INTEGER
		);
		INSERT INTO _lsp_refs VALUES ('symbols/HandleRequest', 'file:///src/main.go', 42, 3, 42, 17);
		INSERT INTO _lsp_refs VALUES ('symbols/HandleRequest', 'file:///src/test.go', 15, 5, 15, 19);
	`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestMaterializeContentSource_Hover(t *testing.T) {
	db := setupTestDBWithLSP(t)
	defer func() { _ = db.Close() }()

	schema := &api.Topology{
		Version: "v1",
		Nodes: []api.Node{
			{
				Name:     "functions",
				Selector: "$",
				Children: []api.Node{
					{
						Name:     "{{.name}}",
						Selector: "(function_declaration name: (identifier) @name) @scope",
						Files: []api.Leaf{
							{Name: "source", ContentTemplate: "{{.scope}}"},
							{Name: "hover", ContentSource: "lsp_hover"},
						},
					},
				},
			},
		},
	}

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	if _, err := db.Exec(`VACUUM INTO ?`, dbPath); err != nil {
		t.Fatal(err)
	}

	if err := materializeVirtuals(dbPath, schema, false); err != nil {
		t.Fatal(err)
	}

	fileDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fileDB.Close() }()

	// Verify hover file was created for HandleRequest
	var record string
	err = fileDB.QueryRow(`SELECT record FROM nodes WHERE id = 'functions/HandleRequest/hover'`).Scan(&record)
	if err != nil {
		t.Fatalf("hover file not found for HandleRequest: %v", err)
	}
	if record != "func HandleRequest(w http.ResponseWriter, r *http.Request)" {
		t.Fatalf("unexpected hover content: %q", record)
	}

	// Verify hover file was also created for ProcessOrder
	err = fileDB.QueryRow(`SELECT record FROM nodes WHERE id = 'functions/ProcessOrder/hover'`).Scan(&record)
	if err != nil {
		t.Fatalf("hover file not found for ProcessOrder: %v", err)
	}
	if record != "func ProcessOrder(ctx context.Context) error" {
		t.Fatalf("unexpected hover content: %q", record)
	}
}

func TestMaterializeContentSource_Diagnostics(t *testing.T) {
	db := setupTestDBWithLSP(t)
	defer func() { _ = db.Close() }()

	schema := &api.Topology{
		Version: "v1",
		Nodes: []api.Node{
			{
				Name:     "functions",
				Selector: "$",
				Children: []api.Node{
					{
						Name:     "{{.name}}",
						Selector: "(function_declaration name: (identifier) @name) @scope",
						Files: []api.Leaf{
							{Name: "source", ContentTemplate: "{{.scope}}"},
							{Name: "diagnostics", ContentSource: "lsp_diagnostics"},
						},
					},
				},
			},
		},
	}

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	if _, err := db.Exec(`VACUUM INTO ?`, dbPath); err != nil {
		t.Fatal(err)
	}

	if err := materializeVirtuals(dbPath, schema, false); err != nil {
		t.Fatal(err)
	}

	fileDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fileDB.Close() }()

	// HandleRequest has diagnostics
	var record string
	err = fileDB.QueryRow(`SELECT record FROM nodes WHERE id = 'functions/HandleRequest/diagnostics'`).Scan(&record)
	if err != nil {
		t.Fatalf("diagnostics file not found: %v", err)
	}
	if !json.Valid([]byte(record)) {
		t.Fatalf("diagnostics content is not valid JSON: %q", record)
	}

	// ProcessOrder has no diagnostics — file should NOT exist
	var count int
	err = fileDB.QueryRow(`SELECT COUNT(*) FROM nodes WHERE id = 'functions/ProcessOrder/diagnostics'`).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("ProcessOrder should not have diagnostics file (no diag data)")
	}
}

func TestMaterializeContentSource_Defs(t *testing.T) {
	db := setupTestDBWithLSP(t)
	defer func() { _ = db.Close() }()

	schema := &api.Topology{
		Version: "v1",
		Nodes: []api.Node{
			{
				Name:     "functions",
				Selector: "$",
				Children: []api.Node{
					{
						Name:     "{{.name}}",
						Selector: "(function_declaration name: (identifier) @name) @scope",
						Files: []api.Leaf{
							{Name: "source", ContentTemplate: "{{.scope}}"},
							{Name: "definitions", ContentSource: "lsp_defs"},
						},
					},
				},
			},
		},
	}

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	if _, err := db.Exec(`VACUUM INTO ?`, dbPath); err != nil {
		t.Fatal(err)
	}

	if err := materializeVirtuals(dbPath, schema, false); err != nil {
		t.Fatal(err)
	}

	fileDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fileDB.Close() }()

	var record string
	err = fileDB.QueryRow(`SELECT record FROM nodes WHERE id = 'functions/HandleRequest/definitions'`).Scan(&record)
	if err != nil {
		t.Fatalf("definitions file not found: %v", err)
	}
	if !json.Valid([]byte(record)) {
		t.Fatalf("definitions content is not valid JSON: %q", record)
	}
	// Should contain the def URI
	if !strings.Contains(record, "server.go") {
		t.Fatalf("expected definitions to reference server.go, got: %s", record)
	}
}

func TestMaterializeContentSource_Refs(t *testing.T) {
	db := setupTestDBWithLSP(t)
	defer func() { _ = db.Close() }()

	schema := &api.Topology{
		Version: "v1",
		Nodes: []api.Node{
			{
				Name:     "functions",
				Selector: "$",
				Children: []api.Node{
					{
						Name:     "{{.name}}",
						Selector: "(function_declaration name: (identifier) @name) @scope",
						Files: []api.Leaf{
							{Name: "source", ContentTemplate: "{{.scope}}"},
							{Name: "references", ContentSource: "lsp_refs"},
						},
					},
				},
			},
		},
	}

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	if _, err := db.Exec(`VACUUM INTO ?`, dbPath); err != nil {
		t.Fatal(err)
	}

	if err := materializeVirtuals(dbPath, schema, false); err != nil {
		t.Fatal(err)
	}

	fileDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fileDB.Close() }()

	var record string
	err = fileDB.QueryRow(`SELECT record FROM nodes WHERE id = 'functions/HandleRequest/references'`).Scan(&record)
	if err != nil {
		t.Fatalf("references file not found: %v", err)
	}
	if !json.Valid([]byte(record)) {
		t.Fatalf("references content is not valid JSON: %q", record)
	}
	// Should contain both ref URIs
	if !strings.Contains(record, "main.go") || !strings.Contains(record, "test.go") {
		t.Fatalf("expected references to contain main.go and test.go, got: %s", record)
	}
}

func TestMaterializeContentSource_NoTable(t *testing.T) {
	// If the backing table doesn't exist, content_source should be a graceful no-op
	db := setupTestDB(t) // no LSP tables
	defer func() { _ = db.Close() }()

	schema := &api.Topology{
		Version: "v1",
		Nodes: []api.Node{
			{
				Name:     "functions",
				Selector: "$",
				Children: []api.Node{
					{
						Name:     "{{.name}}",
						Selector: "(function_declaration name: (identifier) @name) @scope",
						Files: []api.Leaf{
							{Name: "hover", ContentSource: "lsp_hover"},
						},
					},
				},
			},
		},
	}

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	if _, err := db.Exec(`VACUUM INTO ?`, dbPath); err != nil {
		t.Fatal(err)
	}

	// Should not error
	if err := materializeVirtuals(dbPath, schema, false); err != nil {
		t.Fatal(err)
	}

	fileDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fileDB.Close() }()

	// No hover files should exist
	var count int
	err = fileDB.QueryRow(`SELECT COUNT(*) FROM nodes WHERE name = 'hover'`).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected no hover files when _lsp_hover table is absent, got %d", count)
	}
}

func TestMaterializeContentSource_NoSchemaDeclaration(t *testing.T) {
	// If schema has no content_source files, LSP tables are present but nothing
	// should be materialized (schema-driven = opt-in).
	db := setupTestDBWithLSP(t)
	defer func() { _ = db.Close() }()

	schema := &api.Topology{
		Version: "v1",
		Nodes: []api.Node{
			{
				Name:     "functions",
				Selector: "$",
				Children: []api.Node{
					{
						Name:     "{{.name}}",
						Selector: "(function_declaration name: (identifier) @name) @scope",
						Files: []api.Leaf{
							{Name: "source", ContentTemplate: "{{.scope}}"},
						},
					},
				},
			},
		},
	}

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	if _, err := db.Exec(`VACUUM INTO ?`, dbPath); err != nil {
		t.Fatal(err)
	}

	if err := materializeVirtuals(dbPath, schema, false); err != nil {
		t.Fatal(err)
	}

	fileDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fileDB.Close() }()

	// No hover or diagnostics files should be created
	var count int
	err = fileDB.QueryRow(`SELECT COUNT(*) FROM nodes WHERE name IN ('hover', 'diagnostics', 'definitions', 'references')`).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected no LSP files without content_source in schema, got %d", count)
	}
}

func TestMaterializeContentSource_WithInclude(t *testing.T) {
	db := setupTestDBWithLSP(t)
	defer func() { _ = db.Close() }()

	schema := &api.Topology{
		Version: "v1",
		FileSets: map[string][]api.Leaf{
			"lsp": {
				{Name: "hover", ContentSource: "lsp_hover"},
				{Name: "diagnostics", ContentSource: "lsp_diagnostics"},
			},
		},
		Nodes: []api.Node{
			{
				Name:     "functions",
				Selector: "$",
				Children: []api.Node{
					{
						Name:     "{{.name}}",
						Selector: "(function_declaration name: (identifier) @name) @scope",
						Include:  []string{"lsp"},
						Files: []api.Leaf{
							{Name: "source", ContentTemplate: "{{.scope}}"},
						},
					},
				},
			},
		},
	}

	// Resolve includes before materialization
	schema.ResolveIncludes()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	if _, err := db.Exec(`VACUUM INTO ?`, dbPath); err != nil {
		t.Fatal(err)
	}

	if err := materializeVirtuals(dbPath, schema, false); err != nil {
		t.Fatal(err)
	}

	fileDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fileDB.Close() }()

	// hover should exist (from include)
	var record string
	err = fileDB.QueryRow(`SELECT record FROM nodes WHERE id = 'functions/HandleRequest/hover'`).Scan(&record)
	if err != nil {
		t.Fatalf("hover file not found (via include): %v", err)
	}
	if record != "func HandleRequest(w http.ResponseWriter, r *http.Request)" {
		t.Fatalf("unexpected hover: %q", record)
	}

	// diagnostics should exist for HandleRequest (from include)
	err = fileDB.QueryRow(`SELECT record FROM nodes WHERE id = 'functions/HandleRequest/diagnostics'`).Scan(&record)
	if err != nil {
		t.Fatalf("diagnostics file not found (via include): %v", err)
	}
}

func TestResolveIncludes_ParseJSON(t *testing.T) {
	// Verify the full JSON round-trip works with file_sets + include
	raw := `{
		"version": "v1",
		"file_sets": {
			"lsp": [
				{"name": "hover", "content_source": "lsp_hover"},
				{"name": "diagnostics", "content_source": "lsp_diagnostics"}
			]
		},
		"nodes": [{
			"name": "functions",
			"selector": "$",
			"children": [{
				"name": "{{.name}}",
				"selector": "(function_declaration name: (identifier) @name) @scope",
				"include": ["lsp"],
				"files": [{"name": "source", "content_template": "{{.scope}}"}]
			}]
		}]
	}`

	var schema api.Topology
	if err := json.Unmarshal([]byte(raw), &schema); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Before resolve: 1 file (source)
	if len(schema.Nodes[0].Children[0].Files) != 1 {
		t.Fatalf("expected 1 file before resolve, got %d", len(schema.Nodes[0].Children[0].Files))
	}

	schema.ResolveIncludes()

	// After resolve: 3 files (source + hover + diagnostics)
	files := schema.Nodes[0].Children[0].Files
	if len(files) != 3 {
		t.Fatalf("expected 3 files after resolve, got %d", len(files))
	}
	if files[1].Name != "hover" || files[1].ContentSource != "lsp_hover" {
		t.Fatalf("unexpected second file: %+v", files[1])
	}
	if files[2].Name != "diagnostics" || files[2].ContentSource != "lsp_diagnostics" {
		t.Fatalf("unexpected third file: %+v", files[2])
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
