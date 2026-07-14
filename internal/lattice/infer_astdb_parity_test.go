//go:build leyline

package lattice

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	"github.com/agentic-research/mache/internal/lang"
)

// resolvePinnedLeylineForLattice mirrors leyline.ResolveBinary(false)
// without importing internal/leyline — under the `leyline` build tag that
// package's CGO FFI bindings (client.go, leyline_fs.h) would be compiled,
// which this tagged test must not require. PATH and ~/.mache/bin candidates
// are each accepted ONLY if `--version` exactly matches the pin extracted
// from socket.go; no bare unverified PATH leyline, never a download.
func resolvePinnedLeylineForLattice(t *testing.T) string {
	t.Helper()

	// Extract the pin from socket.go (the source-grep pattern
	// version_parity_test.go established for pin invariants).
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

// TestInferFromASTDBParity_Go verifies InferFromASTDB (pure Go, leyline
// _ast) produces the same topology as InferFromTreeSitterRoots (CGO
// tree-sitter) on the same fixture sources with the same config.
// parityFixtures covers the construct spread the go-schema projects:
// functions, methods (pointer + value receivers), a struct, an
// interface, consts, vars, and imports across two files.
var parityFixtures = map[string][]byte{
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

func TestInferFromASTDBParity_Go(t *testing.T) {
	leylineBin := resolvePinnedLeylineForLattice(t)

	fixtures := parityFixtures
	srcDir := t.TempDir()
	names := make([]string, 0, len(fixtures))
	for name, content := range fixtures {
		require.NoError(t, os.WriteFile(filepath.Join(srcDir, name), content, 0o644))
		names = append(names, name)
	}
	sort.Strings(names)

	config := InferConfig{Method: "fca", Language: "go"}

	// --- Sitter path (CGO) ---
	goLang := lang.ForName("go")
	require.NotNil(t, goLang)
	roots := make([]*sitter.Node, 0, len(names))
	for _, name := range names {
		parser := sitter.NewParser()
		parser.SetLanguage(goLang.Grammar())
		tree, err := parser.ParseCtx(context.Background(), nil, fixtures[name])
		require.NoError(t, err)
		require.NotNil(t, tree)
		roots = append(roots, tree.RootNode())
	}
	sitterInf := &Inferrer{Config: config}
	sitterTopo, err := sitterInf.InferFromTreeSitterRoots(roots...)
	require.NoError(t, err)

	// --- Leyline path (pure Go) ---
	dbPath := filepath.Join(t.TempDir(), "parity.db")
	out, err := exec.Command(leylineBin, "parse", srcDir, "-o", dbPath).CombinedOutput()
	require.NoError(t, err, "leyline parse failed: %s", string(out))

	astInf := &Inferrer{Config: config}
	astTopo, err := astInf.InferFromASTDB(dbPath)
	require.NoError(t, err)

	sitterJSON, err := json.MarshalIndent(sitterTopo, "", "  ")
	require.NoError(t, err)
	astJSON, err := json.MarshalIndent(astTopo, "", "  ")
	require.NoError(t, err)

	t.Logf("sitter topology: %d root nodes, %d bytes", len(sitterTopo.Nodes), len(sitterJSON))
	t.Logf("astdb topology:  %d root nodes, %d bytes", len(astTopo.Nodes), len(astJSON))
	require.JSONEq(t, string(sitterJSON), string(astJSON), "topologies should be identical")
}
