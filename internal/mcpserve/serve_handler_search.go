package mcpserve

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agentic-research/mache/graph"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func makeSearchHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		pattern := request.GetString("pattern", "")
		if pattern == "" {
			return mcp.NewToolResultError("pattern is required"), nil
		}

		typeFilter := request.GetString("type", "")
		role := request.GetString("role", "")
		limit := request.GetInt("limit", 100)

		if role == "definition" {
			return searchDefinitions(g, pattern, typeFilter, limit), nil
		}
		return searchReferences(g, pattern, typeFilter, limit), nil
	}
}

// searchDefinitions is search role=definition.
//
// Two backends: SQL-pushdown via graph.DefsSearcher (preferred — covers
// pre-built .db files whose in-memory defs map is empty), and
// in-memory map iteration via graph.DefsMapper (legacy MemoryStore
// and live-ingest paths).
//
// Wrappers like lazyGraph always satisfy the graph.DefsSearcher
// interface (they have the method, even if the inner backend
// doesn't). If the wrapper returns nil from SearchDefs because
// the inner didn't implement it, fall through to the DefsMap
// path — `nil` from SearchDefs is distinguishable from `empty
// map = no matches` and signals "I don't speak this protocol."
//
// Bead mache-9cba08 caught the original SQL pushdown gap; the
// nil-passthrough fallback below was caught by the PR #373
// post-fix audit (lazyGraph→MemoryStore returned nil and the
// fallback was unreachable due to dispatch ordering).
func searchDefinitions(g graph.Graph, pattern, typeFilter string, limit int) *mcp.CallToolResult {
	type searchResult struct {
		Token string `json:"token"`
		Path  string `json:"path"`
		Role  string `json:"role,omitempty"`
	}
	var matches map[string][]string
	if ds, ok := g.(graph.DefsSearcher); ok {
		matches = ds.SearchDefs(pattern, limit)
	}
	if matches == nil {
		if dp, ok := g.(graph.DefsMapper); ok {
			defs := dp.DefsMap()
			matches = make(map[string][]string, len(defs))
			for token, ids := range defs {
				if sqlLikeMatch(pattern, token) {
					matches[token] = ids
				}
			}
		} else {
			return mcp.NewToolResultError("backend does not support definition search")
		}
	}

	var results []searchResult
	seenPaths := make(map[string]bool)
	for token, ids := range matches {
		for _, id := range ids {
			if typeFilter != "" && !strings.Contains(id, "/"+typeFilter+"/") {
				continue
			}
			// Dedup: both "Foo.Bar" and "pkg.Foo.Bar" map to the same path
			if seenPaths[id] {
				continue
			}
			seenPaths[id] = true
			results = append(results, searchResult{Token: token, Path: id, Role: "definition"})
			if len(results) >= limit {
				break
			}
		}
		if len(results) >= limit {
			break
		}
	}
	if results == nil {
		results = []searchResult{}
	}
	data, _ := json.MarshalIndent(results, "", "  ")
	return mcp.NewToolResultText(string(data))
}

// searchReferences is search role=reference, through the graph's
// RefSearcher: the backend knows where its references live (the mache_refs
// sidecar, or node_refs keyed by node_id on projection-v4 and nid on v6) and
// drops the engine's file-level sentinel rows (PR #270), synthetic caller_ids
// that are not real source locations, the same filter GetCallers applies.
func searchReferences(g graph.Graph, pattern, typeFilter string, limit int) *mcp.CallToolResult {
	rs, ok := g.(graph.RefSearcher)
	if !ok {
		return mcp.NewToolResultError("reference search requires a SQLite-backed graph; use role=definition for in-memory search")
	}
	hits, err := rs.SearchRefs(pattern, typeFilter, limit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("query: %v", err))
	}
	type reference struct {
		Token string `json:"token"`
		Path  string `json:"path"`
	}
	results := make([]reference, 0, len(hits))
	for _, h := range hits {
		results = append(results, reference{Token: h.Token, Path: h.ID})
	}
	data, _ := json.MarshalIndent(results, "", "  ")
	return mcp.NewToolResultText(string(data))
}

// sqlLikeMatch performs a simple SQL LIKE pattern match (% = wildcard).
func sqlLikeMatch(pattern, value string) bool {
	pattern = strings.ToLower(pattern)
	value = strings.ToLower(value)

	// Fast paths for common patterns
	if pattern == "%" {
		return true
	}
	if !strings.Contains(pattern, "%") {
		return pattern == value
	}

	parts := strings.Split(pattern, "%")
	pos := 0
	for i, part := range parts {
		if part == "" {
			continue
		}
		idx := strings.Index(value[pos:], part)
		if idx < 0 {
			return false
		}
		if i == 0 && idx != 0 {
			// First part must match start if pattern doesn't begin with %
			return false
		}
		pos += idx + len(part)
	}
	// If pattern doesn't end with %, value must end at pos
	if !strings.HasSuffix(pattern, "%") && pos != len(value) {
		return false
	}
	return true
}
