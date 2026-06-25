package ingest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/lang"
	sitter "github.com/smacker/go-tree-sitter"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// srcFile is one file in a multi-file parity fixture.
type srcFile struct {
	name string // base name, used as both filename and source_id
	src  []byte
}

// emitFileToASTDB parses one source file and appends its _source / _imports /
// nodes / _ast rows to an already-open db via the given prepared statements.
// Node IDs are rooted at the file's source_id (e.g. "b.go/function_declaration_0")
// — mirroring how ley-line keys node_ids per file, and crucially keeping IDs
// unique ACROSS files (the single-file emitter roots every tree at
// "source_file_0", which would collide as a primary key in a multi-file db).
func emitFileToASTDB(tb testing.TB, tx *sql.Tx, insNode, insAST *sql.Stmt, langName string, f srcFile) {
	tb.Helper()
	l := lang.ForName(langName)
	require.NotNil(tb, l, "unknown language %q", langName)

	parser := sitter.NewParser()
	parser.SetLanguage(l.Grammar())
	tree, err := parser.ParseCtx(context.Background(), nil, f.src)
	require.NoError(tb, err)
	defer tree.Close()

	sourceID := f.name

	// All writes go through the open tx — a db.Exec on the same connection
	// while the tx holds the write lock returns SQLITE_BUSY.
	_, err = tx.Exec("INSERT INTO _source (id, language, content, path) VALUES (?, ?, ?, NULL)",
		sourceID, langName, f.src)
	require.NoError(tb, err)

	if langName == "go" {
		sw := NewSitterWalker()
		imports := sw.ExtractGoImports(tree.RootNode(), f.src, l.Grammar())
		sw.Close()
		for alias, path := range imports {
			_, err = tx.Exec("INSERT INTO _imports (source_id, alias, path) VALUES (?, ?, ?)",
				sourceID, alias, path)
			require.NoError(tb, err)
		}
	}

	var emit func(n *sitter.Node, id, parentID string)
	emit = func(n *sitter.Node, id, parentID string) {
		hasNamedKids := n.NamedChildCount() > 0
		kindInt := 0
		record := ""
		if hasNamedKids {
			kindInt = 1
		} else {
			record = string(f.src[n.StartByte():n.EndByte()])
		}
		sp, ep := n.StartPoint(), n.EndPoint()
		_, err := insNode.Exec(id, parentID, n.Type(), kindInt, record, sourceID)
		require.NoError(tb, err)
		_, err = insAST.Exec(id, sourceID, n.Type(),
			int(n.StartByte()), int(n.EndByte()),
			int(sp.Row), int(sp.Column), int(ep.Row), int(ep.Column))
		require.NoError(tb, err)

		counts := map[string]int{}
		for i := 0; i < int(n.NamedChildCount()); i++ {
			c := n.NamedChild(i)
			seg := fmt.Sprintf("%s_%d", c.Type(), counts[c.Type()])
			counts[c.Type()]++
			emit(c, id+"/"+seg, id)
		}
	}
	// Root the file at its source_id so cross-file node_ids never collide.
	emit(tree.RootNode(), sourceID, "")
}

// sitterToASTDBMulti emits several source files into ONE _ast db — the
// multi-file analogue of sitterToASTDB. Each file is parsed independently and
// keyed by its base-name source_id, exactly as the engine's AST arm queries it
// (ASTRoot.SourceID = filepath.Base). Used to prove cross-file projection
// parity (callers/callees that span files).
func sitterToASTDBMulti(tb testing.TB, dbPath, langName string, files []srcFile) *sql.DB {
	tb.Helper()
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(tb, err)

	_, err = db.Exec(`
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY, parent_id TEXT, name TEXT NOT NULL,
			kind INTEGER NOT NULL, size INTEGER DEFAULT 0,
			mtime INTEGER NOT NULL, record_id TEXT, record JSON, source_file TEXT
		);
		CREATE TABLE _ast (
			node_id TEXT PRIMARY KEY, source_id TEXT NOT NULL, node_kind TEXT NOT NULL,
			start_byte INTEGER NOT NULL, end_byte INTEGER NOT NULL,
			start_row INTEGER, start_col INTEGER, end_row INTEGER, end_col INTEGER
		);
		CREATE INDEX idx_ast_source ON _ast(source_id);
		CREATE INDEX idx_ast_kind_source ON _ast(node_kind, source_id);
		CREATE INDEX idx_parent_name ON nodes(parent_id, name);
		CREATE TABLE _source (id TEXT PRIMARY KEY, language TEXT NOT NULL, content BLOB NOT NULL, path TEXT);
		CREATE TABLE _imports (source_id TEXT NOT NULL, alias TEXT NOT NULL, path TEXT NOT NULL);
	`)
	require.NoError(tb, err)

	tx, err := db.Begin()
	require.NoError(tb, err)
	insNode, err := tx.Prepare(`INSERT INTO nodes (id, parent_id, name, kind, mtime, record, source_file) VALUES (?, ?, ?, ?, 0, ?, ?)`)
	require.NoError(tb, err)
	insAST, err := tx.Prepare(`INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	require.NoError(tb, err)
	defer func() { _ = insNode.Close() }()
	defer func() { _ = insAST.Close() }()

	for _, f := range files {
		emitFileToASTDB(tb, tx, insNode, insAST, langName, f)
	}
	require.NoError(tb, tx.Commit())
	return db
}

// runQueryParityMulti ingests a DIRECTORY of source files through both the
// SitterWalker (CGO, parses live) and the ASTWalker (over a multi-file emitted
// _ast db) and asserts byte-level projection parity — including cross-file
// callers. Unlike runQueryParity (single file → single-file ingest path), this
// exercises ingestTreeSitterParallel (the parallel worker dir-walk) with the
// CGO parse SKIPPED on the AST side.
func runQueryParityMulti(t *testing.T, schemaPath, langName string, files []srcFile, requireCallers bool) graph.Graph {
	t.Helper()
	schema := loadParitySchema(t, schemaPath)

	srcDir := t.TempDir()
	for _, f := range files {
		require.NoError(t, os.WriteFile(filepath.Join(srcDir, f.name), f.src, 0o644))
	}

	// SitterWalker path: ingest the whole dir, parses live.
	sitterStore := graph.NewMemoryStore()
	require.NoError(t, NewEngine(&schema, sitterStore).Ingest(srcDir))

	// ASTWalker path: ingest the same dir, but with the CGO parse skipped —
	// queries the multi-file _ast db emitted from the same parses.
	db := sitterToASTDBMulti(t, filepath.Join(t.TempDir(), "ast.db"), langName, files)
	defer func() { _ = db.Close() }()
	astStore := graph.NewMemoryStore()
	astEngine := NewEngine(&schema, astStore)
	astEngine.SetASTWalker(NewASTWalker(db))
	require.NoError(t, astEngine.Ingest(srcDir))

	assertProjectionParity(t, sitterStore, astStore, requireCallers)
	return sitterStore
}

// TestASTQueryParity_MultiFile_Go proves the AST path produces identical
// projection across MULTIPLE files — including a CROSS-FILE caller edge (Bar in
// b.go calls Foo defined in a.go). This is the gap the single-file gate can't
// cover: no node_id collisions across files, per-file source_id scoping, the
// parallel-worker dir path with the parse skipped, and cross-file ref
// resolution. requireCallers=true guards against a vacuous (both-empty) pass.
func TestASTQueryParity_MultiFile_Go(t *testing.T) {
	files := []srcFile{
		{name: "a.go", src: []byte("package demo\n\nfunc Foo() int { return 1 }\n")},
		{name: "b.go", src: []byte("package demo\n\nfunc Bar() int { return Foo() }\n")},
	}
	store := runQueryParityMulti(t, "../../examples/go-schema.json", "go", files, true)

	// Explicit cross-file non-vacuity: Foo (a.go) must have Bar (b.go) as a
	// caller — the edge spans files, so this proves the AST path resolved a
	// real cross-file ref, not just that "some token had callers".
	callers := callerIDs(t, store, "Foo")
	require.NotEmpty(t, callers, "Foo must have a caller (the cross-file Bar→Foo edge)")
	var sawBar bool
	for _, c := range callers {
		if strings.Contains(c, "Bar") {
			sawBar = true
		}
	}
	require.True(t, sawBar, "Foo's callers must include Bar from b.go (cross-file edge); got %v", callers)
}
