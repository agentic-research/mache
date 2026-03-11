package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDetectProjectLanguages_Mixed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Create files for multiple languages
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "util.go"), []byte("package main"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "app.ts"), []byte("const x = 1"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte("resource {}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "data.json"), []byte("{}"), 0o644)) // not a language

	counts, err := detectProjectLanguages(dir)
	require.NoError(t, err)

	assert.Equal(t, 2, counts["go"])
	assert.Equal(t, 1, counts["typescript"])
	assert.Equal(t, 1, counts["terraform"])
	assert.Zero(t, counts["json"]) // json is not a language in DetectLanguageFromExt
}

func TestDetectProjectLanguages_SkipsHiddenDirs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Hidden dir with Go files should be skipped
	hidden := filepath.Join(dir, ".hidden")
	require.NoError(t, os.MkdirAll(hidden, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(hidden, "secret.go"), []byte("package x"), 0o644))

	// node_modules should be skipped
	nm := filepath.Join(dir, "node_modules")
	require.NoError(t, os.MkdirAll(nm, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(nm, "dep.js"), []byte("module.exports"), 0o644))

	// Only this file should be counted
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.py"), []byte("print()"), 0o644))

	counts, err := detectProjectLanguages(dir)
	require.NoError(t, err)

	assert.Equal(t, 1, counts["python"])
	assert.Zero(t, counts["go"])
	assert.Zero(t, counts["javascript"])
}

func TestDetectProjectLanguages_Subdirectories(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Files in subdirectories should be counted
	sub := filepath.Join(dir, "cmd", "api")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sub, "main.go"), []byte("package main"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lib.go"), []byte("package lib"), 0o644))

	counts, err := detectProjectLanguages(dir)
	require.NoError(t, err)

	assert.Equal(t, 2, counts["go"])
}

func TestDetectProjectLanguages_Empty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	counts, err := detectProjectLanguages(dir)
	require.NoError(t, err)
	assert.Empty(t, counts)
}

func TestSourceCodePresets_SubsetOfPresetSchemas(t *testing.T) {
	t.Parallel()
	// Every source code preset must exist in the embedded presets
	for lang, key := range sourceCodePresets {
		_, err := loadPresetSchema(key)
		assert.NoErrorf(t, err, "sourceCodePresets[%q] references missing preset %q", lang, key)
	}
}

func TestInferDirSchema_SinglePresetLanguage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Pure Go project — should use preset directly (no namespace wrapper)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}"), 0o644))

	topo, err := inferDirSchema(dir)
	require.NoError(t, err)
	require.NotNil(t, topo)

	// Should NOT have a "go" namespace node — preset returned directly
	for _, n := range topo.Nodes {
		assert.NotEqual(t, "go", n.Name, "single-language preset should not be namespace-wrapped")
	}
}

func TestInferDirSchema_MultiLanguage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Go + TypeScript — Go gets preset, TS gets inference
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "app.ts"), []byte("export function hello() { return 1 }"), 0o644))

	topo, err := inferDirSchema(dir)
	require.NoError(t, err)
	require.NotNil(t, topo)

	// Should have namespace nodes
	names := make(map[string]bool)
	for _, n := range topo.Nodes {
		names[n.Name] = true
	}
	assert.True(t, names["go"], "expected 'go' namespace node")
	// typescript may or may not produce FCA results from 1 file, but go should be there
}

func TestInferDirSchema_NoSourceFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Only non-source files
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# hi"), 0o644))

	topo, err := inferDirSchema(dir)
	require.NoError(t, err)
	require.NotNil(t, topo)
	assert.Empty(t, topo.Nodes)
}
