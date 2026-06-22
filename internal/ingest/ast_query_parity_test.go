package ingest

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	sitter "github.com/smacker/go-tree-sitter"
	_ "modernc.org/sqlite"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/lang"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Axis-1 parity: ASTWalker (pure-Go, _ast tables) must produce a BYTE-IDENTICAL
// projected graph to SitterWalker (CGO tree-sitter) for the same source — when
// both consume the SAME parse tree. The _ast db here is emitted from mache's
// own tree-sitter parse (sitterToASTDB), so this gate runs in normal CGO CI with
// NO leyline binary dependency. It isolates exactly one variable: the query
// engine. The leyline-coupled axis-2 gate lives in ast_parity_test.go.
//
// This is the load-bearing invariant for every step of mache-36d961 (CGO removal):
// each time a path is ASTWalker-ified, this gate proves the projected output did
// not drift by a single byte.

// sitterToASTDB parses src with mache's own tree-sitter grammar and emits the
// exact _ast/_source/nodes schema ASTWalker consumes (the same shape
// ast_walker_bench_test.go hand-builds, and that `leyline parse` produces).
//
// Node IDs are "/"-joined paths; each segment is "<kind>_<perKindIndex>" so
// stripNumericSuffix recovers the tree-sitter kind and depth == segment count.
// The root node IS emitted (parent_id=""), so top-level selectors like
// `(source_file ...) @scope` match it (see the emit(root, ...) call below).
// source_id is filepath.Base(sourceFile) to match the engine's ASTRoot wiring
// (engine_treesitter.go: SourceID = filepath.Base(result.job.path)).
func sitterToASTDB(tb testing.TB, dbPath, sourceFile, langName string, src []byte) *sql.DB {
	tb.Helper()

	l := lang.ForName(langName)
	require.NotNil(tb, l, "unknown language %q", langName)

	parser := sitter.NewParser()
	parser.SetLanguage(l.Grammar())
	tree, err := parser.ParseCtx(context.Background(), nil, src)
	require.NoError(tb, err)
	defer tree.Close()

	db, err := sql.Open("sqlite", dbPath)
	require.NoError(tb, err)

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
		CREATE INDEX idx_ast_kind_source ON _ast(node_kind, source_id);
		CREATE INDEX idx_parent_name ON nodes(parent_id, name);
		CREATE TABLE _source (
			id TEXT PRIMARY KEY, language TEXT NOT NULL,
			content BLOB NOT NULL, path TEXT
		);
	`)
	require.NoError(tb, err)

	sourceID := filepath.Base(sourceFile)
	_, err = db.Exec("INSERT INTO _source (id, language, content, path) VALUES (?, ?, ?, NULL)",
		sourceID, langName, src)
	require.NoError(tb, err)

	tx, err := db.Begin()
	require.NoError(tb, err)

	insNode, err := tx.Prepare(`INSERT INTO nodes (id, parent_id, name, kind, mtime, record, source_file)
		VALUES (?, ?, ?, ?, 0, ?, ?)`)
	require.NoError(tb, err)
	insAST, err := tx.Prepare(`INSERT INTO _ast
		(node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	require.NoError(tb, err)
	// Cleanup on early require failure: Rollback is a no-op after a successful
	// Commit; closing the prepared statements releases their handles.
	defer func() { _ = tx.Rollback() }()
	defer func() { _ = insNode.Close() }()
	defer func() { _ = insAST.Close() }()

	// Emit every named node, INCLUDING the root (e.g. source_file) — the schema's
	// top-level selector matches it: `(source_file (package_clause ...)) @scope`.
	// IDs are "<kind>_<perKindIndex>" path segments; depth == segment count.
	var emit func(n *sitter.Node, id, parentID string)
	emit = func(n *sitter.Node, id, parentID string) {
		hasNamedKids := n.NamedChildCount() > 0
		kindInt := 0 // leaf ("file")
		record := "" // leaf text comes from the record column
		if hasNamedKids {
			kindInt = 1 // interior ("dir")
		} else {
			record = string(src[n.StartByte():n.EndByte()])
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
	root := tree.RootNode()
	emit(root, fmt.Sprintf("%s_0", root.Type()), "")

	require.NoError(tb, tx.Commit())
	return db
}

// readAllContent reads the entire rendered content of a node via the Graph
// ReadContent(offset) contract, so the comparison sees exactly what an NFS/MCP
// consumer would read byte-for-byte.
func readAllContent(tb testing.TB, g graph.Graph, id string) []byte {
	tb.Helper()
	var out []byte
	buf := make([]byte, 64*1024)
	var off int64
	for {
		n, err := g.ReadContent(id, buf, off)
		if n > 0 {
			out = append(out, buf[:n]...)
			off += int64(n)
		}
		// A gating test must not swallow read errors: a non-EOF error would
		// otherwise masquerade as truncated/empty content and produce false parity.
		if err != nil && err != io.EOF {
			tb.Fatalf("ReadContent(%q) at offset %d: %v", id, off, err)
		}
		if err != nil || n == 0 || n < len(buf) {
			break
		}
	}
	return out
}

// collectTree returns every node ID reachable from the root, sorted.
func collectTree(tb testing.TB, g graph.Graph) []string {
	tb.Helper()
	var all []string
	queue := []string{""}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		children, err := g.ListChildren(id)
		if err != nil {
			tb.Fatalf("ListChildren(%q): %v", id, err) // gating test: fail fast on traversal errors
		}
		for _, c := range children {
			all = append(all, c)
			queue = append(queue, c)
		}
	}
	sort.Strings(all)
	return all
}

// assertProjectionParity asserts the two projected graphs are byte-identical
// from a consumer's vantage: same node set, same children per node, and same
// rendered content bytes. (Cross-file callers/callees parity is intentionally
// out of scope for this single-file gate — it can't witness inter-file edges;
// tracked as gate expansion on mache-1166aa.)
func assertProjectionParity(t *testing.T, want, got graph.Graph) {
	t.Helper()

	wantNodes := collectTree(t, want)
	gotNodes := collectTree(t, got)
	if !assert.Equal(t, wantNodes, gotNodes, "node set must match") {
		logTreeDiff(t, wantNodes, gotNodes)
		return // content/children comparison is meaningless once the tree diverges
	}

	for _, id := range wantNodes {
		wc, werr := want.ListChildren(id)
		gc, gerr := got.ListChildren(id)
		require.NoErrorf(t, werr, "ListChildren(%q) on want", id)
		require.NoErrorf(t, gerr, "ListChildren(%q) on got", id)
		sort.Strings(wc)
		sort.Strings(gc)
		assert.Equalf(t, wc, gc, "children of %q must match", id)

		assert.Equalf(t, readAllContent(t, want, id), readAllContent(t, got, id),
			"rendered content of %q must be byte-identical", id)
	}
}

func logTreeDiff(t *testing.T, want, got []string) {
	t.Helper()
	wset := map[string]bool{}
	for _, w := range want {
		wset[w] = true
	}
	gset := map[string]bool{}
	for _, g := range got {
		gset[g] = true
	}
	n := 0
	for _, w := range want {
		if !gset[w] && n < 15 {
			t.Logf("  SITTER only: %s", w)
			n++
		}
	}
	for _, g := range got {
		if !wset[g] && n < 30 {
			t.Logf("  AST    only: %s", g)
			n++
		}
	}
}

func loadParitySchema(t *testing.T, path string) api.Topology {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var schema api.Topology
	require.NoError(t, json.Unmarshal(data, &schema))
	return schema
}

// runQueryParity ingests the same source through SitterWalker (CGO) and
// ASTWalker (over an emitted _ast db) and asserts byte-level projection parity.
func runQueryParity(t *testing.T, schemaPath, langName, sourceFile string, src []byte) {
	t.Helper()
	schema := loadParitySchema(t, schemaPath)

	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, sourceFile)
	require.NoError(t, os.MkdirAll(filepath.Dir(srcPath), 0o755))
	require.NoError(t, os.WriteFile(srcPath, src, 0o644))

	// SitterWalker path (CGO, parses live)
	sitterStore := graph.NewMemoryStore()
	require.NoError(t, NewEngine(&schema, sitterStore).Ingest(srcPath))

	// ASTWalker path (pure-Go query over a _ast db emitted from the SAME parse)
	db := sitterToASTDB(t, filepath.Join(t.TempDir(), "ast.db"), sourceFile, langName, src)
	defer func() { _ = db.Close() }()
	astStore := graph.NewMemoryStore()
	astEngine := NewEngine(&schema, astStore)
	astEngine.SetASTWalker(NewASTWalker(db))
	require.NoError(t, astEngine.Ingest(srcPath))

	assertProjectionParity(t, sitterStore, astStore)
}

func TestASTQueryParity_Go(t *testing.T) {
	runQueryParity(t, "../../examples/go-schema.json", "go", "example.go", []byte(`package demo

import "fmt"

const MaxRetries = 3

var DefaultName = "world"

type Greeter struct {
	Name string
}

func Hello() string {
	return "hello"
}

func (g *Greeter) Greet() string {
	return "Hi, " + g.Name
}

func Caller() {
	fmt.Println(Hello())
}
`))
}
