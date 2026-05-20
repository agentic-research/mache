package testfixtures

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFixturesSnapshot_CurationFilter_Rust mirrors a typical Rust crate
// layout — Cargo.toml + src/*.rs + target/ build cache + .git/ — and
// asserts the curation filter copies the source + manifest but prunes
// the build output and SCM metadata. This is the load-bearing spec
// covered in ADR-0019 D.7.
func TestFixturesSnapshot_CurationFilter_Rust(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	// Write a small Rust-shaped tree. `target/` holds a binary that
	// MUST be pruned; .git/ holds a config file that MUST be pruned;
	// docs/usage.md must be kept because docs/* markdown is part of
	// the doc-drift surface.
	files := map[string]string{
		"Cargo.toml":             "[package]\nname = \"demo\"\n",
		"Cargo.lock":             "# lock\n",
		"README.md":              "# demo\n",
		"src/lib.rs":             "pub fn hello() {}\n",
		"src/main.rs":            "fn main() {}\n",
		"src/sub/mod.rs":         "pub mod sub;\n",
		"docs/usage.md":          "## usage\n",
		"target/release/demo":    "ELF-like bytes",
		"target/debug/build.log": "compiled\n",
		".git/config":            "[core]\n",
		".vscode/settings.json":  "{}\n",
		"scratch/notes.txt":      "random",
		"scratch/parser.c":       "/* grammar bomb */",
	}
	writeTree(t, src, files)

	res, err := Curate(CurateOptions{Source: src, Dest: dst, Language: "rust"})
	require.NoError(t, err, "curation must succeed on a well-formed Rust tree")

	mustExist(t, dst, "Cargo.toml")
	mustExist(t, dst, "Cargo.lock")
	mustExist(t, dst, "README.md")
	mustExist(t, dst, "src/lib.rs")
	mustExist(t, dst, "src/main.rs")
	mustExist(t, dst, "src/sub/mod.rs")
	mustExist(t, dst, "docs/usage.md")

	mustNotExist(t, dst, "target")
	mustNotExist(t, dst, "target/release/demo")
	mustNotExist(t, dst, ".git")
	mustNotExist(t, dst, ".git/config")
	mustNotExist(t, dst, ".vscode")
	mustNotExist(t, dst, "scratch/parser.c")
	mustNotExist(t, dst, "scratch/notes.txt")

	// 7 included files: Cargo.toml, Cargo.lock, README.md, 3 .rs files, docs/usage.md
	assert.Equal(t, 7, res.FilesCopied, "filter must copy exactly the included files")
	assert.Positive(t, res.BytesCopied, "result must report non-zero bytes")
}

// TestFixturesSnapshot_CurationFilter_Go mirrors a typical Go module —
// go.mod / go.sum + *.go + a testdata/ subtree containing both an .go
// file (kept) and a .db (excluded). The .db exclusion is the rule that
// prevents snapshot-of-snapshot recursion when we eventually snapshot
// mache itself or another repo that vendors test fixtures.
func TestFixturesSnapshot_CurationFilter_Go(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	files := map[string]string{
		"go.mod":                     "module demo\n",
		"go.sum":                     "demo v0.0.0\n",
		"main.go":                    "package main\n",
		"internal/util/util.go":      "package util\n",
		"docs/architecture.md":       "# arch\n",
		"README.md":                  "# demo\n",
		"CHANGELOG.md":               "## v0.1\n",
		"testdata/sample.go":         "package testdata\n",
		"testdata/fixture.db":        "sqlite-bytes",
		"testdata/archive.tar":       "tar-bytes",
		"node_modules/junk/index.js": "// noise",
		"__pycache__/cache.pyc":      "bytecode",
		"bin/built-binary":           "ELF",
		".DS_Store":                  "mac noise",
	}
	writeTree(t, src, files)

	res, err := Curate(CurateOptions{Source: src, Dest: dst, Language: "go"})
	require.NoError(t, err, "curation must succeed on a well-formed Go tree")

	mustExist(t, dst, "go.mod")
	mustExist(t, dst, "go.sum")
	mustExist(t, dst, "main.go")
	mustExist(t, dst, "internal/util/util.go")
	mustExist(t, dst, "docs/architecture.md")
	mustExist(t, dst, "README.md")
	mustExist(t, dst, "CHANGELOG.md")
	mustExist(t, dst, "testdata/sample.go")

	mustNotExist(t, dst, "testdata/fixture.db")
	mustNotExist(t, dst, "testdata/archive.tar")
	mustNotExist(t, dst, "node_modules")
	mustNotExist(t, dst, "__pycache__")
	mustNotExist(t, dst, "bin")
	mustNotExist(t, dst, ".DS_Store")

	// 8 included files: go.mod, go.sum, main.go, util.go, architecture.md, README.md, CHANGELOG.md, sample.go
	assert.Equal(t, 8, res.FilesCopied, "filter must copy exactly the included files")
}

// TestFixturesSnapshot_CurationFilter_RejectsBadInput asserts the
// filter validates its arguments rather than silently producing an
// empty snapshot when called wrong.
func TestFixturesSnapshot_CurationFilter_RejectsBadInput(t *testing.T) {
	_, err := Curate(CurateOptions{})
	require.Error(t, err, "empty options must be rejected")

	_, err = Curate(CurateOptions{Source: "/tmp", Dest: "/tmp", Language: "no-such-language"})
	require.Error(t, err, "unknown language must be rejected")
}

// writeTree creates each file in the map, making parent dirs as needed.
// Used by the curation tests to set up source trees mimicking real repos.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755), "mkdir for %s", rel)
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644), "write %s", rel)
	}
}

// mustExist asserts a relative path exists under root.
func mustExist(t *testing.T, root, rel string) {
	t.Helper()
	_, err := os.Stat(filepath.Join(root, rel))
	assert.NoError(t, err, "expected %s to exist in curated tree", rel)
}

// mustNotExist asserts a relative path does NOT exist under root.
func mustNotExist(t *testing.T, root, rel string) {
	t.Helper()
	_, err := os.Stat(filepath.Join(root, rel))
	assert.True(t, os.IsNotExist(err), "expected %s to be absent from curated tree (err=%v)", rel, err)
}
