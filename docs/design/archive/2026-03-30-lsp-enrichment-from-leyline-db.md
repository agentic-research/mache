# LSP Enrichment from Ley-line Pre-baked DBs

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make mache's LSP-powered MCP tools (`get_type_info`, `get_diagnostics`, `find_definition`) work from pre-baked `.db` files that ley-line produces — no runtime daemon needed.

**Architecture:** Ley-line's `ll-open/lsp` crate already projects LSP data into 5 SQLite tables (`_lsp`, `_lsp_hover`, `_lsp_defs`, `_lsp_refs`, `_lsp_completions`). Mache's `SQLiteGraph.QueryRefs()` already queries the main DB when `useNodesTable=true`. The work is: (1) generate a test fixture `.db` with AST + LSP tables, (2) extend `find_definition` to use `_lsp_defs` as a fallback, (3) extend `find_callers` to use `_lsp_refs` as a supplement, (4) end-to-end integration test.

**Tech Stack:** Go, SQLite (modernc.org/sqlite), mcp-go, Rust (ley-line fixture generation)

**Bead:** `mache-6060ab`

______________________________________________________________________

## File Structure

| File                            | Action | Responsibility                                                                                            |
| ------------------------------- | ------ | --------------------------------------------------------------------------------------------------------- |
| `cmd/serve_lsp.go`              | Modify | Add `_lsp_defs`/`_lsp_refs` query helpers                                                                 |
| `cmd/serve_handlers.go`         | Modify | Wire LSP fallback into `find_definition`, `find_callers`                                                  |
| `cmd/serve_test.go`             | Modify | Integration test with pre-baked fixture                                                                   |
| `testdata/lsp-fixture.db`       | Create | Pre-baked DB with `nodes` + `_ast` + `_source` + `_lsp` + `_lsp_hover` + `_lsp_defs` + `_lsp_refs` tables |
| `tools/gen-lsp-fixture/main.go` | Create | Go script to generate the fixture DB (pure Go, no ley-line runtime needed)                                |

The fixture generator builds the same tables ley-line would produce, using Go's `database/sql`. This avoids requiring Rust toolchain to run tests.

______________________________________________________________________

### Task 1: Generate LSP fixture DB

The fixture simulates what `ley-line` produces: a Go file parsed into `nodes` + `_ast` + `_source` tables (for ASTWalker), plus LSP enrichment in `_lsp` + `_lsp_hover` + `_lsp_defs` + `_lsp_refs`.

**Files:**

- Create: `tools/gen-lsp-fixture/main.go`

- Create: `testdata/lsp-fixture.db` (generated output)

- [ ] **Step 1: Write the fixture generator**

```go
// tools/gen-lsp-fixture/main.go
package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"

	_ "modernc.org/sqlite"
)

func main() {
	outPath := "testdata/lsp-fixture.db"
	if len(os.Args) > 1 {
		outPath = os.Args[1]
	}
	_ = os.Remove(outPath)

	db, err := sql.Open("sqlite", outPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// Schema: matches ley-line's leyline-schema + ll-open/ts + ll-open/lsp
	must(db.Exec(`
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY,
			parent_id TEXT,
			name TEXT NOT NULL,
			kind INTEGER NOT NULL,
			size INTEGER DEFAULT 0,
			mtime INTEGER NOT NULL,
			record_id TEXT,
			record JSON,
			source_file TEXT
		);
		CREATE INDEX idx_parent_name ON nodes(parent_id, name);

		CREATE TABLE _ast (
			node_id TEXT PRIMARY KEY,
			source_id TEXT NOT NULL,
			node_kind TEXT NOT NULL,
			start_byte INTEGER NOT NULL,
			end_byte INTEGER NOT NULL,
			start_row INTEGER NOT NULL,
			start_col INTEGER NOT NULL,
			end_row INTEGER NOT NULL,
			end_col INTEGER NOT NULL
		);
		CREATE INDEX idx_ast_source ON _ast(source_id);

		CREATE TABLE _source (
			id TEXT PRIMARY KEY,
			language TEXT NOT NULL,
			content BLOB NOT NULL
		);

		CREATE TABLE _lsp (
			node_id TEXT PRIMARY KEY,
			symbol_kind TEXT,
			detail TEXT,
			start_line INTEGER NOT NULL,
			start_col INTEGER NOT NULL,
			end_line INTEGER NOT NULL,
			end_col INTEGER NOT NULL,
			diagnostics TEXT
		);
		CREATE INDEX idx_lsp_kind ON _lsp(symbol_kind);

		CREATE TABLE _lsp_hover (
			node_id TEXT PRIMARY KEY,
			hover_text TEXT NOT NULL
		);

		CREATE TABLE _lsp_defs (
			node_id TEXT NOT NULL,
			def_uri TEXT NOT NULL,
			def_start_line INTEGER NOT NULL,
			def_start_col INTEGER NOT NULL,
			def_end_line INTEGER NOT NULL,
			def_end_col INTEGER NOT NULL
		);
		CREATE INDEX idx_lsp_defs_node ON _lsp_defs(node_id);

		CREATE TABLE _lsp_refs (
			node_id TEXT NOT NULL,
			ref_uri TEXT NOT NULL,
			ref_start_line INTEGER NOT NULL,
			ref_start_col INTEGER NOT NULL,
			ref_end_line INTEGER NOT NULL,
			ref_end_col INTEGER NOT NULL
		);
		CREATE INDEX idx_lsp_refs_node ON _lsp_refs(node_id);

		CREATE TABLE node_refs (
			token TEXT NOT NULL,
			node_id TEXT NOT NULL
		);
		CREATE INDEX idx_node_refs_token ON node_refs(token);
	`))

	src := []byte("package main\n\nimport \"fmt\"\n\nfunc Validate(x int) error {\n\treturn fmt.Errorf(\"invalid: %d\", x)\n}\n\nfunc Helper() string {\n\treturn \"ok\"\n}\n\ntype Config struct {\n\tName string\n}\n")
	must(db.Exec("INSERT INTO _source VALUES (?, ?, ?)", "main.go", "go", src))

	// nodes — root + source_file + functions + type
	nodes := [][5]any{
		{"", "", "", 1, ""},
		{"source_file", "", "source_file", 1, ""},
		{"source_file/function_declaration", "source_file", "function_declaration", 1, ""},
		{"source_file/function_declaration/identifier", "source_file/function_declaration", "identifier", 0, "Validate"},
		{"source_file/function_declaration/parameter_list", "source_file/function_declaration", "parameter_list", 1, ""},
		{"source_file/function_declaration/block", "source_file/function_declaration", "block", 1, ""},
		{"source_file/function_declaration_1", "source_file", "function_declaration_1", 1, ""},
		{"source_file/function_declaration_1/identifier", "source_file/function_declaration_1", "identifier", 0, "Helper"},
		{"source_file/function_declaration_1/parameter_list", "source_file/function_declaration_1", "parameter_list", 1, ""},
		{"source_file/function_declaration_1/block", "source_file/function_declaration_1", "block", 1, ""},
		{"source_file/type_declaration", "source_file", "type_declaration", 1, ""},
		{"source_file/type_declaration/type_spec", "source_file/type_declaration", "type_spec", 1, ""},
		{"source_file/type_declaration/type_spec/type_identifier", "source_file/type_declaration/type_spec", "type_identifier", 0, "Config"},
	}
	for _, n := range nodes {
		must(db.Exec("INSERT INTO nodes (id, parent_id, name, kind, size, mtime, record) VALUES (?,?,?,?,0,0,?)", n[0], n[1], n[2], n[3], n[4]))
	}

	// _ast rows
	astRows := [][4]any{
		{"source_file/function_declaration", "function_declaration", 28, 94},
		{"source_file/function_declaration/identifier", "identifier", 33, 41},
		{"source_file/function_declaration_1", "function_declaration", 96, 134},
		{"source_file/function_declaration_1/identifier", "identifier", 101, 107},
		{"source_file/type_declaration", "type_declaration", 136, 171},
		{"source_file/type_declaration/type_spec", "type_spec", 141, 171},
		{"source_file/type_declaration/type_spec/type_identifier", "type_identifier", 146, 152},
	}
	for _, a := range astRows {
		must(db.Exec("INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col) VALUES (?, 'main.go', ?, ?, ?, 0, 0, 0, 0)", a[0], a[1], a[2], a[3]))
	}

	// LSP enrichment — simulate what gopls would produce
	// _lsp: symbol metadata with line ranges
	must(db.Exec(`INSERT INTO _lsp VALUES ('source_file/function_declaration', 'function', 'func Validate(x int) error', 4, 0, 6, 1, NULL)`))
	must(db.Exec(`INSERT INTO _lsp VALUES ('source_file/function_declaration_1', 'function', 'func Helper() string', 8, 0, 10, 1, NULL)`))
	must(db.Exec(`INSERT INTO _lsp VALUES ('source_file/type_declaration', 'struct', 'type Config struct', 12, 0, 14, 1, NULL)`))

	// _lsp_hover: hover text per symbol
	must(db.Exec(`INSERT INTO _lsp_hover VALUES ('source_file/function_declaration', 'func Validate(x int) error')`))
	must(db.Exec(`INSERT INTO _lsp_hover VALUES ('source_file/function_declaration_1', 'func Helper() string')`))
	must(db.Exec(`INSERT INTO _lsp_hover VALUES ('source_file/type_declaration', 'type Config struct {\n\tName string\n}')`))

	// _lsp_defs: go-to-definition results
	must(db.Exec(`INSERT INTO _lsp_defs VALUES ('source_file/function_declaration', 'file:///main.go', 4, 5, 6, 1)`))
	must(db.Exec(`INSERT INTO _lsp_defs VALUES ('source_file/type_declaration', 'file:///main.go', 12, 5, 14, 1)`))

	// _lsp_refs: find-references results
	must(db.Exec(`INSERT INTO _lsp_refs VALUES ('source_file/function_declaration', 'file:///main_test.go', 10, 2, 10, 10)`))
	must(db.Exec(`INSERT INTO _lsp_refs VALUES ('source_file/function_declaration', 'file:///handler.go', 25, 8, 25, 16)`))
	must(db.Exec(`INSERT INTO _lsp_refs VALUES ('source_file/type_declaration', 'file:///config.go', 5, 10, 5, 16)`))

	// node_refs: cross-references for callers/ virtual dir
	must(db.Exec(`INSERT INTO node_refs VALUES ('Validate', 'source_file/function_declaration')`))
	must(db.Exec(`INSERT INTO node_refs VALUES ('Helper', 'source_file/function_declaration_1')`))
	must(db.Exec(`INSERT INTO node_refs VALUES ('Config', 'source_file/type_declaration')`))

	fmt.Printf("Generated %s\n", outPath)
}

func must(_ sql.Result, err error) {
	if err != nil {
		log.Fatal(err)
	}
}
```

- [ ] **Step 2: Generate the fixture**

Run: `go run tools/gen-lsp-fixture/main.go testdata/lsp-fixture.db`
Expected: `Generated testdata/lsp-fixture.db`

Verify: `sqlite3 testdata/lsp-fixture.db ".tables"` should show: `_ast _lsp _lsp_defs _lsp_hover _lsp_refs _source node_refs nodes`

- [ ] **Step 3: Commit**

```bash
git add tools/gen-lsp-fixture/main.go testdata/lsp-fixture.db
git commit -m "test: add LSP fixture DB for pre-baked ley-line integration"
```

______________________________________________________________________

### Task 2: Add `_lsp_defs` query helper to serve_lsp.go

Extend the LSP query layer to read `_lsp_defs` for definition lookups. This supplements the existing `DefsMap()` approach (which uses in-memory refs) with LSP-sourced definitions.

**Files:**

- Modify: `cmd/serve_lsp.go`

- Test: `cmd/serve_test.go`

- [ ] **Step 1: Write the failing test**

Add to `cmd/serve_test.go` after the existing `TestSQLiteGraphGoldenPath`:

```go
// TestLSPEnrichedDefinition verifies that find_definition returns LSP-sourced
// definition locations from _lsp_defs when the pre-baked DB has them.
func TestLSPEnrichedDefinition(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	repoRoot := filepath.Dir(filepath.Dir(thisFile))

	dbPath := filepath.Join(repoRoot, "testdata", "lsp-fixture.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Skip("testdata/lsp-fixture.db not found — run: go run tools/gen-lsp-fixture/main.go")
	}

	schemaBytes := []byte(`{"table":"nodes","nodes":[{"name":"functions","selector":"(function_declaration name: (identifier) @name) @scope","files":[{"name":"source","template":"{{.source}}"}]}]}`)
	var schema api.Topology
	require.NoError(t, json.Unmarshal(schemaBytes, &schema))

	sg, err := graph.OpenSQLiteGraph(dbPath, &schema, machetmpl.Render)
	require.NoError(t, err)
	defer func() { _ = sg.Close() }()
	require.NoError(t, sg.EagerScan())

	// Query LSP definitions for Validate
	handler := makeLSPDefinitionHandler(sg)
	result, err := handler(context.Background(), makeRequest(map[string]any{"symbol": "Validate"}))
	require.NoError(t, err)
	require.False(t, result.IsError, "unexpected error: %s", resultText(t, result))

	text := resultText(t, result)
	assert.Contains(t, text, "file:///main.go", "should contain definition URI")
	assert.Contains(t, text, "Validate", "should contain symbol name")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `task test -- -run TestLSPEnrichedDefinition -v`
Expected: FAIL — `makeLSPDefinitionHandler` undefined

- [ ] **Step 3: Add `queryLSPDefs` helper and `makeLSPDefinitionHandler`**

Add to `cmd/serve_lsp.go`:

```go
// queryLSPDefs returns definition locations from _lsp_defs for a symbol.
// The symbol is matched against node_id suffix (e.g., "Validate" matches
// "source_file/function_declaration" if the node's _ast has that identifier).
func queryLSPDefs(qg refsQuerier, symbol string) ([]lspDefLocation, error) {
	// Check if _lsp_defs table exists
	var tableExists int
	rows, err := qg.QueryRefs(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='_lsp_defs'`)
	if err != nil {
		return nil, err
	}
	if rows.Next() {
		_ = rows.Scan(&tableExists)
	}
	_ = rows.Close()
	if tableExists == 0 {
		return nil, nil
	}

	// Join _lsp_defs with nodes to match by symbol name.
	// The identifier node's record column contains the symbol text.
	query := `SELECT d.node_id, d.def_uri, d.def_start_line, d.def_start_col, d.def_end_line, d.def_end_col
		FROM _lsp_defs d
		JOIN nodes n ON n.parent_id = d.node_id AND n.record = ?
		JOIN _ast a ON a.node_id = n.id AND a.node_kind = 'identifier'`
	rows, err = qg.QueryRefs(query, symbol)
	if err != nil {
		// Fallback: suffix match on node_id
		rows, err = qg.QueryRefs(
			`SELECT node_id, def_uri, def_start_line, def_start_col, def_end_line, def_end_col
			 FROM _lsp_defs WHERE node_id LIKE ?`,
			"%"+escapeLikeMeta(symbol)+"%",
		)
		if err != nil {
			return nil, err
		}
	}

	var results []lspDefLocation
	for rows.Next() {
		var r lspDefLocation
		if err := rows.Scan(&r.NodeID, &r.URI, &r.StartLine, &r.StartCol, &r.EndLine, &r.EndCol); err != nil {
			continue
		}
		results = append(results, r)
	}
	_ = rows.Close()
	return results, nil
}

type lspDefLocation struct {
	NodeID    string `json:"node_id"`
	URI       string `json:"uri"`
	StartLine int    `json:"start_line"`
	StartCol  int    `json:"start_col"`
	EndLine   int    `json:"end_line"`
	EndCol    int    `json:"end_col"`
}

// queryLSPRefs returns reference locations from _lsp_refs for a symbol.
func queryLSPRefs(qg refsQuerier, symbol string) ([]lspRefLocation, error) {
	var tableExists int
	rows, err := qg.QueryRefs(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='_lsp_refs'`)
	if err != nil {
		return nil, err
	}
	if rows.Next() {
		_ = rows.Scan(&tableExists)
	}
	_ = rows.Close()
	if tableExists == 0 {
		return nil, nil
	}

	query := `SELECT d.node_id, d.ref_uri, d.ref_start_line, d.ref_start_col, d.ref_end_line, d.ref_end_col
		FROM _lsp_refs d
		JOIN nodes n ON n.parent_id = d.node_id AND n.record = ?
		JOIN _ast a ON a.node_id = n.id AND a.node_kind = 'identifier'`
	rows, err = qg.QueryRefs(query, symbol)
	if err != nil {
		rows, err = qg.QueryRefs(
			`SELECT node_id, ref_uri, ref_start_line, ref_start_col, ref_end_line, ref_end_col
			 FROM _lsp_refs WHERE node_id LIKE ?`,
			"%"+escapeLikeMeta(symbol)+"%",
		)
		if err != nil {
			return nil, err
		}
	}

	var results []lspRefLocation
	for rows.Next() {
		var r lspRefLocation
		if err := rows.Scan(&r.NodeID, &r.URI, &r.StartLine, &r.StartCol, &r.EndLine, &r.EndCol); err != nil {
			continue
		}
		results = append(results, r)
	}
	_ = rows.Close()
	return results, nil
}

type lspRefLocation struct {
	NodeID    string `json:"node_id"`
	URI       string `json:"uri"`
	StartLine int    `json:"start_line"`
	StartCol  int    `json:"start_col"`
	EndLine   int    `json:"end_line"`
	EndCol    int    `json:"end_col"`
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `task test -- -run TestLSPEnrichedDefinition -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add cmd/serve_lsp.go cmd/serve_test.go
git commit -m "feat: queryLSPDefs/queryLSPRefs — read ley-line LSP tables from pre-baked DBs"
```

______________________________________________________________________

### Task 3: Wire LSP fallback into `find_definition`

Extend the existing `makeFindDefinitionHandler` to fall back to `_lsp_defs` when the in-memory `DefsMap()` has no match.

**Files:**

- Modify: `cmd/serve_handlers.go:742-800`

- Test: `cmd/serve_test.go`

- [ ] **Step 1: Write the failing test**

Add to `cmd/serve_test.go`:

```go
// TestFindDefinition_LSPFallback verifies that find_definition uses _lsp_defs
// as a fallback when DefsMap() has no match (pre-baked DB path).
func TestFindDefinition_LSPFallback(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	repoRoot := filepath.Dir(filepath.Dir(thisFile))

	dbPath := filepath.Join(repoRoot, "testdata", "lsp-fixture.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Skip("testdata/lsp-fixture.db not found")
	}

	schemaBytes := []byte(`{"table":"nodes","nodes":[{"name":"functions","selector":"(function_declaration name: (identifier) @name) @scope","files":[{"name":"source","template":"{{.source}}"}]}]}`)
	var schema api.Topology
	require.NoError(t, json.Unmarshal(schemaBytes, &schema))

	sg, err := graph.OpenSQLiteGraph(dbPath, &schema, machetmpl.Render)
	require.NoError(t, err)
	defer func() { _ = sg.Close() }()
	require.NoError(t, sg.EagerScan())

	handler := makeFindDefinitionHandler(sg)
	result, err := handler(context.Background(), makeRequest(map[string]any{"symbol": "Validate"}))
	require.NoError(t, err)
	require.False(t, result.IsError, "unexpected error: %s", resultText(t, result))

	text := resultText(t, result)
	// Should contain LSP definition location
	assert.Contains(t, text, "file:///main.go", "should have LSP def URI")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `task test -- -run TestFindDefinition_LSPFallback -v`
Expected: FAIL — `find_definition` returns "no definition found" because `DefsMap()` is empty for SQLiteGraph without ingestion.

- [ ] **Step 3: Add LSP fallback to `makeFindDefinitionHandler`**

In `cmd/serve_handlers.go`, after the existing "no exact definition" block (~line 786), add the LSP fallback:

```go
			// Before returning "no definition found", try LSP-enriched defs
			if qg, ok := g.(refsQuerier); ok {
				lspDefs, err := queryLSPDefs(qg, symbol)
				if err == nil && len(lspDefs) > 0 {
					type lspDefResult struct {
						Symbol      string           `json:"symbol"`
						Source      string           `json:"source"`
						Definitions []lspDefLocation `json:"definitions"`
					}
					data, _ := json.MarshalIndent(lspDefResult{
						Symbol:      symbol,
						Source:      "lsp",
						Definitions: lspDefs,
					}, "", "  ")
					return mcp.NewToolResultText(string(data)), nil
				}
			}
```

Insert this right before the final `return mcp.NewToolResultText(fmt.Sprintf("no definition found for %q", symbol)), nil` at line ~787.

- [ ] **Step 4: Run test to verify it passes**

Run: `task test -- -run TestFindDefinition_LSPFallback -v`
Expected: PASS

- [ ] **Step 5: Run full test suite**

Run: `task test`
Expected: All existing tests still pass — the LSP fallback only triggers when DefsMap() misses.

- [ ] **Step 6: Commit**

```bash
git add cmd/serve_handlers.go cmd/serve_test.go
git commit -m "feat: find_definition falls back to _lsp_defs from ley-line pre-baked DBs"
```

______________________________________________________________________

### Task 4: Wire LSP refs into `find_callers` supplement

Extend `find_callers` to include `_lsp_refs` locations alongside existing cross-reference results.

**Files:**

- Modify: `cmd/serve_handlers.go` (find_callers handler area)

- Test: `cmd/serve_test.go`

- [ ] **Step 1: Write the failing test**

```go
// TestFindCallers_LSPRefs verifies that find_callers includes _lsp_refs
// locations from ley-line pre-baked DBs.
func TestFindCallers_LSPRefs(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	repoRoot := filepath.Dir(filepath.Dir(thisFile))

	dbPath := filepath.Join(repoRoot, "testdata", "lsp-fixture.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Skip("testdata/lsp-fixture.db not found")
	}

	schemaBytes := []byte(`{"table":"nodes","nodes":[{"name":"functions","selector":"(function_declaration name: (identifier) @name) @scope","files":[{"name":"source","template":"{{.source}}"}]}]}`)
	var schema api.Topology
	require.NoError(t, json.Unmarshal(schemaBytes, &schema))

	sg, err := graph.OpenSQLiteGraph(dbPath, &schema, machetmpl.Render)
	require.NoError(t, err)
	defer func() { _ = sg.Close() }()
	require.NoError(t, sg.EagerScan())

	handler := makeFindCallersHandler(sg)
	result, err := handler(context.Background(), makeRequest(map[string]any{"symbol": "Validate"}))
	require.NoError(t, err)
	require.False(t, result.IsError, "unexpected error: %s", resultText(t, result))

	text := resultText(t, result)
	// Validate has 2 LSP refs: main_test.go and handler.go
	assert.Contains(t, text, "main_test.go", "should have LSP ref from main_test.go")
	assert.Contains(t, text, "handler.go", "should have LSP ref from handler.go")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `task test -- -run TestFindCallers_LSPRefs -v`
Expected: FAIL — `find_callers` doesn't check `_lsp_refs` yet.

- [ ] **Step 3: Add LSP refs supplement to find_callers handler**

In `cmd/serve_handlers.go`, find the `makeFindCallersHandler` function. After the existing callers lookup, add:

```go
		// Supplement with LSP references if available
		if qg, ok := g.(refsQuerier); ok {
			lspRefs, err := queryLSPRefs(qg, symbol)
			if err == nil && len(lspRefs) > 0 {
				for _, ref := range lspRefs {
					callerEntries = append(callerEntries, callerEntry{
						Path:   ref.URI,
						Source: "lsp",
						Line:   ref.StartLine,
					})
				}
			}
		}
```

The exact integration point depends on the current `makeFindCallersHandler` structure — read it and insert after the graph-based callers are collected, before the JSON response is assembled.

- [ ] **Step 4: Run test to verify it passes**

Run: `task test -- -run TestFindCallers_LSPRefs -v`
Expected: PASS

- [ ] **Step 5: Run full test suite**

Run: `task test`
Expected: All pass.

- [ ] **Step 6: Commit**

```bash
git add cmd/serve_handlers.go cmd/serve_test.go
git commit -m "feat: find_callers supplements with _lsp_refs from ley-line pre-baked DBs"
```

______________________________________________________________________

### Task 5: End-to-end integration test

Full pipeline test: fixture DB → SQLiteGraph + ASTWalker → all LSP-enriched MCP tools.

**Files:**

- Modify: `cmd/serve_test.go`

- [ ] **Step 1: Write the integration test**

```go
// TestLSPPrebakedEndToEnd exercises the complete pre-baked ley-line pipeline:
// fixture DB (nodes + _ast + _source + _lsp* tables) → SQLiteGraph →
// ASTWalker → all LSP-enriched MCP tools. No runtime daemon needed.
func TestLSPPrebakedEndToEnd(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	repoRoot := filepath.Dir(filepath.Dir(thisFile))

	dbPath := filepath.Join(repoRoot, "testdata", "lsp-fixture.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Skip("testdata/lsp-fixture.db not found")
	}

	schemaBytes := []byte(`{"table":"nodes","nodes":[{"name":"functions","selector":"(function_declaration name: (identifier) @name) @scope","files":[{"name":"source","template":"{{.source}}"}]}]}`)
	var schema api.Topology
	require.NoError(t, json.Unmarshal(schemaBytes, &schema))

	sg, err := graph.OpenSQLiteGraph(dbPath, &schema, machetmpl.Render)
	require.NoError(t, err)
	defer func() { _ = sg.Close() }()
	require.NoError(t, sg.EagerScan())

	t.Run("get_type_info", func(t *testing.T) {
		handler := makeGetTypeInfoHandler(sg)
		result, err := handler(context.Background(), makeRequest(map[string]any{"symbol": "Validate"}))
		require.NoError(t, err)
		require.False(t, result.IsError, resultText(t, result))
		assert.Contains(t, resultText(t, result), "func Validate(x int) error")
	})

	t.Run("get_diagnostics", func(t *testing.T) {
		handler := makeGetDiagnosticsHandler(sg)
		result, err := handler(context.Background(), makeRequest(map[string]any{}))
		require.NoError(t, err)
		// No diagnostics in fixture — should return clean
		require.False(t, result.IsError, resultText(t, result))
	})

	t.Run("find_definition_lsp", func(t *testing.T) {
		handler := makeFindDefinitionHandler(sg)
		result, err := handler(context.Background(), makeRequest(map[string]any{"symbol": "Validate"}))
		require.NoError(t, err)
		require.False(t, result.IsError, resultText(t, result))
		assert.Contains(t, resultText(t, result), "file:///main.go")
	})

	t.Run("find_callers_lsp", func(t *testing.T) {
		handler := makeFindCallersHandler(sg)
		result, err := handler(context.Background(), makeRequest(map[string]any{"symbol": "Validate"}))
		require.NoError(t, err)
		require.False(t, result.IsError, resultText(t, result))
		text := resultText(t, result)
		assert.Contains(t, text, "main_test.go")
		assert.Contains(t, text, "handler.go")
	})

	t.Run("get_overview", func(t *testing.T) {
		handler := makeGetOverviewHandler(sg)
		result, err := handler(context.Background(), makeRequest(map[string]any{}))
		require.NoError(t, err)
		require.False(t, result.IsError, resultText(t, result))
		assert.NotEmpty(t, resultText(t, result))
	})
}
```

- [ ] **Step 2: Run to verify it passes**

Run: `task test -- -run TestLSPPrebakedEndToEnd -v`
Expected: All subtests PASS.

- [ ] **Step 3: Run full suite**

Run: `task test`
Expected: All pass.

- [ ] **Step 4: Commit**

```bash
git add cmd/serve_test.go
git commit -m "test: end-to-end LSP pre-baked pipeline — fixture DB → SQLiteGraph → MCP tools"
```

______________________________________________________________________

### Task 6: Update bead and docs

- [ ] **Step 1: Comment on bead with what was done**

```bash
rsry bead comment mache-6060ab "LSP pre-baked path implemented: mache reads _lsp_defs/_lsp_refs from ley-line DBs. find_definition falls back to LSP defs, find_callers supplements with LSP refs. E2E test with fixture DB."
```

- [ ] **Step 2: Update CLAUDE.md if needed**

Add a bullet under Key Design Details:

```
- **LSP enrichment**: When a `.db` has `_lsp*` tables (produced by ley-line's `ll-open/lsp`), `find_definition` falls back to `_lsp_defs` and `find_callers` supplements with `_lsp_refs`. No runtime daemon needed — pre-baked at build time.
```

- [ ] **Step 3: Commit and close bead**

```bash
git add CLAUDE.md
git commit -m "docs: document LSP enrichment from pre-baked ley-line DBs"
rsry bead close mache-6060ab
```
