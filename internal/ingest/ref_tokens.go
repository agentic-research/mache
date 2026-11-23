package ingest

import "github.com/agentic-research/mache/graph"

// refTokens is the reference tokens one rendered `refs` template names: see
// graph.RefTokens, the one rule the engine and the direct scan share.
func refTokens(rendered string) []string { return graph.RefTokens(rendered) }
