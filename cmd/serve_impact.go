package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agentic-research/mache/graph"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// maxImpactNodes caps the total number of nodes returned by get_impact to
// prevent runaway BFS on highly-connected graphs.
const maxImpactNodes = 500

// genericNames are symbols so common that expanding their callers at depth > 0
// would flood the result with unrelated call sites (e.g. every function that
// calls "Close" or "String"). We skip GetCallers for these tokens when past
// the root level because the caller wants the impact of the *original* symbol,
// not every site that calls a ubiquitous interface method.
var genericNames = map[string]bool{
	"Close": true, "String": true, "Error": true, "Read": true,
	"Write": true, "New": true, "Init": true, "Get": true,
	"Set": true, "Len": true, "Reset": true,
}

// makeGetImpactHandler returns a handler that performs multi-hop BFS traversal
// through the refs graph to show the blast radius of a symbol change.
func makeGetImpactHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		symbol := request.GetString("symbol", "")
		if symbol == "" {
			return mcp.NewToolResultError("symbol is required"), nil
		}
		maxDepth := min(max(request.GetInt("depth", 2), 1), 5)
		direction := request.GetString("direction", "both")
		if direction != "callers" && direction != "callees" && direction != "both" {
			return mcp.NewToolResultError("direction must be 'callers', 'callees', or 'both'"), nil
		}
		kind, errResult := validateKindParam(request)
		if errResult != nil {
			return errResult, nil
		}

		// Resolve starting definition(s) for the symbol via the defs map.
		dp, ok := g.(graph.DefsMapper)
		if !ok {
			return mcp.NewToolResultError("backend does not support impact analysis (no defs map)"), nil
		}
		defs := dp.DefsMap()
		roots, found := defs[symbol]
		if !found {
			// Case-insensitive fallback
			symbolLower := strings.ToLower(symbol)
			for token, ids := range defs {
				if strings.ToLower(token) == symbolLower {
					roots = ids
					found = true
					break
				}
			}
		}
		// Apply optional kind filter to the starting roots. get_impact
		// is rooted at a specific symbol-of-a-kind; narrowing by kind
		// here means "trace impact of THIS kind of Encoder," not "trace
		// impact through nodes whose kind matches" — depth>0 traversal
		// follows callers/callees regardless of their construct kind.
		if found && kind != "" {
			roots, _ = filterDirIDsByKindGraph(g, roots, kind)
			if len(roots) == 0 {
				found = false
			}
		}
		if !found || len(roots) == 0 {
			if kind != "" {
				return mcp.NewToolResultText(fmt.Sprintf(`{"symbol":%q,"kind":%q,"error":"no definition found with kind=%s"}`, symbol, kind, kind)), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf(`{"symbol":%q,"error":"no definition found"}`, symbol)), nil
		}

		// BFS state
		type impactNode struct {
			Path      string `json:"path"`
			Depth     int    `json:"depth"`
			Direction string `json:"direction"` // "root", "caller", "callee"
		}

		seen := make(map[string]bool)
		var result []impactNode
		truncated := false

		// Seed the BFS with the definition root(s)
		type bfsEntry struct {
			id    string
			depth int
		}
		var queue []bfsEntry
		for _, root := range roots {
			if !seen[root] {
				seen[root] = true
				result = append(result, impactNode{Path: root, Depth: 0, Direction: "root"})
				queue = append(queue, bfsEntry{id: root, depth: 0})
			}
		}

		// BFS traversal
		for len(queue) > 0 {
			if len(result) >= maxImpactNodes {
				truncated = true
				break
			}

			entry := queue[0]
			queue = queue[1:]

			if entry.depth >= maxDepth {
				continue
			}

			// Callers: the directory name IS the token for GetCallers.
			// Skip generic names (Close, String, Error, etc.) at depth > 0 to
			// avoid explosion — these ubiquitous tokens would pull in most of
			// the codebase and drown out the real impact signal.
			if direction == "callers" || direction == "both" {
				token := tokenForNode(g, entry.id)
				if entry.depth == 0 || !genericNames[token] {
					callers, err := g.GetCallers(token)
					if err == nil {
						for _, c := range callers {
							if len(result) >= maxImpactNodes {
								truncated = true
								break
							}
							if !seen[c.ID] {
								seen[c.ID] = true
								result = append(result, impactNode{
									Path:      c.ID,
									Depth:     entry.depth + 1,
									Direction: "caller",
								})
								queue = append(queue, bfsEntry{id: c.ID, depth: entry.depth + 1})
							}
						}
					}
				}
			}

			if truncated {
				break
			}

			// Callees: GetCallees takes a construct directory path
			if direction == "callees" || direction == "both" {
				callees, err := g.GetCallees(entry.id)
				if err == nil {
					for _, c := range callees {
						if len(result) >= maxImpactNodes {
							truncated = true
							break
						}
						if !seen[c.ID] {
							seen[c.ID] = true
							result = append(result, impactNode{
								Path:      c.ID,
								Depth:     entry.depth + 1,
								Direction: "callee",
							})
							queue = append(queue, bfsEntry{id: c.ID, depth: entry.depth + 1})
						}
					}
				}
			}

			if truncated {
				break
			}
		}

		type impactResult struct {
			Symbol    string       `json:"symbol"`
			Roots     []string     `json:"roots"`
			Depth     int          `json:"depth"`
			Direction string       `json:"direction"`
			Total     int          `json:"total"`
			Truncated bool         `json:"truncated,omitempty"`
			Nodes     []impactNode `json:"nodes"`
		}
		out := impactResult{
			Symbol:    symbol,
			Roots:     roots,
			Depth:     maxDepth,
			Direction: direction,
			Total:     len(result),
			Truncated: truncated,
			Nodes:     result,
		}
		data, _ := json.MarshalIndent(out, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
}
