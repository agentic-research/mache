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

// NewMemoryStore creates a new in-memory graph store.
var NewMemoryStore = ig.NewMemoryStore

// ErrNotFound is returned when a node ID does not exist in the graph.
var ErrNotFound = ig.ErrNotFound
