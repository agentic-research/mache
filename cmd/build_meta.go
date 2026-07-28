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
	"github.com/agentic-research/mache/internal/leyline"
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
// 3339), plus the leyline provenance keys from leylineMetaRows —
// `leyline_pin`, `leyline_version`, and `leyline_source` when a binary was
// resolved. Best-effort: any failure logs and returns nil — the build
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
	rows = append(rows, leylineMetaRows()...)
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

// leylineMetaRows returns the leyline provenance rows for _mache_meta.
//
// Recorded because the producing leyline determines how merkle-AST node
// addresses are DERIVED, and mache has no other way to observe that. LLO
// v0.11.0 bumped IR_SCHEMA_VERSION from merkle-ast-v1 to merkle-ast-v2,
// rewriting every Rust trait signature's node_hash from BYTE-IDENTICAL sources
// (ley-line-open #282). Every staleness mechanism mache has is content- or
// time-based — cache lockfiles hash raw source bytes, the parse skip is
// mtime+size — so none of them can see it. Before this, a .db built under
// merkle-ast-v1 and served after the upgrade was indistinguishable from a fresh
// one, and would serve stale addresses indefinitely (mache-438104).
//
// Two values, because they answer different questions and can disagree:
//
//	leyline_version — what actually ran. MACHE_LEYLINE_BINARY can point at a
//	                  local build, so this is the only honest answer to "what
//	                  produced this .db".
//	leyline_pin     — what this mache build requires. Always present, even when
//	                  no binary was resolved (a pure-.db path), so the pin a
//	                  given mache expects is always recoverable from the
//	                  artifact.
//
// A mismatch between them is the diagnostic: it means an override was in play
// and the .db may not match what CI would produce.
//
// This is the half mache can fix alone. Consuming a lineage tag published by
// LLO is the durable fix and is separate (mache-43d63d, blocked on
// ley-line-open-348de6) — a version string is a proxy for lineage, not lineage
// itself: two releases can share an IR schema, and one release can change it.
func leylineMetaRows() [][2]string {
	prov, ok := leyline.Provenance()
	return leylineMetaRowsFrom(prov, ok, leyline.PinnedBinaryVersion())
}

// leylineMetaRowsFrom is the pure form, taking the provenance rather than
// reading the process-global record. Split out so tests can exercise every
// branch — resolved, unresolved, override-in-play — without mutating a
// package-level singleton that has no reset and would leak into every
// subsequent test in the package.
func leylineMetaRowsFrom(prov leyline.LeylineProvenance, ok bool, pin string) [][2]string {
	rows := [][2]string{{"leyline_pin", pin}}
	if !ok || prov.Version == "" {
		// No binary resolved this process (pure-.db build, or resolution
		// failed). Record the absence rather than omitting the key, so a
		// consumer can tell "built without leyline" from "built by an older
		// mache that didn't stamp this".
		return append(rows, [2]string{"leyline_version", "unresolved"})
	}
	return append(rows,
		[2]string{"leyline_version", prov.Version},
		[2]string{"leyline_source", prov.Source},
	)
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
