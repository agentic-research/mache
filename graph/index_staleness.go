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
	// Capped reports that the count hit the cap and the true number is larger.
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
	st, err := os.Stat(g.dbPath)
	if err != nil {
		return IndexStaleness{}, false
	}
	builtAt := st.ModTime()

	var root string
	if err := g.db.QueryRow(`SELECT value FROM _meta WHERE key = 'source_root'`).Scan(&root); err != nil || root == "" {
		return IndexStaleness{}, false
	}
	if _, err := os.Stat(root); err != nil {
		return IndexStaleness{}, false // tree moved or deleted: unknown, not fresh
	}

	report := IndexStaleness{BuiltAt: builtAt, SourceRoot: root}
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
	return report, true
}

// StalenessReporter is the capability get_overview probes for. On graphs that
// cannot answer (MemoryStore, composites) the overview simply omits the block.
type StalenessReporter interface {
	IndexStaleness() (IndexStaleness, bool)
}
