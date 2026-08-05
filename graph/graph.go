// Package graph provides the public graph API for mache.
//
// Types are defined in internal/graph and re-exported here via type aliases
// so that external consumers (e.g. x-ray) can use mache's in-memory graph
// without importing internal packages.
package graph

import (
	"github.com/agentic-research/mache/api"
	ig "github.com/agentic-research/mache/internal/graph"
	machetmpl "github.com/agentic-research/mache/internal/template"
)

// Graph is the interface for the FUSE layer and external consumers.
// Allows swapping the backend (Memory → SQLite → Mmap).
type Graph = ig.Graph

// Node is the universal primitive for files and directories.
// The Mode field declares whether this is a file or directory.
type Node = ig.Node

// MemoryStore is an in-memory implementation of Graph with roaring bitmap indexing.
type MemoryStore = ig.MemoryStore

// ContentRef is a recipe for lazily resolving file content from a backing store.
type ContentRef = ig.ContentRef

// SourceOrigin tracks the byte range of a construct in its source file.
type SourceOrigin = ig.SourceOrigin

// ContentResolverFunc resolves a ContentRef into byte content.
type ContentResolverFunc = ig.ContentResolverFunc

// CallExtractor parses source code and returns qualified function call tokens.
type CallExtractor = ig.CallExtractor

// QualifiedCall represents a function call with optional package qualifier.
type QualifiedCall = ig.QualifiedCall

// CompositeGraph multiplexes multiple Graph backends under path prefixes.
// Mount "browser" → /browser/... routes to that sub-graph.
type CompositeGraph = ig.CompositeGraph

// ActionResult is returned when an action is performed on a graph node.
type ActionResult = ig.ActionResult

// HotSwapGraph is a thread-safe wrapper that allows atomically swapping the
// underlying graph. Readers hold an RLock during each call; Swap acquires a
// write lock. Use this instead of hand-rolled mutex+pointer patterns.
type HotSwapGraph = ig.HotSwapGraph

// NewMemoryStore creates a new in-memory graph store.
var NewMemoryStore = ig.NewMemoryStore

// NewCompositeGraph creates an empty composite graph for multi-mount routing.
var NewCompositeGraph = ig.NewCompositeGraph

// NewHotSwapGraph creates a thread-safe graph wrapper that supports atomic Swap.
var NewHotSwapGraph = ig.NewHotSwapGraph

// ErrNotFound is returned when a node ID does not exist in the graph.
var ErrNotFound = ig.ErrNotFound

// ErrActNotSupported is returned by Graph implementations that do not support actions.
var ErrActNotSupported = ig.ErrActNotSupported

// TemplateRenderer renders a Go text/template string with the given values map.
type TemplateRenderer = ig.TemplateRenderer

// SQLiteResolver resolves ContentRef entries by fetching records from SQLite
// and re-rendering their content templates.
type SQLiteResolver = ig.SQLiteResolver

// NewSQLiteResolver creates a resolver that uses the given template renderer
// to render content from SQLite records.
var NewSQLiteResolver = ig.NewSQLiteResolver

// Node property accessors. Properties values are JSON (mache-90b89b), so
// external consumers must go through these rather than indexing the map —
// a string property is stored as `"go"`, not `go`.
var (
	PropString    = ig.PropString
	SetPropString = ig.SetPropString
	PropRaw       = ig.PropRaw
	SetPropRaw    = ig.SetPropRaw
)

// SQLiteGraph is the zero-copy backend that reads directly from a mache .db
// file (produced by `mache build` / `leyline parse`) — GetNode/ListChildren/
// ReadContent read the nodes table on demand, and LookupDef/QueryRefs/
// GetCallers query the db's own node_defs/node_refs tables directly. Unlike
// ImportSQLite (below), nothing needs to be pre-populated: a freshly opened
// SQLiteGraph answers LookupDef/QueryRefs immediately, because the data was
// never copied out of the file in the first place.
type SQLiteGraph = ig.SQLiteGraph

// OpenSQLiteGraph opens dbPath with a custom schema and template renderer.
// Most callers want Open, below, which fills in the defaults every mache
// entry point (task pack, mache serve, ...) already uses for a plain .db
// source.
var OpenSQLiteGraph = ig.OpenSQLiteGraph

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
func Open(dbPath string) (*SQLiteGraph, error) {
	return ig.OpenSQLiteGraph(dbPath, &api.Topology{}, machetmpl.Render)
}
