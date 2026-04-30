# Unified Language Registry Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace 10 independent language/extension switch statements with a single `internal/lang` registry that every consumer derives from — zero duplication, zero drift.

**Architecture:** A `Language` struct holds all per-language data (name, extensions, grammar factory, preset schema key, display name, sentinel files). A package-level registry slice built at init time provides derived lookups: `ForExt()`, `ForName()`, `Extensions()`, `IsSourceExt()`. All 9 consumers rewrite to call these lookups. The `internal/lang` package imports tree-sitter grammars (CGO) so `api/` stays pure Go. Circular deps avoided because `lang` depends only on tree-sitter — nothing in `internal/` depends on `lang` upward.

**Tech Stack:** Go, tree-sitter CGO bindings, testify

**Bead:** mache-7gp

______________________________________________________________________

## Current State: 10 Registries, 9+ Bugs

| #   | Registry                  | File:Lines                               | Languages     | Bugs                                                                  |
| --- | ------------------------- | ---------------------------------------- | ------------- | --------------------------------------------------------------------- |
| 1   | `langForExt()`            | `internal/ingest/engine.go:135-176`      | 18            | Duplicate of #2                                                       |
| 2   | `DetectLanguageFromExt()` | `internal/ingest/language.go:28-69`      | 18            | Duplicate of #1                                                       |
| 3   | `GetLanguage()`           | `internal/ingest/engine.go:1686-1727`    | 18            | Third copy (by name)                                                  |
| 4   | `GetLanguageProfile()`    | `internal/ingest/language.go:79-88`      | 1 (hcl)       | 10th switch, not addressed until now                                  |
| 5   | `sourceExtensions`        | `internal/ingest/watcher.go:289-305`     | 14 + .json    | **Missing 8 langs** (.java .c .h .cpp .rb .php .kt .swift .scala etc) |
| 6   | `LanguageForPath()`       | `internal/writeback/validate.go:141-163` | 8             | **Missing 10 langs** (claims "single source of truth")                |
| 7   | `presetSchemas`           | `cmd/schemas.go:15-39`                   | 18 + data     | OK (complete)                                                         |
| 8   | `sourceCodePresets`       | `cmd/infer.go:22-41`                     | 18            | OK (complete)                                                         |
| 9   | mount.go infer switch     | `cmd/mount.go:179-216`                   | 16            | **Missing elixir, toml**                                              |
| 10  | `detectProjectType()`     | `cmd/config.go:408-417`                  | 3 (go/py/sql) | **Missing 15 langs**                                                  |

**Additional missed consumers** (direct grammar imports, no registry):

- `cmd/build.go:15` — imports `golang` directly
- `internal/linter/linter.go:9` — imports `golang` directly
- `internal/lattice/project_ast.go:53` — uses `"hcl"` as key (must work after rename)

Additional bugs in `ingestTreeSitter` vs `ingestTreeSitterParallel`:

- Broken file ID uses basename (collision) vs SHA256 (correct)
- Missing `_project_files` fallback in sequential path
- Missing `RecordFile` in sequential path

## File Structure

### New files

- `internal/lang/lang.go` — `Language` struct, registry, derived lookups
- `internal/lang/lang_test.go` — exhaustive tests for registry completeness

### Modified files (consumers → one-liner rewrites)

- `internal/ingest/engine.go` — delete `langForExt()`, `GetLanguage()`; call `lang.ForExt()`, `lang.ForName()`
- `internal/ingest/language.go` — delete `DetectLanguageFromExt()`; re-export as thin wrapper over `lang.ForExt()`
- `internal/ingest/watcher.go` — delete `sourceExtensions`; call `lang.IsSourceExt()`
- `internal/writeback/validate.go` — delete `LanguageForPath()`; call `lang.ForPath()`
- `cmd/schemas.go` — derive `presetSchemas` from `lang.Registry`
- `cmd/infer.go` — derive `sourceCodePresets` from `lang.Registry`
- `cmd/mount.go` — replace infer switch with `lang.ForExt()`
- `cmd/config.go` — replace `detectProjectType` hardcoded exts with `lang.Extensions()`

### Modified files (ingest dedup)

- `internal/ingest/engine.go` — extract `processTreeSitterResult()`, fix 3 bugs

______________________________________________________________________

### Task 1: Create `internal/lang` package — the single source of truth

**Files:**

- Create: `internal/lang/lang.go`

- Create: `internal/lang/lang_test.go`

- [ ] **Step 1: Write the failing test — registry completeness**

```go
// internal/lang/lang_test.go
package lang

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistry_Has18Languages(t *testing.T) {
	expected := []string{
		"go", "python", "javascript", "typescript", "sql", "terraform",
		"yaml", "rust", "toml", "elixir", "java", "c", "cpp",
		"ruby", "php", "kotlin", "swift", "scala",
	}
	for _, name := range expected {
		l := ForName(name)
		require.NotNil(t, l, "missing language: %s", name)
		assert.Equal(t, name, l.Name)
		assert.NotNil(t, l.Grammar(), "nil grammar for %s", name)
		assert.NotEmpty(t, l.Extensions, "no extensions for %s", name)
	}
}

func TestForExt_AllExtensions(t *testing.T) {
	cases := map[string]string{
		".go": "go", ".py": "python", ".js": "javascript",
		".ts": "typescript", ".tsx": "typescript",
		".sql": "sql", ".tf": "terraform", ".hcl": "terraform",
		".yaml": "yaml", ".yml": "yaml",
		".rs": "rust", ".toml": "toml",
		".ex": "elixir", ".exs": "elixir",
		".java": "java", ".c": "c", ".h": "c",
		".cpp": "cpp", ".cc": "cpp", ".cxx": "cpp",
		".hpp": "cpp", ".hxx": "cpp", ".hh": "cpp",
		".rb": "ruby", ".php": "php",
		".kt": "kotlin", ".kts": "kotlin",
		".swift": "swift", ".scala": "scala", ".sc": "scala",
	}
	for ext, wantName := range cases {
		l := ForExt(ext)
		require.NotNil(t, l, "no language for ext %s", ext)
		assert.Equal(t, wantName, l.Name, "wrong language for ext %s", ext)
	}
}

func TestForExt_Unknown(t *testing.T) {
	assert.Nil(t, ForExt(".xyz"))
	assert.Nil(t, ForExt(".md"))
	assert.Nil(t, ForExt(""))
}

func TestForName_Unknown(t *testing.T) {
	assert.Nil(t, ForName("brainfuck"))
}

func TestIsSourceExt(t *testing.T) {
	assert.True(t, IsSourceExt(".go"))
	assert.True(t, IsSourceExt(".java"))
	assert.True(t, IsSourceExt(".swift"))
	assert.True(t, IsSourceExt(".json")) // special case: data format
	assert.False(t, IsSourceExt(".md"))
	assert.False(t, IsSourceExt(".o"))
}

func TestExtensions_ReturnsAll(t *testing.T) {
	exts := Extensions()
	assert.Contains(t, exts, ".go")
	assert.Contains(t, exts, ".scala")
	assert.Contains(t, exts, ".hh")
	assert.True(t, len(exts) >= 28, "expected at least 28 extensions, got %d", len(exts))
}

func TestForPath(t *testing.T) {
	l := ForPath("/foo/bar/main.go")
	require.NotNil(t, l)
	assert.Equal(t, "go", l.Name)

	assert.Nil(t, ForPath("/foo/README.md"))
}

func TestNoDuplicateExtensions(t *testing.T) {
	seen := map[string]string{}
	for _, l := range Registry {
		for _, ext := range l.Extensions {
			if prev, ok := seen[ext]; ok {
				t.Errorf("extension %s claimed by both %s and %s", ext, prev, l.Name)
			}
			seen[ext] = l.Name
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `task test -- -run TestRegistry ./internal/lang/`
Expected: FAIL — package doesn't exist yet

- [ ] **Step 3: Write minimal implementation**

```go
// internal/lang/lang.go
package lang

import (
	"path/filepath"
	"sort"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	treec "github.com/smacker/go-tree-sitter/c"
	"github.com/smacker/go-tree-sitter/cpp"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/hcl"
	"github.com/smacker/go-tree-sitter/java"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/kotlin"
	"github.com/smacker/go-tree-sitter/php"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/ruby"
	"github.com/smacker/go-tree-sitter/rust"
	"github.com/smacker/go-tree-sitter/scala"
	"github.com/smacker/go-tree-sitter/sql"
	"github.com/smacker/go-tree-sitter/swift"
	"github.com/smacker/go-tree-sitter/toml"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
	"github.com/smacker/go-tree-sitter/yaml"

	"github.com/agentic-research/mache/internal/treesitter/elixir"
)

// Language is the single source of truth for a supported language.
type Language struct {
	Name          string                              // canonical name: "go", "python", "terraform"
	Aliases       []string                            // backward-compat names: e.g. "hcl" for terraform
	DisplayName   string                              // human label: "Go", "Python", "HCL/Terraform"
	Extensions    []string                            // file extensions including dot: ".go", ".py"
	Grammar       func() *sitter.Language             // tree-sitter grammar factory (lazy, CGO-safe)
	PresetSchema  string                              // embedded schema key (empty = no preset)
	SentinelFiles []string                            // files that identify a project: "go.mod", "Cargo.toml"
	EnrichNode    func(n *sitter.Node, rec map[string]any) // language-specific AST enrichment (nil for most)
}

// Registry is the authoritative list of all supported languages.
// Add a language here and every consumer picks it up automatically.
var Registry = []Language{
	{Name: "go", DisplayName: "Go", Extensions: []string{".go"}, Grammar: golang.GetLanguage, PresetSchema: "go", SentinelFiles: []string{"go.mod", "go.sum"}},
	{Name: "python", DisplayName: "Python", Extensions: []string{".py"}, Grammar: python.GetLanguage, PresetSchema: "python", SentinelFiles: []string{"pyproject.toml", "requirements.txt", "setup.py"}},
	{Name: "javascript", DisplayName: "JavaScript", Extensions: []string{".js"}, Grammar: javascript.GetLanguage, PresetSchema: "javascript", SentinelFiles: []string{"package.json"}},
	{Name: "typescript", DisplayName: "TypeScript", Extensions: []string{".ts", ".tsx"}, Grammar: typescript.GetLanguage, PresetSchema: "typescript"},
	{Name: "sql", DisplayName: "SQL", Extensions: []string{".sql"}, Grammar: sql.GetLanguage, PresetSchema: "sql"},
	{Name: "terraform", Aliases: []string{"hcl"}, DisplayName: "HCL/Terraform", Extensions: []string{".tf", ".hcl"}, Grammar: hcl.GetLanguage, PresetSchema: "terraform", EnrichNode: enrichHCLNode},
	{Name: "yaml", DisplayName: "YAML", Extensions: []string{".yaml", ".yml"}, Grammar: yaml.GetLanguage, PresetSchema: "yaml"},
	{Name: "rust", DisplayName: "Rust", Extensions: []string{".rs"}, Grammar: rust.GetLanguage, PresetSchema: "rust", SentinelFiles: []string{"Cargo.toml"}},
	{Name: "toml", DisplayName: "TOML", Extensions: []string{".toml"}, Grammar: toml.GetLanguage, PresetSchema: "toml"},
	{Name: "elixir", DisplayName: "Elixir", Extensions: []string{".ex", ".exs"}, Grammar: elixir.GetLanguage, PresetSchema: "elixir", SentinelFiles: []string{"mix.exs"}},
	{Name: "java", DisplayName: "Java", Extensions: []string{".java"}, Grammar: java.GetLanguage, PresetSchema: "java", SentinelFiles: []string{"pom.xml", "build.gradle"}},
	{Name: "c", DisplayName: "C", Extensions: []string{".c", ".h"}, Grammar: treec.GetLanguage, PresetSchema: "c"},
	{Name: "cpp", DisplayName: "C++", Extensions: []string{".cpp", ".cc", ".cxx", ".hpp", ".hxx", ".hh"}, Grammar: cpp.GetLanguage, PresetSchema: "cpp", SentinelFiles: []string{"CMakeLists.txt"}},
	{Name: "ruby", DisplayName: "Ruby", Extensions: []string{".rb"}, Grammar: ruby.GetLanguage, PresetSchema: "ruby", SentinelFiles: []string{"Gemfile"}},
	{Name: "php", DisplayName: "PHP", Extensions: []string{".php"}, Grammar: php.GetLanguage, PresetSchema: "php", SentinelFiles: []string{"composer.json"}},
	{Name: "kotlin", DisplayName: "Kotlin", Extensions: []string{".kt", ".kts"}, Grammar: kotlin.GetLanguage, PresetSchema: "kotlin"},
	{Name: "swift", DisplayName: "Swift", Extensions: []string{".swift"}, Grammar: swift.GetLanguage, PresetSchema: "swift", SentinelFiles: []string{"Package.swift"}},
	{Name: "scala", DisplayName: "Scala", Extensions: []string{".scala", ".sc"}, Grammar: scala.GetLanguage, PresetSchema: "scala", SentinelFiles: []string{"build.sbt"}},
}

// Derived indexes — built once at init, never mutated.
var (
	byExt  map[string]*Language
	byName map[string]*Language
	srcSet map[string]bool // all extensions + .json
)

func init() {
	byExt = make(map[string]*Language, 32)
	byName = make(map[string]*Language, len(Registry))
	srcSet = make(map[string]bool, 32)

	for i := range Registry {
		l := &Registry[i]
		byName[l.Name] = l
		for _, alias := range l.Aliases {
			byName[alias] = l // backward compat: ForName("hcl") → terraform
		}
		for _, ext := range l.Extensions {
			byExt[ext] = l
			srcSet[ext] = true
		}
	}
	// Data format extensions are source files but not tree-sitter languages.
	srcSet[".json"] = true
}

// ForExt returns the language for a file extension (including dot), or nil.
// Case-insensitive to handle ".Go", ".PY" etc.
func ForExt(ext string) *Language {
	return byExt[strings.ToLower(ext)]
}

// ForName returns the language by canonical name, or nil.
func ForName(name string) *Language {
	return byName[name]
}

// ForPath returns the language for a file path (by extension), or nil.
func ForPath(path string) *Language {
	return byExt[strings.ToLower(filepath.Ext(path))]
}

// IsSourceExt returns true if the extension is a recognized source file
// (tree-sitter languages + .json).
func IsSourceExt(ext string) bool {
	return srcSet[ext]
}

// Extensions returns all recognized file extensions in sorted order.
func Extensions() []string {
	out := make([]string, 0, len(srcSet))
	for ext := range srcSet {
		out = append(out, ext)
	}
	sort.Strings(out)
	return out
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `task test -- -run 'TestRegistry|TestForExt|TestForName|TestIsSourceExt|TestExtensions|TestForPath|TestNoDuplicate' ./internal/lang/`
Expected: ALL PASS

- [ ] **Step 5: Commit**

```bash
git add internal/lang/
git commit -m "feat: internal/lang — single source of truth for all language registries"
```

______________________________________________________________________

### Task 2: Wire `internal/ingest` — delete 3 duplicates

**Files:**

- Modify: `internal/ingest/engine.go:135-176` (delete `langForExt`), `:1686-1727` (delete `GetLanguage`)

- Modify: `internal/ingest/language.go:28-69` (rewrite `DetectLanguageFromExt` as thin wrapper)

- Modify: `internal/ingest/watcher.go:289-305` (delete `sourceExtensions`, `isSourceFile`)

- Modify: `internal/ingest/engine_test.go`, `watcher_test.go` as needed

- [ ] **Step 1: Replace `langForExt` with `lang.ForExt`**

In `engine.go`, delete the `langForExt` function (lines 135-176). Replace all call sites:

```go
// Before:
lang, langName := langForExt(ext)
// After:
l := lang.ForExt(ext)
var grammar *sitter.Language
var langName string
if l != nil {
    grammar, langName = l.Grammar(), l.Name
}
```

There are 2 call sites: `ingestTreeSitterParallel` (~line 550) and `ingestFile` (~line 759).

- [ ] **Step 2: Replace `GetLanguage` with `lang.ForName`**

Delete `GetLanguage` (lines 1686-1727). Replace the one external call site:

```go
// cmd/mount.go:922 — the only external caller
// Before:
grammar := ingest.GetLanguage(langName)
// After:
l := lang.ForName(langName)
if l != nil { grammar = l.Grammar() }
```

- [ ] **Step 2b: Replace `GetLanguageProfile` with `lang.ForName`**

Delete `GetLanguageProfile` (language.go:79-88). Replace call sites:

```go
// Before:
profile := GetLanguageProfile(langName)
if profile != nil { profile.EnrichNode(n, rec) }
// After:
l := lang.ForName(langName)
if l != nil && l.EnrichNode != nil { l.EnrichNode(n, rec) }
```

Move `enrichHCLNode` function from `language.go` to `internal/lang/lang.go` (or keep in `language.go` and pass as function reference — prefer the latter to avoid pulling HCL-specific logic into `lang`).

- [ ] **Step 3: Rewrite `DetectLanguageFromExt` as thin wrapper**

In `language.go`, replace the 40-line switch with:

```go
func DetectLanguageFromExt(ext string) (langName string, grammar *sitter.Language, ok bool) {
	l := lang.ForExt(ext)
	if l == nil {
		return "", nil, false
	}
	return l.Name, l.Grammar(), true
}
```

Keep it exported for backward compat — external callers (lattice, cmd) use it.

- [ ] **Step 4: Replace `sourceExtensions` and `isSourceFile`**

In `watcher.go`, delete the `sourceExtensions` map (lines 289-305) and rewrite:

```go
func isSourceFile(path string) bool {
	return lang.IsSourceExt(filepath.Ext(path))
}
```

This fixes the 8-language gap bug immediately.

- [ ] **Step 5: Run full test suite**

Run: `task test`
Expected: ALL PASS

- [ ] **Step 6: Commit**

```bash
git add internal/ingest/ internal/lang/
git commit -m "refactor: wire internal/ingest to lang registry — delete 3 duplicate switches"
```

______________________________________________________________________

### Task 3: Wire `internal/writeback` — fix 10-language gap

**Files:**

- Modify: `internal/writeback/validate.go:141-163`

- [ ] **Step 1: Write test for newly-supported languages**

```go
func TestLanguageForPath_AllLanguages(t *testing.T) {
	cases := map[string]bool{
		"main.go": true, "app.py": true, "index.js": true,
		"app.ts": true, "query.sql": true, "main.tf": true,
		"config.yaml": true, "lib.rs": true,
		// These were previously broken:
		"config.toml": true, "mix.ex": true, "App.java": true,
		"main.c": true, "lib.cpp": true, "app.rb": true,
		"index.php": true, "Main.kt": true, "App.swift": true,
		"Main.scala": true,
		// Non-languages:
		"README.md": false, "data.csv": false,
	}
	for file, wantNonNil := range cases {
		result := LanguageForPath(file)
		if wantNonNil {
			assert.NotNil(t, result, "expected language for %s", file)
		} else {
			assert.Nil(t, result, "expected nil for %s", file)
		}
	}
}
```

- [ ] **Step 2: Rewrite `LanguageForPath`**

Replace the 22-line switch (lines 141-163) with:

```go
func LanguageForPath(filePath string) *sitter.Language {
	l := lang.ForPath(filePath)
	if l == nil {
		return nil
	}
	return l.Grammar()
}
```

- [ ] **Step 3: Run tests**

Run: `task test -- -run TestLanguageForPath ./internal/writeback/`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/writeback/
git commit -m "fix: LanguageForPath now supports all 18 languages via lang registry"
```

______________________________________________________________________

### Task 4: Wire `cmd/` consumers — schemas, infer, mount, config

**Files:**

- Modify: `cmd/schemas.go:15-39`

- Modify: `cmd/infer.go:22-41`

- Modify: `cmd/mount.go:179-216`

- Modify: `cmd/config.go:408-417`

- [ ] **Step 1: Derive `presetSchemas` from registry**

In `schemas.go`, replace the hardcoded map with:

```go
var presetSchemas map[string]string

func init() {
	presetSchemas = make(map[string]string)
	for _, l := range lang.Registry {
		if l.PresetSchema != "" {
			presetSchemas[l.Name] = "schemas/" + l.PresetSchema + ".json"
		}
	}
	// Data-format presets (not auto-detected from file extensions)
	presetSchemas["cli"] = "schemas/cli.json"
	presetSchemas["mcp"] = "schemas/mcp.json"
	presetSchemas["mcp-registry"] = "schemas/mcp-registry.json"
}
```

- [ ] **Step 2: Derive `sourceCodePresets` from registry**

In `infer.go`, replace the hardcoded map with:

```go
var sourceCodePresets map[string]string

func init() {
	sourceCodePresets = make(map[string]string)
	for _, l := range lang.Registry {
		if l.PresetSchema != "" {
			sourceCodePresets[l.Name] = l.PresetSchema
		}
	}
}
```

- [ ] **Step 3: Replace mount.go infer switch**

Replace the 40-line switch (lines 179-216) with:

```go
l := lang.ForExt(ext)
if l != nil {
	inferred, err = inferFromTreeSitterFile(inf, dataPath, l.Grammar(), l.DisplayName)
}
```

Keep the `.db` and `.git` cases as-is (they're not language-specific).

- [ ] **Step 4: Replace `detectProjectType` with registry-driven detection**

In `config.go`, replace the hardcoded 3-language switch with:

```go
ext := strings.ToLower(filepath.Ext(name))
if l := lang.ForExt(ext); l != nil {
	counts[l.Name]++
}
```

Also add sentinel file detection using `Language.SentinelFiles`.

- [ ] **Step 5: Run full test suite**

Run: `task test`
Expected: ALL PASS — existing tests for `TestDetectProjectType_GoProject` etc still pass, plus new languages now detected

- [ ] **Step 6: Commit**

```bash
git add cmd/
git commit -m "refactor: wire cmd/ consumers to lang registry — all derived, zero hardcoded"
```

______________________________________________________________________

### Task 5: Deduplicate `ingestTreeSitter` — fix 3 bugs (depends on Task 2)

**Files:**

- Modify: `internal/ingest/engine.go`

- [ ] **Step 1: Write tests for the 3 bugs**

```go
func TestIngestTreeSitter_BrokenFileID_UsesHash(t *testing.T) {
	// Two broken files with same basename in different dirs
	// should NOT overwrite each other
	// ... create temp dir with dir1/broken.go and dir2/broken.go
	// ... both with invalid syntax
	// ... assert both nodes exist with different IDs
}

func TestIngestTreeSitter_UnmatchedFile_RoutesToProjectFiles(t *testing.T) {
	// A file whose language has no matching schema nodes
	// should end up in _project_files/, not silently dropped
}

func TestReIngestFile_RecordsFileMetadata(t *testing.T) {
	// After ReIngestFile, the file should be recorded in the index
	// so subsequent re-ingest with same mtime is skipped
}
```

- [ ] **Step 2: Extract `processTreeSitterResult` method**

Create a shared struct and method that both paths use:

```go
type treeSitterResult struct {
	path      string
	realPath  string
	content   []byte
	tree      *sitter.Tree
	lang      *sitter.Language
	langName  string
	modTime   time.Time
	context   []byte
	readErr   error
	parseErr  error
}

func (e *Engine) processTreeSitterResult(r *treeSitterResult) error {
	// 1. Handle read errors
	// 2. Handle parse errors → BROKEN_ with SHA256(fullpath) ID
	// 3. Filter schema nodes by language
	// 4. No applicable nodes → route to _project_files
	// 5. processNode for each applicable schema node
	// 6. No nodes produced → route to _project_files
	// 7. Atomic swap via ReplaceFileNodes
	// 8. RecordFile for incremental re-ingestion
	// 9. Extract and store address refs
}
```

- [ ] **Step 3: Rewrite `ingestTreeSitter` (sequential) to use it**

```go
func (e *Engine) ingestTreeSitter(path string, grammar *sitter.Language, langName string, modTime time.Time) error {
	r := &treeSitterResult{langName: langName, lang: grammar, modTime: modTime}
	// ... resolve path, read file, parse ...
	return e.processTreeSitterResult(r)
}
```

- [ ] **Step 4: Rewrite `ingestTreeSitterParallel` phase 2 to use it**

The parallel path's phase 2 loop (lines 608-740) becomes:

```go
for _, result := range results {
	if err := e.processTreeSitterResult(&result); err != nil {
		if firstErr == nil { firstErr = err }
	}
}
```

- [ ] **Step 5: Run full test suite**

Run: `task test`
Expected: ALL PASS — the 3 new bug-fix tests pass, all existing tests pass

- [ ] **Step 6: Commit**

```bash
git add internal/ingest/
git commit -m "fix: deduplicate ingestTreeSitter — fix broken file ID collision, missing fallback, missing RecordFile"
```

______________________________________________________________________

### Task 6: Delete dead code, final cleanup

**Files:**

- Modify: `internal/ingest/engine.go` — remove unused tree-sitter imports

- Modify: `internal/ingest/language.go` — remove unused tree-sitter imports (now in `lang`)

- Modify: `internal/writeback/validate.go` — remove unused tree-sitter imports

- Modify: `cmd/mount.go` — remove unused tree-sitter imports

- Modify: `cmd/build.go` — replace direct `golang.GetLanguage()` with `lang.ForName("go").Grammar()`

- Modify: `internal/linter/linter.go` — replace direct `golang.GetLanguage()` with `lang.ForName("go").Grammar()`

- Modify: `internal/lattice/project_ast.go` — ensure `"hcl"` key works via alias

- [ ] **Step 1: Remove all tree-sitter imports from consumers**

Every file that previously imported grammar packages directly (golang, python, rust, etc.) should now only import `internal/lang`. The grammar imports live exclusively in `internal/lang/lang.go`.

Exception: `internal/ingest/language.go` may still need direct imports if `GetLanguageProfile` references grammar-specific logic. Check and clean.

- [ ] **Step 2: Run lint**

Run: `task lint`
Expected: No unused import errors

- [ ] **Step 3: Run full test suite one final time**

Run: `task check`
Expected: fmt + vet + lint + test + validate ALL PASS

- [ ] **Step 4: Commit**

```bash
git add .
git commit -m "chore: remove dead tree-sitter imports — all grammars via internal/lang"
```

______________________________________________________________________

## Verification Checklist

After all tasks complete, verify:

- [ ] `grep -r "case \".go\"" internal/ cmd/` returns ONLY `internal/lang/lang.go` (no more switches)
- [ ] `grep -r "GetLanguage()" internal/ingest/` returns zero results (deleted)
- [ ] `grep -r "GetLanguageProfile" internal/` returns zero results (deleted)
- [ ] `grep -r "langForExt" internal/` returns zero results (deleted)
- [ ] `grep -r "sourceExtensions" internal/` returns zero results (deleted)
- [ ] `grep -r "golang.GetLanguage" cmd/ internal/` returns zero results (all via `lang` now)
- [ ] `lang.ForName("hcl")` returns terraform language (backward compat alias)
- [ ] `task check` passes (fmt + vet + lint + test + validate)
- [ ] Adding a new language requires editing exactly ONE file: `internal/lang/lang.go`
