package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agentic-research/mache/internal/graph"
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

		type searchResult struct {
			Token string `json:"token"`
			Path  string `json:"path"`
			Role  string `json:"role,omitempty"`
		}

		// Definition search.
		//
		// Two backends: SQL-pushdown via defsSearcher (preferred — covers
		// pre-built .db files whose in-memory defs map is empty), and
		// in-memory map iteration via defsMapProvider (legacy MemoryStore
		// and live-ingest paths).
		//
		// Wrappers like lazyGraph always satisfy the defsSearcher
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
		if role == "definition" {
			var matches map[string][]string
			if ds, ok := g.(defsSearcher); ok {
				matches = ds.SearchDefs(pattern, limit)
			}
			if matches == nil {
				if dp, ok := g.(defsMapProvider); ok {
					defs := dp.DefsMap()
					matches = make(map[string][]string, len(defs))
					for token, ids := range defs {
						if sqlLikeMatch(pattern, token) {
							matches[token] = ids
						}
					}
				} else {
					return mcp.NewToolResultError("backend does not support definition search"), nil
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
			return mcp.NewToolResultText(string(data)), nil
		}

		// Reference search (default): query mache_refs (vtab) or node_refs (real table).
		// Leyline-parsed .dbs have node_refs (token, node_id); legacy sidecar has mache_refs (token, path).
		//
		// `path NOT LIKE '_file_level:%'` filters the engine's
		// file-level sentinel rows (from PR #270) — synthetic
		// caller_ids the engine uses to mark file-level fn-value
		// refs (mache-02r9) as alive. They're not real source
		// locations and would surface as fake "/path/file.go"
		// results. NodesTableReader.GetCallers applies the same
		// filter; this puts search/role=reference in agreement.
		qg, ok := g.(refsQuerier)
		if !ok {
			return mcp.NewToolResultError("reference search requires a SQLite-backed graph; use role=definition for in-memory search"), nil
		}
		var rows *sql.Rows
		var err error
		if typeFilter != "" {
			rows, err = qg.QueryRefs(
				"SELECT token, path FROM mache_refs WHERE token LIKE ? AND path LIKE ? AND path NOT LIKE '_file_level:%' LIMIT ?",
				pattern, "%/"+typeFilter+"/%", limit,
			)
		} else {
			rows, err = qg.QueryRefs(
				"SELECT token, path FROM mache_refs WHERE token LIKE ? AND path NOT LIKE '_file_level:%' LIMIT ?",
				pattern, limit,
			)
		}
		// Fallback: node_refs table (leyline-parsed .db) when mache_refs vtab doesn't exist.
		if err != nil && strings.Contains(err.Error(), "no such table") {
			if typeFilter != "" {
				rows, err = qg.QueryRefs(
					"SELECT token, node_id AS path FROM node_refs WHERE token LIKE ? AND node_id LIKE ? AND node_id NOT LIKE '_file_level:%' LIMIT ?",
					pattern, "%/"+typeFilter+"/%", limit,
				)
			} else {
				rows, err = qg.QueryRefs(
					"SELECT token, node_id AS path FROM node_refs WHERE token LIKE ? AND node_id NOT LIKE '_file_level:%' LIMIT ?",
					pattern, limit,
				)
			}
		}
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("query: %v", err)), nil
		}
		defer func() { _ = rows.Close() }()

		var results []searchResult
		for rows.Next() {
			var r searchResult
			if err := rows.Scan(&r.Token, &r.Path); err != nil {
				continue
			}
			results = append(results, r)
		}
		if results == nil {
			results = []searchResult{}
		}

		data, _ := json.MarshalIndent(results, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
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
