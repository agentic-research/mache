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
// MemoryStore satisfies this interface.
type IngestionTarget = ii.IngestionTarget

// Engine drives the ingestion process: schema traversal, file walking, and
// node creation for both JSON and tree-sitter source paths.
type Engine = ii.Engine

// JsonWalker implements the Walker interface for JSON-like data using JSONPath.
type JsonWalker = ii.JsonWalker

// NewEngine creates a new ingestion engine for the given schema and store.
var NewEngine = ii.NewEngine

// NewJsonWalker creates a new JSONPath-based walker.
var NewJsonWalker = ii.NewJsonWalker

// StreamSQLite iterates over all records in a SQLite database, calling fn for
// each one. Only one parsed record is alive at a time, keeping memory constant.
var StreamSQLite = ii.StreamSQLite
