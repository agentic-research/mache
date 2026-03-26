// Package materialize provides the public materializer API for mache.
//
// Types are defined in internal/materialize and re-exported here via type
// aliases so that external consumers (e.g. venturi) can call materializers
// as library functions without importing internal packages.
package materialize

import (
	im "github.com/agentic-research/mache/internal/materialize"
)

// Materializer writes a projected node tree (from a mache SQLite DB) to an
// output format.
type Materializer = im.Materializer

// ForFormat returns the appropriate Materializer for the given format string.
var ForFormat = im.ForFormat
