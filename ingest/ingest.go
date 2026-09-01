// Package ingest provides the public ingestion API for mache.
//
// Types are defined in internal/ingest and re-exported here via type aliases
// so that external consumers (e.g. venturi) can use mache's ingestion engine
// without importing internal packages.
package ingest

import (
	ii "github.com/agentic-research/mache/internal/ingest"
)

// IngestionTarget combines Graph reading with writing capabilities.
// The graph.MemoryStore type satisfies this interface.
type IngestionTarget = ii.IngestionTarget

// Engine drives the ingestion process: schema traversal, file walking, and
// node creation for both JSON and tree-sitter source paths.
type Engine = ii.Engine

// Walker queries structured data (JSON or tree-sitter AST) and returns matches.
type Walker = ii.Walker

// Match is a single result from a Walker query.
type Match = ii.Match

// JsonWalker implements the Walker interface for JSON-like data using JSONPath.
type JsonWalker = ii.JsonWalker

// SQLiteWriter is the IngestionTarget that persists a projection to a SQLite
// .db (nodes/node_defs/node_refs per the schema). It is exported for consumers
// that need custom ingestion control; most callers should use
// build.ParseWithSchema or build.ParseWithSchemaRef.
type SQLiteWriter = ii.SQLiteWriter

// ASTWalker projects an open, caller-owned leyline _ast SQLite database. Pair
// it with an Engine via Engine.SetASTWalker for custom projection control.
type ASTWalker = ii.ASTWalker

// NewSQLiteWriter opens (creating/truncating) a SQLite projection target.
var NewSQLiteWriter = ii.NewSQLiteWriter

// NewASTWalker constructs a walker over an open, caller-owned leyline _ast DB.
var NewASTWalker = ii.NewASTWalker

// NewEngine creates a new ingestion engine for the given schema and store.
var NewEngine = ii.NewEngine

// NewJsonWalker creates a new JSONPath-based walker.
var NewJsonWalker = ii.NewJsonWalker

// StreamSQLite iterates over all records in a SQLite database, calling fn for
// each one. Only one parsed record is alive at a time, keeping memory constant.
var StreamSQLite = ii.StreamSQLite
