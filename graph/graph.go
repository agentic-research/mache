// Package graph provides the public graph API for mache.
//
// Types are defined in internal/graph and re-exported here via type aliases
// so that external consumers (e.g. x-ray) can use mache's in-memory graph
// without importing internal packages.
package graph

import (
	ig "github.com/agentic-research/mache/internal/graph"
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
