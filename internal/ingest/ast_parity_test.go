//go:build leyline

package ingest

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// runParityTest verifies Engine+ASTWalker produces the same projected tree
// as Engine+SitterWalker for a given language. This is the parity gate for
// tree-sitter CGO deletion per language.
//
// Requires: leyline binary on PATH (skips if not available).
func runParityTest(t *testing.T, schemaPath, lang, sourceFile string, sourceContent []byte) {
	t.Helper()

	if _, err := exec.LookPath("leyline"); err != nil {
		t.Skip("leyline binary not on PATH — skipping parity test")
	}

	schemaData, err := os.ReadFile(schemaPath)
	require.NoError(t, err)
	var schema api.Topology
	require.NoError(t, json.Unmarshal(schemaData, &schema))

	// Write source file
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, sourceFile)
	require.NoError(t, os.MkdirAll(filepath.Dir(srcPath), 0o755))
	require.NoError(t, os.WriteFile(srcPath, sourceContent, 0o644))

	// --- SitterWalker path (CGO) ---
	sitterStore := graph.NewMemoryStore()
	sitterEngine := NewEngine(&schema, sitterStore)
	require.NoError(t, sitterEngine.Ingest(srcPath))

	sitterNodes := collectAllNodes(t, sitterStore)
	t.Logf("SitterWalker: %d nodes", len(sitterNodes))

	// --- ASTWalker path (pure Go, via leyline parse) ---
	dbPath := filepath.Join(t.TempDir(), "test.db")
	cmd := exec.Command("leyline", "parse", srcDir, "-o", dbPath, "--lang", lang)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "leyline parse failed: %s", string(out))

	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	astStore := graph.NewMemoryStore()
	astEngine := NewEngine(&schema, astStore)
	astEngine.SetASTWalker(NewASTWalker(db))
	require.NoError(t, astEngine.Ingest(srcDir))

	astNodes := collectAllNodes(t, astStore)
	t.Logf("ASTWalker:    %d nodes", len(astNodes))

	// --- Compare ---
	sitterRoots, _ := sitterStore.ListChildren("")
	astRoots, _ := astStore.ListChildren("")
	sort.Strings(sitterRoots)
	sort.Strings(astRoots)
	assert.Equal(t, sitterRoots, astRoots, "root children should match")

	sort.Strings(sitterNodes)
	sort.Strings(astNodes)
	assert.Equal(t, len(sitterNodes), len(astNodes), "node count should match")
	if !assert.Equal(t, sitterNodes, astNodes, "all nodes should match") {
		// Log first 10 differences for debugging
		diffs := 0
		si, ai := 0, 0
		for si < len(sitterNodes) && ai < len(astNodes) && diffs < 10 {
			if sitterNodes[si] == astNodes[ai] {
				si++
				ai++
			} else if sitterNodes[si] < astNodes[ai] {
				t.Logf("  SITTER only: %s", sitterNodes[si])
				si++
				diffs++
			} else {
				t.Logf("  AST only:    %s", astNodes[ai])
				ai++
				diffs++
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Per-language parity tests
// ---------------------------------------------------------------------------

func TestASTParity_Go(t *testing.T) {
	runParityTest(t, "../../examples/go-schema.json", "go", "example.go", []byte(`package demo

import "fmt"

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

func Caller() {
	fmt.Println(Hello())
}
`))
}

func TestASTParity_Python(t *testing.T) {
	runParityTest(t, "../../cmd/schemas/python.json", "python", "example.py", []byte(`import os
from pathlib import Path

class Animal:
    def __init__(self, name):
        self.name = name

    def speak(self):
        return f"{self.name} speaks"

class Dog(Animal):
    def speak(self):
        return f"{self.name} barks"

def greet(name):
    return f"Hello, {name}"

def main():
    dog = Dog("Rex")
    print(greet(dog.name))
    print(dog.speak())
`))
}

func TestASTParity_Elixir(t *testing.T) {
	runParityTest(t, "../../cmd/schemas/elixir.json", "elixir", "example.ex", []byte(`defmodule MyApp.Greeter do
  def hello(name) do
    "Hello, #{name}"
  end

  defp internal_helper do
    :ok
  end

  defmacro my_macro(expr) do
    quote do
      unquote(expr)
    end
  end
end

defmodule MyApp.Worker do
  def run do
    Greeter.hello("world")
  end
end
`))
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

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
