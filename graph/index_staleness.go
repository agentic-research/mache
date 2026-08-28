package graph

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// IndexStaleness is how far a projected graph has drifted from the tree it was
// built from — the honest answer to "can I trust this index?".
//
// Why it exists (mache-6c9e1d): the flagship serve path parses the repo ONCE
// at session start and serves that frozen .db for the session's whole life.
// Every edit after that is invisible to search/find_definition, and the
// longer and more productive a session, the wronger the answers — with
// nothing anywhere saying so. Until the live-reparse path exists, the
// warn-don't-lie floor is to REPORT the drift.
type IndexStaleness struct {
	// BuiltAt is when the index was produced.
	BuiltAt time.Time `json:"built_at"`
	// SourceRoot is the tree it was built from.
	SourceRoot string `json:"source_root"`
	// ModifiedSince is how many source files changed after BuiltAt, capped at
	// staleScanCap — the point is "should I care", not a manifest.
	ModifiedSince int `json:"modified_since"`
	// DeletedSince counts indexed source files that no longer exist on disk.
	// Exact, not heuristic: the .db records every file it indexed
	// (nodes.source_file), so a missing path is a fact. A rename counts here
	// too — the indexed path IS gone, and the mtime walk cannot see it
	// (mv preserves mtimes, so the new path does not read as modified
	// either; without this field a rename was doubly invisible).
	DeletedSince int `json:"deleted_since,omitempty"`
	// Capped reports that a count hit the cap and the true number is larger.
	Capped bool `json:"capped,omitempty"`
}

// staleScanCap bounds the modified-file count. Past this the answer is "a
// lot"; walking on to an exact number costs time and changes no decision.
const staleScanCap = 500

// staleness derives the report from facts the artifact already carries:
// the .db file's own mtime IS the build time, and ley-line stamps
// _meta.source_root at parse. Nothing new is written anywhere — a pure
// read-side derivation, which also means it works retroactively on every
// existing .db.
//
// Returns ok=false when the graph cannot answer (no dbPath, no _meta, source
// root gone) — absence of a staleness report must read as "unknown", never as
// "fresh".
func (g *SQLiteGraph) IndexStaleness() (IndexStaleness, bool) {
	if g.dbPath == "" {
		return IndexStaleness{}, false
	}
	builtAt, ok := g.staleBuiltAt()
	if !ok {
		return IndexStaleness{}, false
	}
	var root string
	if err := g.db.QueryRow(`SELECT value FROM _meta WHERE key = 'source_root'`).Scan(&root); err != nil || root == "" {
		return IndexStaleness{}, false
	}
	if _, err := os.Stat(root); err != nil {
		return IndexStaleness{}, false // tree moved or deleted: unknown, not fresh
	}

	report := IndexStaleness{BuiltAt: builtAt, SourceRoot: root}
	g.countModified(&report, root, builtAt)
	g.countDeleted(&report, root)
	return report, true
}

// staleBuiltAt resolves when the index was produced: the producer's own
// _meta.parse_time stamp when present (survives file copies and
// mtime-mangling transports), else the .db file's mtime.
func (g *SQLiteGraph) staleBuiltAt() (time.Time, bool) {
	st, err := os.Stat(g.dbPath)
	if err != nil {
		return time.Time{}, false
	}
	builtAt := st.ModTime()
	var parseTime int64
	if err := g.db.QueryRow(`SELECT value FROM _meta WHERE key = 'parse_time'`).Scan(&parseTime); err == nil && parseTime > 0 {
		builtAt = time.Unix(parseTime, 0)
	}
	return builtAt, true
}

// countModified walks the source tree counting files whose mtime postdates
// the build, capped — the point is "should I care", not a manifest. Noise
// directories and dotfiles are excluded so churn cannot cry wolf.
func (g *SQLiteGraph) countModified(report *IndexStaleness, root string, builtAt time.Time) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree: skip, do not fail the report
		}
		if d.IsDir() {
			// The usual noise directories change constantly without changing
			// what the index should contain.
			switch d.Name() {
			case ".git", "node_modules", ".jj", "target":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().After(builtAt) {
			report.ModifiedSince++
			if report.ModifiedSince >= staleScanCap {
				report.Capped = true
				return filepath.SkipAll
			}
		}
		return nil
	})
}

// countDeleted checks every file the index knows against the disk. Exact and
// bounded — the query is capped like the walk, and a db without the
// source_file column (older projections) simply reports no deletions rather
// than failing the whole report. The mtime walk is structurally blind to
// this half of drift: a deleted file is not in the walk, and a rename
// preserves mtimes, so without this a rename was doubly invisible.
func (g *SQLiteGraph) countDeleted(report *IndexStaleness, root string) {
	rows, err := g.db.Query(
		`SELECT DISTINCT source_file FROM nodes
		 WHERE source_file IS NOT NULL AND source_file != ''
		 LIMIT ?`, staleScanCap*4)
	if err != nil {
		return
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var rel string
		if rows.Scan(&rel) != nil {
			continue
		}
		p := rel
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, rel)
		}
		if _, err := os.Stat(p); os.IsNotExist(err) {
			report.DeletedSince++
			if report.DeletedSince >= staleScanCap {
				report.Capped = true
				return
			}
		}
	}
}

// StalenessReporter is the capability get_overview probes for. On graphs that
// cannot answer (MemoryStore, composites) the overview simply omits the block.
type StalenessReporter interface {
	IndexStaleness() (IndexStaleness, bool)
}
