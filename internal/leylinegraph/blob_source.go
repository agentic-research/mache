package leylinegraph

import (
	"database/sql"

	"github.com/agentic-research/mache/graph"
)

// WireParseDB gives store what it reads from the leyline parse db its source
// projection was built from: on-demand callee resolution (whole-file and
// AST-scoped) and the parsed blobs a write rebuilds its file from.
func WireParseDB(store *graph.MemoryStore, astDB *sql.DB) {
	store.SetCallExtractor(NewASTCallExtractor(astDB))
	store.SetScopedCallExtractor(NewASTScopedCallExtractor(astDB))
	store.SetBlobSource(graph.NewParseBlobs(astDB))
}
