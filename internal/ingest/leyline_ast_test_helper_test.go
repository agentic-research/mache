package ingest

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"
)

// resolvePinnedLeylineForIngest resolves the pinned leyline binary WITHOUT
// importing internal/leyline — under the `leyline` build tag that package pulls
// in CGO FFI (libleyline/FUSE), which these default-tag tests must not require.
// PATH and ~/.mache/bin candidates are accepted only if `--version` matches the
// pin in socket.go; never a download. Skips the test when none is available.
func resolvePinnedLeylineForIngest(t *testing.T) string {
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

// attachLeylineAST parses target (a source dir, or the parent dir of a source
// file) with the pinned leyline into an `_ast` db and wires an ASTWalker onto
// engine. ADR-0012 step 4 removed in-process CGO tree-sitter, so every source
// (tree-sitter S-expression) schema now projects the ley-line `_ast` db via the
// ASTWalker — this helper is the test-side equivalent of the serve/mount/build
// leyline-parse step. Skips the test when the pinned leyline isn't available.
func attachLeylineAST(t *testing.T, engine *Engine, target string) {
	t.Helper()
	bin := resolvePinnedLeylineForIngest(t)

	parseDir := target
	if info, err := os.Stat(target); err == nil && !info.IsDir() {
		parseDir = filepath.Dir(target)
	}
	dbPath := filepath.Join(t.TempDir(), "ast.db")
	out, err := exec.Command(bin, "parse", parseDir, "-o", dbPath).CombinedOutput() //nolint:gosec // test-only, pinned binary
	require.NoError(t, err, "leyline parse failed: %s", string(out))
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	engine.SetASTWalker(NewASTWalker(db))
}
