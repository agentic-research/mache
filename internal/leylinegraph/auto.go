// R3 re-home (mache-96c378 stage 5): the auto-parse + walker attachment
// pipeline moved here from cmd/serve.go. mount, build, serve, and the
// schema-inference path all invoke leyline through these two functions, and
// serve owning them was the schemainfer→mcpserve and cmd→mcpserve edge the
// decomposition had to empty.
package leylinegraph

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/exec"

	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/leyline"
)

// AutoInvokeLeylineParse runs `leyline parse <sourceDir> -o <tmpdb>` and
// returns the path to the produced .db plus a cleanup function that removes
// it. Returns an error if leyline is not available on PATH or in the bundled
// location, or if parsing fails. The caller should fall back to the
// in-process ingest path on any error.
func AutoInvokeLeylineParse(sourceDir string) (string, func(), error) {
	// Resolve — and if absent, auto-download — the leyline binary via the same
	// provisioning path mache serve uses, so `mache build` no longer silently
	// degrades to tree-sitter merely because leyline isn't installed.
	// MACHE_NO_LEYLINE opts out (returns an error the auto caller treats as
	// "fall back to tree-sitter").
	leylineBin, err := leyline.ResolveBinary(true)
	if err != nil {
		return "", nil, err
	}
	// Publish which binary this is. Without it Provenance() reports "no binary
	// resolved" even though we are about to run one, and writeBuildMetadata has
	// nothing to stamp into the .db — leaving artifacts whose producing leyline
	// is unknowable (mache-438104).
	leyline.RecordResolved(leylineBin, "resolved")

	// Prefer the persistent per-project db: pointed at the SAME path as last
	// time, leyline re-parses only what changed. Pointed at a fresh temp file
	// it has nothing to diff and re-parses everything (mache-80a851). A nil
	// entry means the cache is unavailable — no home dir, another process
	// holds the lock, under `go test` — and the temp path below is then
	// exactly what mache did unconditionally before.
	if entry := reserveParseCache(sourceDir); entry != nil {
		log.Printf("auto-leyline: parsing %s -> %s (persistent; unchanged files are skipped)",
			sourceDir, entry.path)
		if err := runLeylineParse(leylineBin, sourceDir, entry.path); err != nil {
			// A failed parse may have left the db half-written, and it would
			// be the input to every later run. Drop it so the next run starts
			// clean rather than inheriting the damage.
			entry.discard()
			return "", nil, err
		}
		return entry.path, entry.release, nil
	}

	tmpFile, err := os.CreateTemp("", "mache-leyline-*.db")
	if err != nil {
		return "", nil, fmt.Errorf("create temp .db: %w", err)
	}
	tmpPath := tmpFile.Name()
	_ = tmpFile.Close()

	cleanup := func() {
		_ = os.Remove(tmpPath)
		_ = os.Remove(tmpPath + "-wal")
		_ = os.Remove(tmpPath + "-shm")
	}

	log.Printf("auto-leyline: parsing %s -> %s", sourceDir, tmpPath)
	if err := runLeylineParse(leylineBin, sourceDir, tmpPath); err != nil {
		cleanup()
		return "", nil, err
	}
	return tmpPath, cleanup, nil
}

// runLeylineParse shells out to `leyline parse <sourceDir> -o <dbPath>`.
func runLeylineParse(leylineBin, sourceDir, dbPath string) error {
	cmd := exec.Command(leylineBin, "parse", sourceDir, "-o", dbPath)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("leyline parse: %w", err)
	}
	return nil
}

// AttachLeylineASTWalker parses dataSource with ley-line into a temporary
// `_ast` db, opens it, and wires an ASTWalker onto the engine so a source
// (tree-sitter S-expression) schema projects with zero in-process CGO
// (ADR-0012 step 4). Returns the open db (for a call extractor) and a cleanup
// that closes the db and removes the temp file. ley-line is mandatory: a
// resolution failure is returned as an error (guardrail 2), never swallowed.
func AttachLeylineASTWalker(dataSource string, engine *ingest.Engine) (*sql.DB, func(), error) {
	dbPath, parseCleanup, err := AutoInvokeLeylineParse(dataSource)
	if err != nil {
		return nil, nil, err
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		parseCleanup()
		return nil, nil, fmt.Errorf("open ley-line parse output %s: %w", dbPath, err)
	}
	engine.SetASTWalker(ingest.NewASTWalker(db))
	cleanup := func() {
		_ = db.Close()
		parseCleanup()
	}
	return db, cleanup, nil
}
