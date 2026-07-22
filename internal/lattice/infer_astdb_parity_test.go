package lattice

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-research/mache/internal/leyline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// pinnedLeylineForLattice resolves the pinned leyline binary via the production
// resolver (PATH → ~/.mache/bin → verified pin, never a download) — no regex,
// no hand-rolled candidate loop. Importing internal/leyline is CGO-free now
// that the //go:build leyline FFI (client.go) is gone, so tests use the real
// ResolveBinary instead of re-deriving the pin from socket.go source text.
// Fails in CI (where the binary must be provisioned via `task leyline:ensure`)
// and skips otherwise.
func pinnedLeylineForLattice(t *testing.T) string {
	t.Helper()
	bin, err := leyline.ResolveBinary(false) // never download in tests
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("pinned leyline unavailable in CI (%v) — provision it (task leyline:ensure) before tests", err)
		}
		t.Skipf("pinned leyline unavailable (%v); source projection requires it", err)
	}
	return bin
}

// astdbFixtures cover the construct spread the go-schema projects: functions,
// methods (pointer + value receivers), a struct, an interface, consts, vars,
// and imports across two files.
var astdbFixtures = map[string][]byte{
	"main.go": []byte(`package demo

import "fmt"

const MaxRetries = 3

var DefaultName = "world"

func Hello() string {
	return "hello"
}

func Caller() {
	fmt.Println(Hello())
}
`),
	"types.go": []byte(`package demo

type Greeter struct {
	Name string
}

type Speaker interface {
	Speak() string
}

func (g *Greeter) Greet() string {
	return "Hi, " + g.Name
}

func (g Greeter) String() string {
	return g.Name
}
`),
}

// TestInferFromASTDB_Go verifies InferFromASTDB (pure Go, over a leyline
// `_ast` db) produces a non-empty tree-sitter topology for a fixture covering
// functions, methods, types, consts, vars, and imports. This replaces the
// former sitter-vs-AST parity gate: in-process CGO tree-sitter (and the
// InferFromTreeSitterRoots reference path) were removed in ADR-0012 step 4,
// so there is no CGO topology left to compare against.
func TestInferFromASTDB_Go(t *testing.T) {
	leylineBin := pinnedLeylineForLattice(t)

	srcDir := t.TempDir()
	for name, content := range astdbFixtures {
		require.NoError(t, os.WriteFile(filepath.Join(srcDir, name), content, 0o644))
	}

	dbPath := filepath.Join(t.TempDir(), "astdb.db")
	out, err := exec.Command(leylineBin, "parse", srcDir, "-o", dbPath).CombinedOutput()
	require.NoError(t, err, "leyline parse failed: %s", string(out))

	inf := &Inferrer{Config: InferConfig{Method: "fca", Language: "go"}}
	topo, err := inf.InferFromASTDB(dbPath)
	require.NoError(t, err)
	require.NotNil(t, topo)
	require.NotEmpty(t, topo.Nodes, "FCA over the _ast db must yield at least one root node")

	astJSON, err := json.MarshalIndent(topo, "", "  ")
	require.NoError(t, err)
	t.Logf("astdb topology: %d root nodes, %d bytes", len(topo.Nodes), len(astJSON))

	// The projected selectors must be tree-sitter S-expressions (start with
	// '('), not JSONPath — the FCA path forces ProjectAST for _ast data.
	var sawSExpr bool
	var walk func(nodes []nodeView)
	walk = func(nodes []nodeView) {
		for _, n := range nodes {
			if strings.HasPrefix(strings.TrimSpace(n.Selector), "(") {
				sawSExpr = true
			}
			walk(n.Children)
		}
	}
	var tv topoView
	require.NoError(t, json.Unmarshal(astJSON, &tv))
	walk(tv.Nodes)
	assert.True(t, sawSExpr, "inferred topology should contain tree-sitter S-expression selectors")
}

// minimal views for selector inspection without depending on api internals.
type topoView struct {
	Nodes []nodeView `json:"nodes"`
}

type nodeView struct {
	Selector string     `json:"selector"`
	Children []nodeView `json:"children"`
}
