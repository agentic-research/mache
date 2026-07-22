package ingest

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/leyline"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"
)

// pinnedLeylineForIngest resolves the pinned leyline binary via the production
// resolver (PATH → ~/.mache/bin → verified pin, never a download) — no regex,
// no hand-rolled candidate loop. Importing internal/leyline is CGO-free now
// that the //go:build leyline FFI (client.go) is gone, so tests use the real
// ResolveBinary instead of re-deriving the pin from socket.go source text.
// Fails in CI (where the binary must be provisioned via `task leyline:ensure`)
// and skips otherwise.
func pinnedLeylineForIngest(t *testing.T) string {
	t.Helper()
	bin, err := leyline.ResolveBinary(false) // never download in tests
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("pinned leyline unavailable in CI (%v) — provision it (task leyline:ensure) before tests", err)
		}
		t.Skipf("pinned leyline unavailable (%v); source projection requires it", err)
	}
	return bin
}

// attachLeylineAST parses target (a source dir, or the parent dir of a source
// file) with the pinned leyline into an `_ast` db and wires an ASTWalker onto
// engine. ADR-0012 step 4 removed in-process CGO tree-sitter, so every source
// (tree-sitter S-expression) schema now projects the ley-line `_ast` db via the
// ASTWalker — this helper is the test-side equivalent of the serve/mount/build
// leyline-parse step. Skips the test when the pinned leyline isn't available.
func attachLeylineAST(t *testing.T, engine *Engine, target string) {
	t.Helper()
	bin := pinnedLeylineForIngest(t)

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
