package lltest

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/leyline"

	_ "modernc.org/sqlite"
)

// IngestSourceViaLeyline is the test-side equivalent of the serve/mount source
// path after in-process tree-sitter removal (mache-37ae8b, ADR-0012 step 4):
// it parses srcPath with the pinned leyline binary into an `_ast` db, attaches
// an ASTWalker to engine, and ingests. Tests that previously did
// `engine.Ingest(sourceFileOrDir)` — which now errors because the Engine has
// no in-process parser — call this instead.
//
// srcPath may be a file or a directory (leyline parse wants a directory, so a
// single file is staged into a temp dir first). The binary is resolved with
// ResolveBinary(false), so the test SKIPS rather than downloading when the
// pinned leyline isn't cached — mirroring every other leyline e2e test.
func IngestSourceViaLeyline(t *testing.T, engine *ingest.Engine, srcPath string) {
	t.Helper()
	db, parseDir := ParseSourceViaLeyline(t, srcPath)
	engine.SetASTWalker(ingest.NewASTWalker(db))
	if err := engine.Ingest(parseDir); err != nil {
		t.Fatalf("ingest %s: %v", parseDir, err)
	}
}

// ParseSourceViaLeyline is the parse half of IngestSourceViaLeyline: it
// parses srcPath with the pinned leyline binary and returns the opened `_ast`
// db (closed at test cleanup) and the directory that was parsed — the path to
// ingest, and the root leyline keyed every source_id against. Tests that need
// the parse output itself, not just its projection (a count of `_ast` nodes
// to check the projection against), call this and ingest separately.
func ParseSourceViaLeyline(t *testing.T, srcPath string) (*sql.DB, string) {
	t.Helper()

	bin, err := leyline.ResolveBinary(false) // never download in tests
	if err != nil {
		// In CI the pinned leyline MUST be provisioned (`task leyline:ensure`
		// runs before `task test`), so a skip here would let green CI falsely
		// imply the sole-parser projection path was exercised (mache-01c467).
		// Fail loudly in CI; skip only in a local offline run.
		if os.Getenv("CI") != "" {
			t.Fatalf("pinned leyline unavailable in CI (%v) — provision it before tests "+
				"with `task leyline:ensure`; source projection requires it after "+
				"in-process tree-sitter removal (mache-37ae8b)", err)
		}
		t.Skipf("pinned leyline unavailable (%v); source projection requires it "+
			"after in-process tree-sitter removal (mache-37ae8b)", err)
	}

	parseDir := parseRoot(t, srcPath)
	dbFile := filepath.Join(t.TempDir(), "ast.db")
	cmd := exec.Command(bin, "parse", parseDir, "-o", dbFile)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("leyline parse %s: %v", parseDir, err)
	}

	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Fatalf("open _ast db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, parseDir
}

// parseRoot is the directory `leyline parse` is pointed at for srcPath: the
// path itself when it is a directory, else a temp dir the single file is
// staged into (leyline parse takes a directory).
func parseRoot(t *testing.T, srcPath string) string {
	t.Helper()
	info, err := os.Stat(srcPath)
	if err != nil {
		t.Fatalf("stat %s: %v", srcPath, err)
	}
	if info.IsDir() {
		return srcPath
	}
	staged := t.TempDir()
	content, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read %s: %v", srcPath, err)
	}
	if err := os.WriteFile(filepath.Join(staged, filepath.Base(srcPath)), content, 0o644); err != nil {
		t.Fatalf("stage %s: %v", srcPath, err)
	}
	return staged
}
