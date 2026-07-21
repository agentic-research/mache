package lattice

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// resolvePinnedLeylineForLattice mirrors leyline.ResolveBinary(false)
// without importing internal/leyline. PATH and ~/.mache/bin candidates are
// each accepted ONLY if `--version` exactly matches the pin extracted from
// socket.go; no bare unverified PATH leyline, never a download.
func resolvePinnedLeylineForLattice(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "module root with go.mod not found")
		dir = parent
	}
	src, err := os.ReadFile(filepath.Join(dir, "internal", "leyline", "socket.go"))
	require.NoError(t, err)
	m := regexp.MustCompile(`const leylineBinaryVersion = "(v\d+\.\d+\.\d+)"`).FindSubmatch(src)
	require.NotNil(t, m, "leylineBinaryVersion const not found in internal/leyline/socket.go")
	pin := string(m[1])

	var candidates []string
	if p, err := exec.LookPath("leyline"); err == nil {
		candidates = append(candidates, p)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".mache", "bin", "leyline"))
	}
	for _, c := range candidates {
		out, err := exec.Command(c, "--version").Output()
		if err != nil {
			continue
		}
		got := regexp.MustCompile(`\d+\.\d+\.\d+`).FindString(string(out))
		if got != "" && "v"+got == pin {
			return c
		}
	}
	t.Skipf("no leyline matching the pinned %s available (tests never download)", pin)
	return ""
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
	leylineBin := resolvePinnedLeylineForLattice(t)

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
