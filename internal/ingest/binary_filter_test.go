package ingest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsBinaryFile(t *testing.T) {
	tmpDir := t.TempDir()

	// Text file — should NOT be detected as binary
	textFile := filepath.Join(tmpDir, "hello.txt")
	require.NoError(t, os.WriteFile(textFile, []byte("hello world\n"), 0o644))
	assert.False(t, isBinaryFile(textFile), "plain text should not be binary")

	// Go source — should NOT be detected as binary
	goFile := filepath.Join(tmpDir, "main.go")
	require.NoError(t, os.WriteFile(goFile, []byte("package main\n\nfunc main() {}\n"), 0o644))
	assert.False(t, isBinaryFile(goFile), "Go source should not be binary")

	// Empty file — should NOT be detected as binary
	emptyFile := filepath.Join(tmpDir, "empty")
	require.NoError(t, os.WriteFile(emptyFile, []byte{}, 0o644))
	assert.False(t, isBinaryFile(emptyFile), "empty file should not be binary")

	// Binary with null bytes (like a compiled executable)
	binFile := filepath.Join(tmpDir, "program")
	require.NoError(t, os.WriteFile(binFile, []byte{0x7f, 'E', 'L', 'F', 0, 0, 0}, 0o644))
	assert.True(t, isBinaryFile(binFile), "ELF binary should be detected")

	// Mach-O magic (macOS binary)
	machoFile := filepath.Join(tmpDir, "program.macho")
	require.NoError(t, os.WriteFile(machoFile, []byte{0xfe, 0xed, 0xfa, 0xce, 0, 0}, 0o644))
	assert.True(t, isBinaryFile(machoFile), "Mach-O binary should be detected")

	// SQLite file header — has null byte at position 15
	sqliteFile := filepath.Join(tmpDir, "data.db")
	header := []byte("SQLite format 3\x00")
	require.NoError(t, os.WriteFile(sqliteFile, header, 0o644))
	assert.True(t, isBinaryFile(sqliteFile), "SQLite file contains null byte (but .db is handled before this check)")

	// Object file (.o) content
	objFile := filepath.Join(tmpDir, "foo.o")
	require.NoError(t, os.WriteFile(objFile, []byte{0xcf, 0xfa, 0xed, 0xfe, 0, 0}, 0o644))
	assert.True(t, isBinaryFile(objFile), ".o file should be detected as binary")

	// Non-existent file — should return false (not crash)
	assert.False(t, isBinaryFile(filepath.Join(tmpDir, "nope")), "missing file should return false")
}

func TestEngine_Ingest_SkipsBinaryFiles(t *testing.T) {
	schema := loadGoSchema(t)

	tmpDir := t.TempDir()

	// 1. Go source file — should be ingested
	goFile := filepath.Join(tmpDir, "main.go")
	require.NoError(t, os.WriteFile(goFile, []byte("package main\n\nfunc Hello() {}\n"), 0o644))

	// 2. Text file (unknown extension) — should be ingested as raw
	txtFile := filepath.Join(tmpDir, "notes.txt")
	require.NoError(t, os.WriteFile(txtFile, []byte("some notes"), 0o644))

	// 3. Compiled binary (no extension) — should be SKIPPED
	binFile := filepath.Join(tmpDir, "mybinary")
	require.NoError(t, os.WriteFile(binFile, []byte{0x7f, 'E', 'L', 'F', 0, 0, 0, 0}, 0o644))

	// 4. Compiled binary with extension — should be SKIPPED
	binExe := filepath.Join(tmpDir, "test.exe")
	require.NoError(t, os.WriteFile(binExe, []byte{'M', 'Z', 0, 0, 0, 0}, 0o644))

	// 5. Object file in subdir — should be SKIPPED
	subDir := filepath.Join(tmpDir, "lib")
	require.NoError(t, os.MkdirAll(subDir, 0o755))
	objFile := filepath.Join(subDir, "helper.o")
	require.NoError(t, os.WriteFile(objFile, []byte{0xcf, 0xfa, 0xed, 0xfe, 0, 0}, 0o644))

	// 6. Image file — should be SKIPPED
	pngFile := filepath.Join(tmpDir, "logo.png")
	// PNG header has null bytes
	require.NoError(t, os.WriteFile(pngFile, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0}, 0o644))

	store := graph.NewMemoryStore()
	engine := NewEngine(schema, store)
	require.NoError(t, engine.Ingest(tmpDir))

	// Go file was ingested (tree-sitter processed)
	_, err := store.GetNode("main/functions/Hello/source")
	require.NoError(t, err, "Go function Hello() should be ingested")

	// Text file was ingested as raw under _project_files/
	_, err = store.GetNode("_project_files/notes.txt")
	require.NoError(t, err, "text file should be ingested under _project_files/")

	// _project_files root node exists
	pfNode, err := store.GetNode("_project_files")
	require.NoError(t, err, "_project_files root should exist")
	assert.True(t, pfNode.Mode.IsDir(), "_project_files should be a directory")

	// Binary files should NOT be ingested
	_, err = store.GetNode("mybinary")
	assert.ErrorIs(t, err, graph.ErrNotFound, "compiled binary should NOT be ingested")

	_, err = store.GetNode("test.exe")
	assert.ErrorIs(t, err, graph.ErrNotFound, "binary .exe should NOT be ingested")

	_, err = store.GetNode("lib/helper.o")
	assert.ErrorIs(t, err, graph.ErrNotFound, ".o object file should NOT be ingested")

	_, err = store.GetNode("logo.png")
	assert.ErrorIs(t, err, graph.ErrNotFound, "PNG image should NOT be ingested")
}

func TestEngine_Ingest_SkipsBinaryInBuildDirs(t *testing.T) {
	schema := loadGoSchema(t)

	tmpDir := t.TempDir()

	// Source file at root
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpDir, "lib.go"),
		[]byte("package lib\n\nfunc Run() {}\n"), 0o644))

	// target/ directory (Rust build output) — entire dir should be skipped
	targetDir := filepath.Join(tmpDir, "target", "debug", "deps")
	require.NoError(t, os.MkdirAll(targetDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(targetDir, "bench.rcgu.o"),
		[]byte{0xcf, 0xfa, 0xed, 0xfe, 0, 0}, 0o644))

	// node_modules/ — should be skipped
	nmDir := filepath.Join(tmpDir, "node_modules", "foo")
	require.NoError(t, os.MkdirAll(nmDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(nmDir, "index.js"),
		[]byte("module.exports = 42"), 0o644))

	store := graph.NewMemoryStore()
	engine := NewEngine(schema, store)
	require.NoError(t, engine.Ingest(tmpDir))

	// Source file ingested
	_, err := store.GetNode("lib/functions/Run/source")
	require.NoError(t, err, "lib.go should be ingested")

	// Build artifact dirs completely skipped
	_, err = store.GetNode("target/debug/deps/bench.rcgu.o")
	assert.ErrorIs(t, err, graph.ErrNotFound, "target/ contents should not be ingested")

	_, err = store.GetNode("node_modules/foo/index.js")
	assert.ErrorIs(t, err, graph.ErrNotFound, "node_modules/ contents should not be ingested")
}

func TestEngine_Ingest_MixedLanguages_NoError(t *testing.T) {
	// FCA may infer a Go-specific schema (with Go tree-sitter selectors).
	// When ingestion encounters non-Go source files (.py, .js, .yaml),
	// the Go selector should be gracefully skipped, not crash.
	// Language-mismatched files should appear under _project_files/.
	schema := loadGoSchema(t)

	tmpDir := t.TempDir()

	// Go file — matches the schema's selectors
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpDir, "main.go"),
		[]byte("package main\n\nfunc Hello() {}\n"), 0o644))

	// Python file — selector "function_declaration" doesn't exist in Python grammar
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpDir, "helper.py"),
		[]byte("def greet():\n    print('hi')\n"), 0o644))

	// JS file — different grammar, same issue
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpDir, "util.js"),
		[]byte("function add(a, b) { return a + b; }\n"), 0o644))

	// YAML file — no function_declaration at all
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpDir, "config.yaml"),
		[]byte("key: value\n"), 0o644))

	store := graph.NewMemoryStore()
	engine := NewEngine(schema, store)

	// This must NOT error — language mismatches should be skipped gracefully
	err := engine.Ingest(tmpDir)
	require.NoError(t, err, "mixed-language ingestion should not fail")

	// Go file was ingested correctly
	_, err = store.GetNode("main/functions/Hello/source")
	require.NoError(t, err, "Go function Hello() should be ingested")

	// Language-mismatched files routed to _project_files/
	pyNode, err := store.GetNode("_project_files/helper.py")
	require.NoError(t, err, "Python file should appear under _project_files/")
	assert.Equal(t, "def greet():\n    print('hi')\n", string(pyNode.Data))

	jsNode, err := store.GetNode("_project_files/util.js")
	require.NoError(t, err, "JS file should appear under _project_files/")
	assert.Equal(t, "function add(a, b) { return a + b; }\n", string(jsNode.Data))

	yamlNode, err := store.GetNode("_project_files/config.yaml")
	require.NoError(t, err, "YAML file should appear under _project_files/")
	assert.Equal(t, "key: value\n", string(yamlNode.Data))
}

func TestEngine_Ingest_ProjectFilesHierarchy(t *testing.T) {
	// Verify nested raw files preserve directory structure under _project_files/.
	schema := loadGoSchema(t)

	tmpDir := t.TempDir()

	// Go file at root — parsed via tree-sitter
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpDir, "main.go"),
		[]byte("package main\n\nfunc Run() {}\n"), 0o644))

	// Nested non-AST files
	docsDir := filepath.Join(tmpDir, "docs")
	require.NoError(t, os.MkdirAll(docsDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(docsDir, "manual.md"),
		[]byte("# Manual\n"), 0o644))

	configDir := filepath.Join(tmpDir, "configs", "prod")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(configDir, "settings.toml"),
		[]byte("[server]\nport = 8080\n"), 0o644))

	// Root-level non-AST file
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpDir, "README.md"),
		[]byte("# Hello\n"), 0o644))

	store := graph.NewMemoryStore()
	engine := NewEngine(schema, store)
	require.NoError(t, engine.Ingest(tmpDir))

	// _project_files root
	pfNode, err := store.GetNode("_project_files")
	require.NoError(t, err, "_project_files root should exist")
	assert.True(t, pfNode.Mode.IsDir())

	// Root-level file
	readmeNode, err := store.GetNode("_project_files/README.md")
	require.NoError(t, err, "README.md should be under _project_files/")
	assert.Equal(t, "# Hello\n", string(readmeNode.Data))

	// Nested directory structure preserved
	_, err = store.GetNode("_project_files/docs")
	require.NoError(t, err, "docs/ dir should exist under _project_files/")

	manualNode, err := store.GetNode("_project_files/docs/manual.md")
	require.NoError(t, err, "manual.md should preserve nested path")
	assert.Equal(t, "# Manual\n", string(manualNode.Data))

	// Deeply nested
	settingsNode, err := store.GetNode("_project_files/configs/prod/settings.toml")
	require.NoError(t, err, "settings.toml should preserve deep nesting")
	assert.Equal(t, "[server]\nport = 8080\n", string(settingsNode.Data))
}
