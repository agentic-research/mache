// Phase 4 tests: chunks-as-parse-outputs round-trip.
//
// When the source db has an `_ast` table, mache push emits chunks
// containing source content + AST node rows; mache pull restores
// both `_source` AND `_ast` byte-equal.
//
// Phase 1 fallback (chunk = raw content) is exercised by the
// existing cache_test.go tests, which don't create `_ast`. These
// tests explicitly create `_ast` so the AST path runs.

package cmd

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"testing"

	_ "modernc.org/sqlite"
)

// synthAstNode is one synthetic _ast row to seed.
type synthAstNode struct {
	nodeID, sourceID, nodeKind         string
	startByte, endByte                 int64
	startRow, startCol, endRow, endCol int64
}

// makeSyntheticDBWithAST creates a SQLite db at dbPath with _source
// + _ast populated. The _ast schema matches mache's ingest pipeline
// (per internal/ingest/ast_walker_test.go).
func makeSyntheticDBWithAST(t *testing.T, dbPath string, sources []synthSource, nodes []synthAstNode) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TABLE _source (
		id TEXT PRIMARY KEY, path TEXT, language TEXT, content BLOB
	)`); err != nil {
		t.Fatalf("create _source: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE _ast (
		node_id TEXT PRIMARY KEY,
		source_id TEXT NOT NULL,
		node_kind TEXT NOT NULL,
		start_byte INTEGER NOT NULL,
		end_byte INTEGER NOT NULL,
		start_row INTEGER,
		start_col INTEGER,
		end_row INTEGER,
		end_col INTEGER
	)`); err != nil {
		t.Fatalf("create _ast: %v", err)
	}

	srcStmt, _ := db.Prepare("INSERT INTO _source(id, path, language, content) VALUES(?,?,?,?)")
	defer func() { _ = srcStmt.Close() }()
	for _, s := range sources {
		if _, err := srcStmt.Exec(s.id, s.path, s.language, s.content); err != nil {
			t.Fatalf("insert _source %s: %v", s.id, err)
		}
	}

	astStmt, _ := db.Prepare(`INSERT INTO _ast
		(node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col)
		VALUES (?,?,?,?,?,?,?,?,?)`)
	defer func() { _ = astStmt.Close() }()
	for _, n := range nodes {
		if _, err := astStmt.Exec(n.nodeID, n.sourceID, n.nodeKind,
			n.startByte, n.endByte, n.startRow, n.startCol, n.endRow, n.endCol); err != nil {
			t.Fatalf("insert _ast %s: %v", n.nodeID, err)
		}
	}
}

// readBackAstNodes returns _ast rows in stable order (source_id, node_id).
func readBackAstNodes(t *testing.T, dbPath string) []synthAstNode {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT node_id, source_id, node_kind, start_byte, end_byte,
		start_row, start_col, end_row, end_col FROM _ast ORDER BY source_id, node_id`)
	if err != nil {
		t.Fatalf("query _ast: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []synthAstNode
	for rows.Next() {
		var n synthAstNode
		var sr, sc, er, ec sql.NullInt64
		if err := rows.Scan(&n.nodeID, &n.sourceID, &n.nodeKind,
			&n.startByte, &n.endByte, &sr, &sc, &er, &ec); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n.startRow = sr.Int64
		n.startCol = sc.Int64
		n.endRow = er.Int64
		n.endCol = ec.Int64
		out = append(out, n)
	}
	return out
}

// ── push: AST table detected ──────────────────────────────────────

func TestCacheAST_PushDetectsASTTable(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "input.db")
	outDir := filepath.Join(tmp, "out")

	makeSyntheticDBWithAST(t,
		dbPath,
		[]synthSource{
			{id: "a.go", path: "a.go", language: "go", content: []byte("package a\n\nfunc A() {}\n")},
		},
		[]synthAstNode{
			{
				nodeID: "a.go/function_declaration", sourceID: "a.go", nodeKind: "function_declaration",
				startByte: 11, endByte: 23, startRow: 2, startCol: 0, endRow: 2, endCol: 12,
			},
			{
				nodeID: "a.go/function_declaration/identifier", sourceID: "a.go", nodeKind: "identifier",
				startByte: 16, endByte: 17, startRow: 2, startCol: 5, endRow: 2, endCol: 6,
			},
		},
	)

	var buf bytes.Buffer
	if err := runCachePush(&buf, dbPath, outDir); err != nil {
		t.Fatalf("push: %v\n%s", err, buf.String())
	}

	// The chunk file should be JSON (Phase 4 shape), not raw content.
	// Reach in via the lockfile to find the chunk path, then assert.
	chunksDir := filepath.Join(outDir, "objects")
	// Find any chunk file under chunksDir/*/* and check its shape.
	bucketEntries, _ := readDirAll(t, chunksDir)
	if len(bucketEntries) == 0 {
		t.Fatalf("no chunk files written")
	}
	var anyChunk []byte
	for _, p := range bucketEntries {
		body, err := readFileBytes(t, p)
		if err != nil {
			continue
		}
		anyChunk = body
		break
	}
	if !chunkBodyIsASTShape(anyChunk) {
		t.Errorf("Phase 4 expected JSON-shape chunk; first bytes: %.40q", string(anyChunk))
	}
}

// ── push + pull: full _source + _ast round-trip ───────────────────

func TestCacheAST_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "input.db")
	outDir := filepath.Join(tmp, "out")
	restoredPath := filepath.Join(tmp, "restored.db")

	srcs := []synthSource{
		{id: "main.go", path: "main.go", language: "go", content: []byte("package main\nfunc main() { auth.Validate(\"x\") }\n")},
		{id: "auth.go", path: "auth.go", language: "go", content: []byte("package auth\nfunc Validate(s string) error { return nil }\n")},
	}
	nodes := []synthAstNode{
		{
			nodeID: "auth.go/function_declaration", sourceID: "auth.go", nodeKind: "function_declaration",
			startByte: 13, endByte: 55, startRow: 1, startCol: 0, endRow: 1, endCol: 42,
		},
		{
			nodeID: "auth.go/function_declaration/identifier", sourceID: "auth.go", nodeKind: "identifier",
			startByte: 18, endByte: 26, startRow: 1, startCol: 5, endRow: 1, endCol: 13,
		},
		{
			nodeID: "main.go/call_expression", sourceID: "main.go", nodeKind: "call_expression",
			startByte: 25, endByte: 45, startRow: 1, startCol: 12, endRow: 1, endCol: 32,
		},
	}
	makeSyntheticDBWithAST(t, dbPath, srcs, nodes)

	var buf bytes.Buffer
	if err := runCachePush(&buf, dbPath, outDir); err != nil {
		t.Fatalf("push: %v", err)
	}
	if err := runCachePull(&buf, outDir, restoredPath, true); err != nil {
		t.Fatalf("pull: %v", err)
	}

	// _source round-trip
	restoredSrcs := readBackSources(t, restoredPath)
	if len(restoredSrcs) != len(srcs) {
		t.Fatalf("source count: want %d, got %d", len(srcs), len(restoredSrcs))
	}
	pathToOrig := map[string][]byte{}
	for _, s := range srcs {
		pathToOrig[s.path] = s.content
	}
	for _, r := range restoredSrcs {
		if !bytes.Equal(r.content, pathToOrig[r.path]) {
			t.Errorf("content drift for %s", r.path)
		}
	}

	// _ast round-trip (the new Phase 4 guarantee)
	restoredNodes := readBackAstNodes(t, restoredPath)
	if len(restoredNodes) != len(nodes) {
		t.Fatalf("_ast row count: want %d, got %d", len(nodes), len(restoredNodes))
	}
	sortNodes := func(ns []synthAstNode) {
		sort.Slice(ns, func(i, j int) bool {
			if ns[i].sourceID != ns[j].sourceID {
				return ns[i].sourceID < ns[j].sourceID
			}
			return ns[i].nodeID < ns[j].nodeID
		})
	}
	wantSorted := append([]synthAstNode{}, nodes...)
	sortNodes(wantSorted)
	sortNodes(restoredNodes)
	for i := range wantSorted {
		if restoredNodes[i] != wantSorted[i] {
			t.Errorf("_ast row[%d] drift:\n  want: %+v\n  got:  %+v",
				i, wantSorted[i], restoredNodes[i])
		}
	}
}

// ── push: no _ast table → Phase 1 chunks (fallback) ───────────────

func TestCacheAST_NoTableFallsBackToRawContent(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "input.db")
	outDir := filepath.Join(tmp, "out")

	// Use the existing makeSyntheticDB (no _ast table).
	makeSyntheticDB(t, dbPath, []synthSource{
		{id: "a.go", path: "a.go", language: "go", content: []byte("package a\n")},
	})
	if err := runCachePush(new(bytes.Buffer), dbPath, outDir); err != nil {
		t.Fatalf("push: %v", err)
	}

	// Chunk should be raw content, NOT JSON.
	chunksDir := filepath.Join(outDir, "objects")
	bucketEntries, _ := readDirAll(t, chunksDir)
	if len(bucketEntries) == 0 {
		t.Fatalf("no chunk files written")
	}
	body, err := readFileBytes(t, bucketEntries[0])
	if err != nil {
		t.Fatalf("read chunk: %v", err)
	}
	if chunkBodyIsASTShape(body) {
		t.Errorf("Phase 1 fallback expected raw content; got JSON: %.40q", string(body))
	}
	if !bytes.Equal(body, []byte("package a\n")) {
		t.Errorf("Phase 1 chunk drift: want raw content, got %.40q", string(body))
	}
}

// ── helpers ───────────────────────────────────────────────────────

// readDirAll walks <objects-dir>/*/* and returns every chunk file
// path. The cache emits chunks as `<bucket>/<remainder>` so this is
// a 2-level walk, not arbitrary recursion.
func readDirAll(t *testing.T, dir string) ([]string, error) {
	t.Helper()
	bucketEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, b := range bucketEntries {
		if !b.IsDir() {
			continue
		}
		bucketPath := filepath.Join(dir, b.Name())
		files, err := os.ReadDir(bucketPath)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			out = append(out, filepath.Join(bucketPath, f.Name()))
		}
	}
	return out, nil
}

func readFileBytes(t *testing.T, path string) ([]byte, error) {
	t.Helper()
	return os.ReadFile(path)
}
