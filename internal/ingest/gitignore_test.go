package ingest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #1: .gitignore not respected — secrets indexed into DB
// Files matching .gitignore patterns should be excluded from ingestion.

func TestEngine_RespectsGitignore(t *testing.T) {
	schema := loadGoSchema(t)
	tmpDir := t.TempDir()

	// Create a .gitignore that excludes .env files and a data directory
	err := os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(`
.env*
secrets/
*.log
data/
`), 0o644)
	require.NoError(t, err)

	// Create a Go file (should be ingested)
	goFile := filepath.Join(tmpDir, "main.go")
	err = os.WriteFile(goFile, []byte("package main\nfunc Main() {}\n"), 0o644)
	require.NoError(t, err)

	// Create files that SHOULD be excluded by .gitignore
	err = os.WriteFile(filepath.Join(tmpDir, ".envrc"), []byte(`export API_KEY="secret123"`), 0o644)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(tmpDir, ".env.bak"), []byte(`DB_PASSWORD="hunter2"`), 0o644)
	require.NoError(t, err)

	secretsDir := filepath.Join(tmpDir, "secrets")
	require.NoError(t, os.Mkdir(secretsDir, 0o755))
	err = os.WriteFile(filepath.Join(secretsDir, "creds.json"), []byte(`{"key":"value"}`), 0o644)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(tmpDir, "debug.log"), []byte("log data"), 0o644)
	require.NoError(t, err)

	dataDir := filepath.Join(tmpDir, "data")
	require.NoError(t, os.Mkdir(dataDir, 0o755))
	err = os.WriteFile(filepath.Join(dataDir, "experiment.yaml"), []byte("key: value"), 0o644)
	require.NoError(t, err)

	// Create a file that SHOULD be ingested (not in .gitignore)
	err = os.WriteFile(filepath.Join(tmpDir, "README.txt"), []byte("hello"), 0o644)
	require.NoError(t, err)

	store := graph.NewMemoryStore()
	engine := NewEngine(schema, store)
	require.NoError(t, engine.Ingest(tmpDir))

	// Go file should be processed
	_, err = store.GetNode("main/functions/Main/source")
	require.NoError(t, err, "Go file should be processed")

	// README should be in _project_files
	_, err = store.GetNode("_project_files/README.txt")
	require.NoError(t, err, "README.txt should be ingested")

	// .envrc should NOT be ingested (matches .env* in .gitignore)
	_, err = store.GetNode("_project_files/.envrc")
	assert.Error(t, err, ".envrc should be excluded by .gitignore")

	// .env.bak should NOT be ingested
	_, err = store.GetNode("_project_files/.env.bak")
	assert.Error(t, err, ".env.bak should be excluded by .gitignore")

	// secrets/ directory should NOT be ingested
	_, err = store.GetNode("_project_files/secrets")
	assert.Error(t, err, "secrets/ dir should be excluded by .gitignore")
	_, err = store.GetNode("_project_files/secrets/creds.json")
	assert.Error(t, err, "secrets/creds.json should be excluded by .gitignore")

	// *.log should NOT be ingested
	_, err = store.GetNode("_project_files/debug.log")
	assert.Error(t, err, "debug.log should be excluded by .gitignore")

	// data/ directory should NOT be ingested
	_, err = store.GetNode("_project_files/data")
	assert.Error(t, err, "data/ dir should be excluded by .gitignore")
	_, err = store.GetNode("_project_files/data/experiment.yaml")
	assert.Error(t, err, "data/experiment.yaml should be excluded by .gitignore")
}

func TestEngine_GitignoreNestedDirectories(t *testing.T) {
	schema := loadGoSchema(t)
	tmpDir := t.TempDir()

	// Root .gitignore
	err := os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("*.log\n"), 0o644)
	require.NoError(t, err)

	// Nested .gitignore in subdir
	subDir := filepath.Join(tmpDir, "pkg")
	require.NoError(t, os.Mkdir(subDir, 0o755))
	err = os.WriteFile(filepath.Join(subDir, ".gitignore"), []byte("testdata/\n"), 0o644)
	require.NoError(t, err)

	// Files
	err = os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\nfunc Main() {}\n"), 0o644)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(tmpDir, "app.log"), []byte("log"), 0o644)
	require.NoError(t, err)

	testdataDir := filepath.Join(subDir, "testdata")
	require.NoError(t, os.Mkdir(testdataDir, 0o755))
	err = os.WriteFile(filepath.Join(testdataDir, "fixture.txt"), []byte("fixture"), 0o644)
	require.NoError(t, err)

	// Non-ignored file in pkg/
	err = os.WriteFile(filepath.Join(subDir, "util.go"), []byte("package pkg\nfunc Util() {}\n"), 0o644)
	require.NoError(t, err)

	store := graph.NewMemoryStore()
	engine := NewEngine(schema, store)
	require.NoError(t, engine.Ingest(tmpDir))

	// app.log should be excluded (root .gitignore)
	_, err = store.GetNode("_project_files/app.log")
	assert.Error(t, err, "app.log should be excluded by root .gitignore")

	// pkg/testdata/ should be excluded (nested .gitignore)
	_, err = store.GetNode("_project_files/pkg/testdata")
	assert.Error(t, err, "pkg/testdata/ should be excluded by nested .gitignore")

	// pkg/util.go SHOULD be ingested
	_, err = store.GetNode("pkg/functions/Util/source")
	require.NoError(t, err, "pkg/util.go should be processed")
}

func TestEngine_GitignoreNestedNegation(t *testing.T) {
	schema := loadGoSchema(t)
	tmpDir := t.TempDir()

	// Root .gitignore ignores all .generated.go files
	err := os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("*.generated.go\n"), 0o644)
	require.NoError(t, err)

	// Nested .gitignore in pkg/ un-ignores .generated.go files (negation)
	subDir := filepath.Join(tmpDir, "pkg")
	require.NoError(t, os.Mkdir(subDir, 0o755))
	err = os.WriteFile(filepath.Join(subDir, ".gitignore"), []byte("!*.generated.go\n"), 0o644)
	require.NoError(t, err)

	// Root-level generated file (should be ignored by root .gitignore)
	err = os.WriteFile(filepath.Join(tmpDir, "root.generated.go"), []byte("package main\nfunc RootGen() {}\n"), 0o644)
	require.NoError(t, err)

	// pkg/ generated file (should be UN-ignored by nested negation)
	err = os.WriteFile(filepath.Join(subDir, "model.generated.go"), []byte("package pkg\nfunc ModelGen() {}\n"), 0o644)
	require.NoError(t, err)

	// A normal Go file in root (should be ingested)
	err = os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\nfunc Main() {}\n"), 0o644)
	require.NoError(t, err)

	store := graph.NewMemoryStore()
	engine := NewEngine(schema, store)
	require.NoError(t, engine.Ingest(tmpDir))

	// main.go should be processed
	_, err = store.GetNode("main/functions/Main/source")
	require.NoError(t, err, "main.go should be processed")

	// root.generated.go should be excluded (root .gitignore: *.generated.go)
	_, err = store.GetNode("root.generated/functions/RootGen/source")
	assert.Error(t, err, "root.generated.go should be excluded by root .gitignore")

	// pkg/model.generated.go should be UN-ignored (nested negation: !*.generated.go)
	_, err = store.GetNode("pkg/functions/ModelGen/source")
	require.NoError(t, err, "pkg/model.generated.go should be un-ignored by nested negation")
}

func TestGitignoreMatcher_DoublestarPatterns(t *testing.T) {
	// Unit test for ** pattern matching without full engine
	m := &gitignoreMatcher{
		rootDir: "/tmp/test",
		patterns: []gitignorePattern{
			{pattern: "**/vendor", dirOnly: true},
			{pattern: "docs/**", dirOnly: false},
			{pattern: "src/**/test_*.go", dirOnly: false},
		},
		nested: make(map[string][]gitignorePattern),
	}

	// **/vendor should match vendor dirs at any depth
	assert.True(t, m.Match("vendor", true), "**/vendor should match top-level vendor dir")
	assert.True(t, m.Match("a/vendor", true), "**/vendor should match nested vendor dir")
	assert.True(t, m.Match("a/b/vendor", true), "**/vendor should match deeply nested vendor dir")
	assert.False(t, m.Match("vendor", false), "**/vendor with dirOnly should not match files")

	// docs/** should match anything inside docs
	assert.True(t, m.Match("docs/readme.md", false), "docs/** should match file in docs")
	assert.True(t, m.Match("docs/api/v1.md", false), "docs/** should match nested file in docs")
	assert.False(t, m.Match("other/readme.md", false), "docs/** should not match file outside docs")

	// src/**/test_*.go should match test files at any depth under src
	assert.True(t, m.Match("src/test_foo.go", false), "src/**/test_*.go should match direct child")
	assert.True(t, m.Match("src/pkg/test_bar.go", false), "src/**/test_*.go should match nested child")
	assert.True(t, m.Match("src/a/b/test_baz.go", false), "src/**/test_*.go should match deep child")
	assert.False(t, m.Match("src/pkg/main.go", false), "src/**/test_*.go should not match non-test file")
}

func TestGitignoreMatcher_NestedDeterministicOrder(t *testing.T) {
	// Verify that nested gitignore evaluation is deterministic: deeper dirs
	// override shallower ones, evaluated shallowest-first so deeper wins.
	m := &gitignoreMatcher{
		rootDir: "/tmp/test",
		patterns: []gitignorePattern{
			{pattern: "*.txt"},
		},
		nested: map[string][]gitignorePattern{
			"a": {
				{pattern: "*.txt", negate: false}, // a/.gitignore: ignore .txt
			},
			"a/b": {
				{pattern: "*.txt", negate: true}, // a/b/.gitignore: un-ignore .txt
			},
		},
	}

	// a/foo.txt — ignored by root AND by a/.gitignore
	assert.True(t, m.Match("a/foo.txt", false), "a/foo.txt should be ignored")

	// a/b/bar.txt — ignored by root, but a/b/.gitignore negates it (deeper wins)
	assert.False(t, m.Match("a/b/bar.txt", false), "a/b/bar.txt should be un-ignored by deeper nested negation")
}

func TestEngine_NoGitignoreOptOut(t *testing.T) {
	// When --no-gitignore is set, .gitignore should be ignored
	schema := loadGoSchema(t)
	tmpDir := t.TempDir()

	err := os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("secret.txt\n"), 0o644)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(tmpDir, "secret.txt"), []byte("top secret"), 0o644)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\nfunc Main() {}\n"), 0o644)
	require.NoError(t, err)

	store := graph.NewMemoryStore()
	engine := NewEngine(schema, store)
	engine.RespectGitignore = false // opt-out
	require.NoError(t, engine.Ingest(tmpDir))

	// secret.txt SHOULD be ingested when gitignore is disabled
	_, err = store.GetNode("_project_files/secret.txt")
	require.NoError(t, err, "secret.txt should be ingested when gitignore disabled")
}
