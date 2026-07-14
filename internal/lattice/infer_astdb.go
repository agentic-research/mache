package lattice

import (
	"database/sql"
	"fmt"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/lang"

	// Pure-Go SQLite driver for opening leyline-produced .db files.
	_ "modernc.org/sqlite"
)

// InferFromASTDB infers a topology from a leyline-parsed `_ast` database —
// the pure-Go replacement for InferFromTreeSitterRoots (no in-process
// tree-sitter, no CGO).
//
// When Config.Language is set, only sources matching that language's file
// extensions (per the lang registry) contribute records; when empty, every
// `_ast` row in the database contributes. Config.SampleSize bounds the
// number of records read (0 = unlimited). Like InferFromTreeSitterRoots,
// the method is forced to "fca" for the duration of the call — greedy
// generates JSONPath selectors that fail as tree-sitter queries.
func (inf *Inferrer) InferFromASTDB(dbPath string) (*api.Topology, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open ast db %s: %w", dbPath, err)
	}
	defer func() { _ = db.Close() }()

	limit := inf.Config.SampleSize
	if limit < 0 {
		limit = 0
	}

	var records []any
	if inf.Config.Language == "" {
		records, err = ingest.FlattenASTDB(db, "", limit)
		if err != nil {
			return nil, err
		}
	} else {
		l := lang.ForName(inf.Config.Language)
		if l == nil {
			return nil, fmt.Errorf("infer from ast db: unknown language %q", inf.Config.Language)
		}
		for _, ext := range l.Extensions {
			remaining := 0
			if limit > 0 {
				remaining = limit - len(records)
				if remaining <= 0 {
					break
				}
			}
			recs, err := ingest.FlattenASTDB(db, "%"+ext, remaining)
			if err != nil {
				return nil, err
			}
			records = append(records, recs...)
		}
	}

	// Force FCA method for AST data — greedy generates JSONPath selectors
	// that fail when the engine tries to use them as tree-sitter queries.
	saved := inf.Config.Method
	inf.Config.Method = "fca"
	defer func() { inf.Config.Method = saved }()

	return inf.InferFromRecords(records)
}
