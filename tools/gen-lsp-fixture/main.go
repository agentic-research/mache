// gen-lsp-fixture generates a SQLite test fixture database that simulates
// the output of ley-line's ll-open/ts and ll-open/lsp pipelines.
//
// The fixture contains a small Go package with:
//   - 2 functions: Validate(x int) error, Helper() string
//   - 1 type: Config struct { Name string }
//   - Full LSP metadata: hover text, definition locations, reference locations
//   - node_refs entries for mache's callers/ virtual directory
//
// Table schemas match the ley-line contract exactly:
//   - nodes, _ast, _source  (ll-open/ts)
//   - _lsp, _lsp_hover, _lsp_defs, _lsp_refs  (ll-open/lsp)
//   - node_refs  (mache cross-ref index)
//
// Usage:
//
//	go run ./tools/gen-lsp-fixture -o testdata/lsp-fixture.db
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

func main() {
	outPath := flag.String("o", "testdata/lsp-fixture.db", "Output SQLite database path")
	flag.Parse()

	if err := run(*outPath); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %s\n", *outPath)
}

func run(outPath string) error {
	// Remove stale file so we always produce a clean fixture.
	if err := os.Remove(outPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove old fixture: %w", err)
	}

	db, err := sql.Open("sqlite", outPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	db.SetMaxOpenConns(1)

	if err := createSchema(db); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	if err := insertFixture(db); err != nil {
		return fmt.Errorf("insert fixture: %w", err)
	}
	return nil
}

// createSchema creates all tables required by the ley-line contract plus
// mache's own node_refs cross-reference index.
func createSchema(db *sql.DB) error {
	stmts := []string{
		// ley-line core node tree (leyline-schema)
		`CREATE TABLE nodes (
			id          TEXT PRIMARY KEY,
			parent_id   TEXT,
			name        TEXT NOT NULL,
			kind        INTEGER NOT NULL,
			size        INTEGER DEFAULT 0,
			mtime       INTEGER NOT NULL,
			record_id   TEXT,
			record      JSON,
			source_file TEXT
		)`,

		// ll-open/ts: AST node positions
		`CREATE TABLE _ast (
			node_id    TEXT PRIMARY KEY,
			source_id  TEXT NOT NULL,
			node_kind  TEXT NOT NULL,
			start_byte INTEGER NOT NULL,
			end_byte   INTEGER NOT NULL,
			start_row  INTEGER NOT NULL,
			start_col  INTEGER NOT NULL,
			end_row    INTEGER NOT NULL,
			end_col    INTEGER NOT NULL
		)`,

		// ll-open/ts: source file content indexed by language
		`CREATE TABLE _source (
			id       TEXT PRIMARY KEY,
			language TEXT NOT NULL,
			content  BLOB NOT NULL
		)`,

		// ll-open/lsp: symbol metadata (kind, signature, position span, diagnostics)
		`CREATE TABLE _lsp (
			node_id     TEXT PRIMARY KEY,
			symbol_kind TEXT,
			detail      TEXT,
			start_line  INTEGER NOT NULL,
			start_col   INTEGER NOT NULL,
			end_line    INTEGER NOT NULL,
			end_col     INTEGER NOT NULL,
			diagnostics TEXT
		)`,

		// ll-open/lsp: markdown hover documentation
		`CREATE TABLE _lsp_hover (
			node_id    TEXT PRIMARY KEY,
			hover_text TEXT NOT NULL
		)`,

		// ll-open/lsp: go-to-definition targets
		`CREATE TABLE _lsp_defs (
			node_id        TEXT NOT NULL,
			def_uri        TEXT NOT NULL,
			def_start_line INTEGER NOT NULL,
			def_start_col  INTEGER NOT NULL,
			def_end_line   INTEGER NOT NULL,
			def_end_col    INTEGER NOT NULL
		)`,

		// ll-open/lsp: find-references results
		`CREATE TABLE _lsp_refs (
			node_id        TEXT NOT NULL,
			ref_uri        TEXT NOT NULL,
			ref_start_line INTEGER NOT NULL,
			ref_start_col  INTEGER NOT NULL,
			ref_end_line   INTEGER NOT NULL,
			ref_end_col    INTEGER NOT NULL
		)`,

		// mache cross-ref index: token → node_id (callers/ virtual directory)
		`CREATE TABLE node_refs (
			token   TEXT NOT NULL,
			node_id TEXT NOT NULL
		)`,

		`CREATE INDEX idx_node_refs_token ON node_refs (token)`,
		`CREATE INDEX idx_lsp_defs_node   ON _lsp_defs (node_id)`,
		`CREATE INDEX idx_lsp_refs_node   ON _lsp_refs (node_id)`,
		`CREATE INDEX idx_ast_source      ON _ast (source_id)`,
	}

	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("exec %q: %w", s[:min(40, len(s))], err)
		}
	}
	return nil
}

// insertFixture populates all tables with a realistic Go package snapshot.
//
// Simulated package layout:
//
//	mypackage/
//	  Validate/       (function: Validate(x int) error)
//	    source        (Go source text)
//	  Helper/         (function: Helper() string)
//	    source
//	  Config/         (type: Config struct { Name string })
//	    source
func insertFixture(db *sql.DB) error {
	now := time.Now().UnixNano()

	// Source file content (single file containing all three symbols)
	sourceContent := `package mypackage

import "errors"

// Config holds the application configuration.
type Config struct {
	Name string
}

// Validate checks that x is positive.
func Validate(x int) error {
	if x <= 0 {
		return errors.New("x must be positive")
	}
	return nil
}

// Helper returns a greeting string.
func Helper() string {
	return "hello"
}
`
	sourceID := "mypackage/mypackage.go"

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// ------------------------------------------------------------------
	// nodes table: directory tree
	// kind=1 → directory, kind=0 → file
	// ------------------------------------------------------------------
	nodesData := []struct {
		id, parentID, name string
		kind               int
		size               int
		record             string
		sourceFile         string
	}{
		// Root package directory
		{"mypackage", "", "mypackage", 1, 0, "", ""},

		// Validate function directory
		{"mypackage/Validate", "mypackage", "Validate", 1, 0, `{"lang":"go","pkg":"mypackage"}`, ""},
		// Validate source file (inline content in record column)
		{
			"mypackage/Validate/source", "mypackage/Validate", "source", 0,
			len("func Validate(x int) error {\n\tif x <= 0 {\n\t\treturn errors.New(\"x must be positive\")\n\t}\n\treturn nil\n}\n"),
			"func Validate(x int) error {\n\tif x <= 0 {\n\t\treturn errors.New(\"x must be positive\")\n\t}\n\treturn nil\n}\n",
			sourceID,
		},

		// Helper function directory
		{"mypackage/Helper", "mypackage", "Helper", 1, 0, `{"lang":"go","pkg":"mypackage"}`, ""},
		// Helper source file
		{
			"mypackage/Helper/source", "mypackage/Helper", "source", 0,
			len("func Helper() string {\n\treturn \"hello\"\n}\n"),
			"func Helper() string {\n\treturn \"hello\"\n}\n",
			sourceID,
		},

		// Config type directory
		{"mypackage/Config", "mypackage", "Config", 1, 0, `{"lang":"go","pkg":"mypackage"}`, ""},
		// Config source file
		{
			"mypackage/Config/source", "mypackage/Config", "source", 0,
			len("type Config struct {\n\tName string\n}\n"),
			"type Config struct {\n\tName string\n}\n",
			sourceID,
		},
	}

	for _, n := range nodesData {
		var parentID, record, sf interface{}
		if n.parentID != "" {
			parentID = n.parentID
		}
		if n.record != "" {
			record = n.record
		}
		if n.sourceFile != "" {
			sf = n.sourceFile
		}
		_, err = tx.Exec(
			`INSERT INTO nodes (id, parent_id, name, kind, size, mtime, record_id, record, source_file)
			 VALUES (?, ?, ?, ?, ?, ?, NULL, ?, ?)`,
			n.id, parentID, n.name, n.kind, n.size, now, record, sf,
		)
		if err != nil {
			return fmt.Errorf("insert node %s: %w", n.id, err)
		}
	}

	// ------------------------------------------------------------------
	// _source table: raw source content indexed by file path
	// ------------------------------------------------------------------
	_, err = tx.Exec(
		`INSERT INTO _source (id, language, content) VALUES (?, ?, ?)`,
		sourceID, "go", sourceContent,
	)
	if err != nil {
		return fmt.Errorf("insert _source: %w", err)
	}

	// ------------------------------------------------------------------
	// _ast table: AST node positions within the source file
	//
	// Byte offsets and row/col are approximate but structurally valid.
	// Line numbering is 0-based (tree-sitter convention).
	// ------------------------------------------------------------------
	astData := []struct {
		nodeID                             string
		nodeKind                           string
		startByte, endByte                 int
		startRow, startCol, endRow, endCol int
	}{
		// Config type declaration: "type Config struct { Name string }" at ~line 6
		{"mypackage/Config", "type_declaration", 67, 101, 5, 0, 8, 1},
		// Validate function declaration: lines 10-15
		{"mypackage/Validate", "function_declaration", 103, 197, 9, 0, 15, 1},
		// Helper function declaration: lines 17-21
		{"mypackage/Helper", "function_declaration", 199, 239, 16, 0, 20, 1},
	}

	for _, a := range astData {
		_, err = tx.Exec(
			`INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.nodeID, sourceID, a.nodeKind,
			a.startByte, a.endByte,
			a.startRow, a.startCol, a.endRow, a.endCol,
		)
		if err != nil {
			return fmt.Errorf("insert _ast %s: %w", a.nodeID, err)
		}
	}

	// ------------------------------------------------------------------
	// _lsp table: symbol metadata per node
	// ------------------------------------------------------------------
	lspData := []struct {
		nodeID, symbolKind, detail string
		startLine, startCol        int
		endLine, endCol            int
		diagnostics                string
	}{
		{
			nodeID: "mypackage/Validate", symbolKind: "Function",
			detail:    "func Validate(x int) error",
			startLine: 10, startCol: 0, endLine: 15, endCol: 1,
			diagnostics: "",
		},
		{
			nodeID: "mypackage/Helper", symbolKind: "Function",
			detail:    "func Helper() string",
			startLine: 17, startCol: 0, endLine: 20, endCol: 1,
			diagnostics: "",
		},
		{
			nodeID: "mypackage/Config", symbolKind: "Struct",
			detail:    "type Config struct",
			startLine: 6, startCol: 0, endLine: 8, endCol: 1,
			diagnostics: "",
		},
	}

	for _, l := range lspData {
		var diag interface{}
		if l.diagnostics != "" {
			diag = l.diagnostics
		}
		_, err = tx.Exec(
			`INSERT INTO _lsp (node_id, symbol_kind, detail, start_line, start_col, end_line, end_col, diagnostics)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			l.nodeID, l.symbolKind, l.detail,
			l.startLine, l.startCol, l.endLine, l.endCol, diag,
		)
		if err != nil {
			return fmt.Errorf("insert _lsp %s: %w", l.nodeID, err)
		}
	}

	// ------------------------------------------------------------------
	// _lsp_hover table: markdown hover documentation
	// ------------------------------------------------------------------
	hoverData := []struct {
		nodeID, hoverText string
	}{
		{
			"mypackage/Validate",
			"```go\nfunc Validate(x int) error\n```\n\nValidate checks that x is positive.\nReturns an error if x <= 0.",
		},
		{
			"mypackage/Helper",
			"```go\nfunc Helper() string\n```\n\nHelper returns a greeting string.",
		},
		{
			"mypackage/Config",
			"```go\ntype Config struct {\n    Name string\n}\n```\n\nConfig holds the application configuration.",
		},
	}

	for _, h := range hoverData {
		_, err = tx.Exec(
			`INSERT INTO _lsp_hover (node_id, hover_text) VALUES (?, ?)`,
			h.nodeID, h.hoverText,
		)
		if err != nil {
			return fmt.Errorf("insert _lsp_hover %s: %w", h.nodeID, err)
		}
	}

	// ------------------------------------------------------------------
	// _lsp_defs table: go-to-definition locations
	// (Validate and Config have definition entries; Helper omitted to
	//  verify graceful absence in tests)
	// ------------------------------------------------------------------
	defsData := []struct {
		nodeID, defURI            string
		defStartLine, defStartCol int
		defEndLine, defEndCol     int
	}{
		{
			nodeID: "mypackage/Validate", defURI: "file:///project/mypackage/mypackage.go",
			defStartLine: 10, defStartCol: 0, defEndLine: 15, defEndCol: 1,
		},
		{
			nodeID: "mypackage/Config", defURI: "file:///project/mypackage/mypackage.go",
			defStartLine: 6, defStartCol: 0, defEndLine: 8, defEndCol: 1,
		},
	}

	for _, d := range defsData {
		_, err = tx.Exec(
			`INSERT INTO _lsp_defs (node_id, def_uri, def_start_line, def_start_col, def_end_line, def_end_col)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			d.nodeID, d.defURI, d.defStartLine, d.defStartCol, d.defEndLine, d.defEndCol,
		)
		if err != nil {
			return fmt.Errorf("insert _lsp_defs %s: %w", d.nodeID, err)
		}
	}

	// ------------------------------------------------------------------
	// _lsp_refs table: find-references results
	//   - Validate: referenced from main_test.go:10 and handler.go:25
	//   - Config:   referenced from config.go:5
	//   - Helper:   no refs (tests graceful absence)
	// ------------------------------------------------------------------
	refsData := []struct {
		nodeID, refURI            string
		refStartLine, refStartCol int
		refEndLine, refEndCol     int
	}{
		{
			nodeID: "mypackage/Validate", refURI: "file:///project/main_test.go",
			refStartLine: 10, refStartCol: 4, refEndLine: 10, refEndCol: 12,
		},
		{
			nodeID: "mypackage/Validate", refURI: "file:///project/handler.go",
			refStartLine: 25, refStartCol: 8, refEndLine: 25, refEndCol: 16,
		},
		{
			nodeID: "mypackage/Config", refURI: "file:///project/config.go",
			refStartLine: 5, refStartCol: 6, refEndLine: 5, refEndCol: 12,
		},
	}

	for _, r := range refsData {
		_, err = tx.Exec(
			`INSERT INTO _lsp_refs (node_id, ref_uri, ref_start_line, ref_start_col, ref_end_line, ref_end_col)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			r.nodeID, r.refURI, r.refStartLine, r.refStartCol, r.refEndLine, r.refEndCol,
		)
		if err != nil {
			return fmt.Errorf("insert _lsp_refs %s: %w", r.nodeID, err)
		}
	}

	// ------------------------------------------------------------------
	// node_refs table: token → node_id for callers/ virtual directory
	//
	// Tokens match the directory name (symbol name) so mache's GetCallers
	// resolves them correctly.
	// ------------------------------------------------------------------
	nodeRefsData := []struct {
		token, nodeID string
	}{
		// Validate is called from pkg/main and pkg/handler
		{"Validate", "mypackage/Validate"},
		// Config is referenced from pkg/config
		{"Config", "mypackage/Config"},
		// Helper is not referenced (tests empty callers/)
	}

	for _, nr := range nodeRefsData {
		_, err = tx.Exec(
			`INSERT INTO node_refs (token, node_id) VALUES (?, ?)`,
			nr.token, nr.nodeID,
		)
		if err != nil {
			return fmt.Errorf("insert node_refs %s->%s: %w", nr.token, nr.nodeID, err)
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
