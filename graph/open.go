package graph

import (
	"github.com/agentic-research/mache/api"
	machetmpl "github.com/agentic-research/mache/internal/template"
)

// Open opens a mache .db file for querying, with the same defaults mache's
// own CLI uses for a bare .db source (empty schema — auto-detected from the
// db's own tables — and mache's pure-Go template renderer). Returns a
// *SQLiteGraph, which implements Graph plus LookupDef/QueryRefs/GetCallers
// directly against the file — no import/populate step required.
//
// This is the fix for the trap MemoryStore + ImportSQLite falls into: a
// MemoryStore built by ImportSQLite has an empty defs map and an
// uninitialized refs sidecar (ImportSQLite only replicates the node tree),
// so LookupDef silently returns nil and QueryRefs errors
// "refsDB not initialized" — not because the .db lacks the data, but
// because it was never copied into MemoryStore's separate in-memory
// indices. Open sidesteps the whole class by not copying anything.
//
// OpenSQLiteGraph is the lower-level form, for callers supplying their own
// schema and template renderer.
func Open(dbPath string) (*SQLiteGraph, error) {
	return OpenSQLiteGraph(dbPath, &api.Topology{}, machetmpl.Render)
}
