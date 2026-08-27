package testutil

import (
	"database/sql"

	"github.com/agentic-research/mache/graph"
)

// SmellTestGraph is a tiny graph backend that delegates QueryRefs to a
// caller-supplied *sql.DB. It exists only for tests because spinning up a
// full SQLiteGraph just to run a SQL pattern is not worth it.
type SmellTestGraph struct {
	*graph.MemoryStore
	DB   *sql.DB
	Path string // optional: when set, exposed via DBPath() for capnp readthrough
}

func (s *SmellTestGraph) QueryRefs(query string, args ...any) (*sql.Rows, error) {
	return s.DB.Query(query, args...)
}

// DBPath implements graph.DBPathProvider when Path is set, opting this
// test graph into capnp readthrough (mache-190508 step 3 / mache-6bd4d8).
// Tests that don't set Path keep the legacy mention-only view shape.
func (s *SmellTestGraph) DBPath() string { return s.Path }
