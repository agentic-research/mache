package ingest

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/leyline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"
)

// AST-backed address-ref extraction tests (ADR-0012 step 4). These replace the
// former SitterWalker-based unit tests: source is parsed by the pinned ley-line
// into an `_ast` db and refs are extracted via ASTWalker.ExtractAddressRefs —
// the same pure-Go path the engine uses. Gated on the pinned leyline being
// available (PATH or ~/.mache/bin cache); skips otherwise.

// parseSourceToASTWalker writes files into a temp dir, parses them with the
// pinned ley-line into an `_ast` db, and returns an ASTWalker over it.
func parseSourceToASTWalker(t *testing.T, lang string, files map[string]string) (*ASTWalker, *sql.DB) {
	t.Helper()
	bin, err := leyline.ResolveBinary(false)
	if err != nil {
		t.Skipf("pinned leyline unavailable (%v); run with the cached ~/.mache/bin/leyline", err)
	}
	srcDir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(srcDir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	}
	dbPath := filepath.Join(t.TempDir(), "ast.db")
	out, err := exec.Command(bin, "parse", srcDir, "-o", dbPath, "--lang", lang).CombinedOutput() //nolint:gosec // test-only, pinned binary
	require.NoError(t, err, "leyline parse failed: %s", string(out))
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return NewASTWalker(db), db
}

func TestExtractAddressRefs_GoOsGetenv(t *testing.T) {
	w, _ := parseSourceToASTWalker(t, "go", map[string]string{
		"main.go": `package main

import "os"

func main() {
	db := os.Getenv("DATABASE_URL")
	key := os.Getenv("API_KEY")
	_ = db
	_ = key
}
`,
	})

	refs, err := w.ExtractAddressRefs("main.go", "go")
	require.NoError(t, err)
	assert.Contains(t, refs, "env:DATABASE_URL")
	assert.Contains(t, refs, "env:API_KEY")
	var envRefs []string
	for _, ref := range refs {
		if strings.HasPrefix(ref, "env:") {
			envRefs = append(envRefs, ref)
		}
	}
	assert.Len(t, envRefs, 2, "should find exactly two env refs")
}

func TestExtractAddressRefs_GoOsGetenv_Dedup(t *testing.T) {
	w, _ := parseSourceToASTWalker(t, "go", map[string]string{
		"main.go": `package main

import "os"

func main() {
	_ = os.Getenv("SAME_VAR")
	_ = os.Getenv("SAME_VAR")
}
`,
	})

	refs, err := w.ExtractAddressRefs("main.go", "go")
	require.NoError(t, err)
	var envRefs []string
	for _, ref := range refs {
		if strings.HasPrefix(ref, "env:") {
			envRefs = append(envRefs, ref)
		}
	}
	assert.Equal(t, []string{"env:SAME_VAR"}, envRefs, "duplicates should be deduplicated")
}

func TestExtractAddressRefs_GoOsGetenv_NotMatched(t *testing.T) {
	w, _ := parseSourceToASTWalker(t, "go", map[string]string{
		"main.go": `package main

import "fmt"

func main() {
	// Not os.Getenv -- should not match
	fmt.Println("DATABASE_URL")
}
`,
	})

	refs, err := w.ExtractAddressRefs("main.go", "go")
	require.NoError(t, err)
	for _, ref := range refs {
		assert.NotContains(t, ref, "env:", "fmt.Println should not emit env refs")
	}
}

func TestExtractAddressRefs_HCLVariable(t *testing.T) {
	w, _ := parseSourceToASTWalker(t, "hcl", map[string]string{
		"variables.tf": `variable "DATABASE_URL" {
  type    = string
  default = "postgres://localhost:5432/mydb"
}

variable "API_KEY" {
  type = string
}

resource "aws_instance" "web" {
  ami = "ami-12345"
}
`,
	})

	refs, err := w.ExtractAddressRefs("variables.tf", "terraform")
	require.NoError(t, err)
	assert.Contains(t, refs, "env:DATABASE_URL")
	assert.Contains(t, refs, "env:API_KEY")
	for _, ref := range refs {
		assert.NotContains(t, ref, "aws_instance", "resource names should not appear as env refs")
	}
	assert.Len(t, refs, 2, "should find exactly two variable env refs")
}

// TestExtractAddressRefs_HCLModuleSource — bead mache-q43l first milestone.
// Module blocks with a `source = "..."` attribute emit `mod:<value>` tokens.
func TestExtractAddressRefs_HCLModuleSource(t *testing.T) {
	w, _ := parseSourceToASTWalker(t, "hcl", map[string]string{
		"main.tf": `module "vpc" {
  source = "./modules/vpc"
  version = "1.0.0"
}

module "remote_app" {
  source = "github.com/foo/bar"
}

variable "DB" {
  default = "postgres://localhost:5432/mydb"
}

resource "aws_instance" "web" {
  source = "this should not match — wrong block type"
}
`,
	})

	refs, err := w.ExtractAddressRefs("main.tf", "terraform")
	require.NoError(t, err)
	assert.Contains(t, refs, "mod:./modules/vpc")
	assert.Contains(t, refs, "mod:github.com/foo/bar")
	for _, ref := range refs {
		assert.NotContains(t, ref, "aws_instance", "resource source attributes must not produce mod refs")
		assert.NotContains(t, ref, "this should not match", "non-module block sources must not match")
	}
}

func TestExtractAddressRefs_GoImports(t *testing.T) {
	w, _ := parseSourceToASTWalker(t, "go", map[string]string{
		"main.go": `package main

import (
	std "fmt"
	_ ` + "`example.com/blank`" + `
	. "example.com/dot"
	"example.com/normal"
)

func main() {
	std.Println(normal.Name)
	_ = Name
}
`,
	})

	refs, err := w.ExtractAddressRefs("main.go", "go")
	require.NoError(t, err)
	assert.Contains(t, refs, "gomod:fmt")
	assert.Contains(t, refs, "gomod:example.com/blank")
	assert.Contains(t, refs, "gomod:example.com/dot")
	assert.Contains(t, refs, "gomod:example.com/normal")
	assert.Len(t, refs, 4, "aliases, blank imports, dot imports, and raw literals must not affect import identity")
}

func TestExtractAddressRefs_PreservesNestedSourceID(t *testing.T) {
	w, _ := parseSourceToASTWalker(t, "go", map[string]string{
		"sub/nested.go": `package nested

import dep "example.com/nested/dep"

func Nested() any { return dep.Value }
`,
	})

	refs, err := w.ExtractAddressRefs("sub/nested.go", "go")
	require.NoError(t, err)
	assert.Contains(t, refs, "gomod:example.com/nested/dep")
}

func TestEngine_AddressRefs_GoImportsReachNodeRefs(t *testing.T) {
	bin, err := leyline.ResolveBinary(false)
	if err != nil {
		t.Skipf("pinned leyline unavailable (%v)", err)
	}

	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte(`package main

import dep "example.com/acme/dep"

func UseDep() {
	_ = dep.Value
}
`), 0o644))

	schemaJSON := `{
  "version": "v1",
  "nodes": [{
    "name": "functions",
    "selector": "$",
    "language": "go",
    "children": [{"name": "{{.name}}", "selector": "(function_declaration name: (identifier) @name) @scope", "files": [{"name": "source", "content_template": "{{.scope}}"}]}]
  }]
}`
	var schema api.Topology
	require.NoError(t, json.Unmarshal([]byte(schemaJSON), &schema))

	dbPath := filepath.Join(t.TempDir(), "ast.db")
	out, err := exec.Command(bin, "parse", tmpDir, "-o", dbPath).CombinedOutput() //nolint:gosec // test-only, pinned binary
	require.NoError(t, err, "leyline parse failed: %s", string(out))
	astDB, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = astDB.Close() }()

	store := graph.NewMemoryStore()
	engine := NewEngine(&schema, store)
	engine.SetASTWalker(NewASTWalker(astDB))
	require.NoError(t, engine.Ingest(tmpDir))

	callers, err := store.GetCallers("gomod:example.com/acme/dep")
	require.NoError(t, err)
	require.Len(t, callers, 1)
	assert.Equal(t, "functions/UseDep/source", callers[0].ID)
}

// TestEngine_AddressRefs_CrossLanguageBridge — a mixed Go + HCL project where
// both reference the same env var must produce env:DATABASE_URL refs that
// connect the two constructs in the graph. Exercises the full engine + ASTWalker
// projection (ADR-0012 step 4 — no in-process CGO).
func TestEngine_AddressRefs_CrossLanguageBridge(t *testing.T) {
	bin, err := leyline.ResolveBinary(false)
	if err != nil {
		t.Skipf("pinned leyline unavailable (%v)", err)
	}

	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte(`package main

import "os"

func LoadConfig() {
	db := os.Getenv("DATABASE_URL")
	_ = db
}
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "variables.tf"), []byte(`variable "DATABASE_URL" {
  type    = string
  default = "postgres://localhost:5432/mydb"
}
`), 0o644))

	schemaJSON := `{
  "version": "v1",
  "nodes": [
    {
      "name": "functions",
      "selector": "$",
      "language": "go",
      "children": [
        {
          "name": "{{.name}}",
          "selector": "(function_declaration name: (identifier) @name) @scope",
          "files": [ { "name": "source", "content_template": "{{.scope}}" } ]
        }
      ]
    },
    {
      "name": "variables",
      "selector": "$",
      "language": "terraform",
      "children": [
        {
          "name": "{{.name}}",
          "selector": "(block (identifier) @_type (string_lit) @name (body) @scope (#eq? @_type \"variable\"))",
          "files": [ { "name": "source", "content_template": "{{.scope}}" } ]
        }
      ]
    }
  ]
}`
	var schema api.Topology
	require.NoError(t, json.Unmarshal([]byte(schemaJSON), &schema))

	// ley-line parses source into an _ast db; the engine projects it via ASTWalker.
	dbPath := filepath.Join(t.TempDir(), "ast.db")
	out, err := exec.Command(bin, "parse", tmpDir, "-o", dbPath).CombinedOutput() //nolint:gosec // test-only, pinned binary
	require.NoError(t, err, "leyline parse failed: %s", string(out))
	astDB, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = astDB.Close() }()

	store := graph.NewMemoryStore()
	engine := NewEngine(&schema, store)
	engine.SetASTWalker(NewASTWalker(astDB))
	require.NoError(t, engine.Ingest(tmpDir))

	callers, err := store.GetCallers("env:DATABASE_URL")
	require.NoError(t, err)
	require.NotEmpty(t, callers, "env:DATABASE_URL should have callers from both Go and HCL")

	var goSourceFound, hclSourceFound bool
	for _, node := range callers {
		t.Logf("Caller: %s", node.ID)
		if node.ID == "functions/LoadConfig/source" {
			goSourceFound = true
		}
		if filepath.Dir(filepath.Dir(node.ID)) == "variables" {
			hclSourceFound = true
		}
	}
	assert.True(t, goSourceFound, "Go LoadConfig function should reference env:DATABASE_URL")
	assert.True(t, hclSourceFound, "HCL variable declaration should reference env:DATABASE_URL")

	refsMap := store.RefsMap()
	envRefs, ok := refsMap["env:DATABASE_URL"]
	require.True(t, ok, "env:DATABASE_URL should be in refs map")
	assert.GreaterOrEqual(t, len(envRefs), 2, "should have refs from both Go and HCL")
}
