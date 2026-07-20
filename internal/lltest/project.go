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

	bin, err := leyline.ResolveBinary(false) // never download in tests
	if err != nil {
		t.Skipf("pinned leyline unavailable (%v); source projection requires it "+
			"after in-process tree-sitter removal (mache-37ae8b)", err)
	}

	parseDir := srcPath
	info, err := os.Stat(srcPath)
	if err != nil {
		t.Fatalf("stat %s: %v", srcPath, err)
	}
	if !info.IsDir() {
		// leyline parse takes a directory; stage the single file.
		staged := t.TempDir()
		content, rerr := os.ReadFile(srcPath)
		if rerr != nil {
			t.Fatalf("read %s: %v", srcPath, rerr)
		}
		if werr := os.WriteFile(filepath.Join(staged, filepath.Base(srcPath)), content, 0o644); werr != nil {
			t.Fatalf("stage %s: %v", srcPath, werr)
		}
		parseDir = staged
	}

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

	engine.SetASTWalker(ingest.NewASTWalker(db))
	if err := engine.Ingest(parseDir); err != nil {
		t.Fatalf("ingest %s: %v", parseDir, err)
	}
}
