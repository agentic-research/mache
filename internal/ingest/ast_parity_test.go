package ingest

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestASTParity_GoSchema verifies that Engine+ASTWalker (reading from a
// leyline-produced .db) produces the same projected tree as Engine+SitterWalker
// (parsing source directly with CGO tree-sitter).
//
// This is the parity gate for tree-sitter CGO deletion. If this test passes,
// ASTWalker is a drop-in replacement for SitterWalker.
//
// Requires: leyline binary on PATH (skips if not available).
func TestASTParity_GoSchema(t *testing.T) {
	leylineBin, err := exec.LookPath("leyline")
	if err != nil {
		t.Skip("leyline binary not on PATH — skipping parity test")
	}

	schema := loadGoSchema(t)

	// Write test Go source
	srcDir := t.TempDir()
	goFile := filepath.Join(srcDir, "example.go")
	require.NoError(t, os.WriteFile(goFile, []byte(`package demo

const MaxRetries = 3

var DefaultName = "world"

type Greeter struct {
	Name string
}

type Speaker interface {
	Speak() string
}

func Hello() string {
	return "hello"
}

func (g *Greeter) Greet() string {
	return "Hi, " + g.Name
}

func (g Greeter) String() string {
	return g.Name
}

import "fmt"

func Caller() {
	fmt.Println(Hello())
}
`), 0o644))

	// --- SitterWalker path (CGO) ---
	sitterStore := graph.NewMemoryStore()
	sitterEngine := NewEngine(schema, sitterStore)
	require.NoError(t, sitterEngine.Ingest(goFile))

	sitterNodes := collectAllNodes(t, sitterStore)
	t.Logf("SitterWalker produced %d nodes", len(sitterNodes))

	// --- ASTWalker path (pure Go, via leyline parse) ---
	dbPath := filepath.Join(t.TempDir(), "test.db")
	cmd := exec.Command(leylineBin, "parse", srcDir, "-o", dbPath, "--lang", "go")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "leyline parse failed: %s", string(out))

	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// Verify _ast table exists
	var count int
	require.NoError(t, db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='_ast'").Scan(&count))
	require.Equal(t, 1, count, "_ast table must exist in leyline-produced .db")

	// --- Trace: query method selectors directly with ASTWalker ---
	aw := NewASTWalker(db)
	ptrSel := `(method_declaration receiver: (parameter_list (parameter_declaration type: (pointer_type (type_identifier) @receiver))) name: (field_identifier) @name) @scope`
	valSel := `(method_declaration receiver: (parameter_list (parameter_declaration type: (type_identifier) @receiver)) name: (field_identifier) @name) @scope`
	funcSel := `(function_declaration name: (identifier) @name) @scope`

	root := ASTRoot{DB: db, SourceID: "example.go", ParentPrefix: ""}
	ptrMatches, ptrErr := aw.Query(root, ptrSel)
	valMatches, valErr := aw.Query(root, valSel)
	funcMatches, funcErr := aw.Query(root, funcSel)

	t.Logf("AST Pointer receiver matches: %d (err=%v)", len(ptrMatches), ptrErr)
	for i, m := range ptrMatches {
		t.Logf("  ptr[%d]: %v", i, m.Values())
	}
	t.Logf("AST Value receiver matches: %d (err=%v)", len(valMatches), valErr)
	for i, m := range valMatches {
		t.Logf("  val[%d]: %v", i, m.Values())
	}
	t.Logf("AST Function matches: %d (err=%v)", len(funcMatches), funcErr)
	for i, m := range funcMatches {
		t.Logf("  func[%d]: %v", i, m.Values())
	}

	// Expected: ptr should match Greet only, val should match String only
	// If either matches BOTH methods, that's the bug
	// --- End trace ---

	astStore := graph.NewMemoryStore()
	astEngine := NewEngine(schema, astStore)
	astEngine.SetASTWalker(NewASTWalker(db))
	require.NoError(t, astEngine.Ingest(srcDir))

	astNodes := collectAllNodes(t, astStore)
	t.Logf("ASTWalker produced %d nodes", len(astNodes))

	// --- Compare projected trees ---

	// Both should produce the same root-level packages
	sitterRoots, _ := sitterStore.ListChildren("")
	astRoots, _ := astStore.ListChildren("")
	sort.Strings(sitterRoots)
	sort.Strings(astRoots)
	assert.Equal(t, sitterRoots, astRoots, "root children should match")

	// Both should find the same functions
	sitterFuncs := findNodesByPrefix(sitterNodes, "demo/functions/")
	astFuncs := findNodesByPrefix(astNodes, "demo/functions/")
	sort.Strings(sitterFuncs)
	sort.Strings(astFuncs)
	assert.Equal(t, sitterFuncs, astFuncs, "functions should match")

	// Both should find the same types
	sitterTypes := findNodesByPrefix(sitterNodes, "demo/types/")
	astTypes := findNodesByPrefix(astNodes, "demo/types/")
	sort.Strings(sitterTypes)
	sort.Strings(astTypes)
	assert.Equal(t, sitterTypes, astTypes, "types should match")

	// Both should find the same methods
	sitterMethods := findNodesByPrefix(sitterNodes, "demo/methods/")
	astMethods := findNodesByPrefix(astNodes, "demo/methods/")
	sort.Strings(sitterMethods)
	sort.Strings(astMethods)
	assert.Equal(t, sitterMethods, astMethods, "methods should match")
}

// collectAllNodes returns all node IDs in the store via BFS.
func collectAllNodes(t *testing.T, store *graph.MemoryStore) []string {
	t.Helper()
	var all []string
	queue := []string{""}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		children, err := store.ListChildren(id)
		if err != nil {
			continue
		}
		for _, c := range children {
			all = append(all, c)
			queue = append(queue, c)
		}
	}
	return all
}

// findNodesByPrefix returns node IDs that start with the given prefix.
func findNodesByPrefix(nodes []string, prefix string) []string {
	var result []string
	for _, n := range nodes {
		if len(n) >= len(prefix) && n[:len(prefix)] == prefix {
			result = append(result, n)
		}
	}
	return result
}
