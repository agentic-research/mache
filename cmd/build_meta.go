package cmd

import (
	"database/sql"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/lang"
	_ "modernc.org/sqlite"
)

// writeBuildMetadata stamps a `_mache_meta` table into dbPath recording
// the backend that produced it, the mache version, and the build
// timestamp. Both `mache build --backend=leyline` and
// `--backend=tree-sitter` call this so consumers (smell rules, find-
// smells preflight, ad-hoc CLI inspection) can answer "which backend
// produced this .db?" without `.tables` archaeology.
//
// The table is `(key TEXT PRIMARY KEY, value TEXT NOT NULL)`. Keys
// today: `backend`, `mache_version`, `mache_commit`, `built_at` (RFC
// 3339). Best-effort: any failure logs and returns nil — the build
// shouldn't fail because the marker couldn't be written.
func writeBuildMetadata(dbPath, backend string) error {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Printf("warn: _mache_meta: open %s: %v", dbPath, err)
		return nil
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS _mache_meta (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`); err != nil {
		log.Printf("warn: _mache_meta: create table: %v", err)
		return nil
	}

	rows := [][2]string{
		{"backend", backend},
		{"mache_version", Version},
		{"mache_commit", Commit},
		{"built_at", time.Now().UTC().Format(time.RFC3339)},
	}
	for _, kv := range rows {
		if _, err := db.Exec(
			`INSERT OR REPLACE INTO _mache_meta (key, value) VALUES (?, ?)`,
			kv[0], kv[1],
		); err != nil {
			log.Printf("warn: _mache_meta: write %s=%q: %v", kv[0], kv[1], err)
			return nil
		}
	}
	return nil
}

// countNodes returns the number of rows in `nodes`, or -1 if the table
// is missing or unreadable. Used by warnIfEmptyBuild to detect "leyline
// returned 0 parsed and we silently wrote an empty .db" — see issue #3
// in the schema-bugs PR.
func countNodes(dbPath string) int {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return -1
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&n); err != nil {
		return -1
	}
	return n
}

// countSourceFiles walks sourceDir and returns the count of files whose
// extension is recognized by lang.IsSourceExt. Mirrors the directory
// skipping logic the engine uses so a `.git` or `vendor` dir doesn't
// inflate the count. Returns -1 on walk error (caller treats as
// "unknown — don't warn").
func countSourceFiles(sourceDir string) int {
	count := 0
	err := filepath.WalkDir(sourceDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != sourceDir && ingest.ShouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if lang.IsSourceExt(strings.ToLower(filepath.Ext(path))) {
			count++
		}
		return nil
	})
	if err != nil {
		return -1
	}
	return count
}

// warnIfEmptyBuild logs a loud warning when the build produced a .db
// with zero `nodes` rows despite the source directory containing files
// of recognized source extensions. This catches the case from issue #3:
// leyline (or any backend) silently dropping all input — e.g. a
// Terraform directory parsed by a backend that doesn't grok .tf —
// and exiting 0 with an empty .db.
//
// The warning is informational only; the build doesn't fail. A clean
// build of an empty input directory (truly 0 source files) doesn't
// trigger the warning.
func warnIfEmptyBuild(dbPath, sourceDir, backend string) {
	nodes := countNodes(dbPath)
	if nodes != 0 {
		return
	}
	srcCount := countSourceFiles(sourceDir)
	if srcCount <= 0 {
		// No source files in the input — empty .db is the correct
		// answer. Don't pollute the log.
		return
	}
	log.Printf("WARNING: %s produced an empty .db (0 nodes) from %s, "+
		"which contains %d file(s) with recognized source extensions. "+
		"Backend %q may not support those file types — check `leyline --help` "+
		"or `mache build --backend=tree-sitter` if you expected them to be parsed.",
		filepath.Base(dbPath), sourceDir, srcCount, backend)
}
