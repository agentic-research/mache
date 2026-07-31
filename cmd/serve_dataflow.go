package cmd

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// maxDataflowNodes bounds reference-flow responses on dense repositories.
const maxDataflowNodes = 500

type dataflowNode struct {
	Path  string `json:"path"`
	Depth int    `json:"depth"`
}

type dataflowEdge struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Direction string `json:"direction"`
	Evidence  string `json:"evidence"`
}

type dataflowResult struct {
	Symbol    string         `json:"symbol"`
	Roots     []string       `json:"roots"`
	Nodes     []dataflowNode `json:"nodes"`
	Edges     []dataflowEdge `json:"edges"`
	Truncated bool           `json:"truncated"`
}

// makeGetDataflowHandler returns a deterministic, bounded traversal of the
// existing node_refs-backed caller/callee projection. It reports reference
// flow only; the edges do not claim LSP-confirmed binding, SSA, taint, or data
// dependence.
func makeGetDataflowHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		symbol := request.GetString("symbol", "")
		if symbol == "" {
			return mcp.NewToolResultError("symbol is required"), nil
		}

		direction := request.GetString("direction", "both")
		if direction != "callers" && direction != "callees" && direction != "both" {
			return mcp.NewToolResultError("direction must be 'callers', 'callees', or 'both'"), nil
		}
		kind, errResult := validateKindParam(request)
		if errResult != nil {
			return errResult, nil
		}
		maxDepth := min(max(request.GetInt("depth", 2), 1), 5)

		dp, ok := g.(defsMapProvider)
		if !ok {
			return mcp.NewToolResultError("backend does not support reference flow (no defs map)"), nil
		}
		roots := resolveDataflowRoots(dp.DefsMap(), symbol)
		if kind != "" {
			roots, _ = filterDirIDsByKindGraph(g, roots, kind)
		}
		if len(roots) == 0 {
			data, _ := json.Marshal(dataflowResult{
				Symbol: symbol,
				Roots:  []string{},
				Nodes:  []dataflowNode{},
				Edges:  []dataflowEdge{},
			})
			return mcp.NewToolResultText(string(data)), nil
		}

		type queueEntry struct {
			id    string
			depth int
		}
		seen := make(map[string]bool, min(len(roots), maxDataflowNodes))
		nodeDepth := make(map[string]int, min(len(roots), maxDataflowNodes))
		queue := make([]queueEntry, 0, min(len(roots), maxDataflowNodes))
		truncated := false
		for _, root := range roots {
			if len(seen) == maxDataflowNodes {
				truncated = true
				break
			}
			if !seen[root] {
				seen[root] = true
				nodeDepth[root] = 0
				queue = append(queue, queueEntry{id: root})
			}
		}
		roots = roots[:len(queue)]

		edges := make([]dataflowEdge, 0)
		edgeSeen := make(map[dataflowEdge]bool)
		for len(queue) > 0 && !truncated {
			entry := queue[0]
			queue = queue[1:]
			if entry.depth >= maxDepth {
				continue
			}

			if direction == "callers" || direction == "both" {
				callers, err := g.GetCallers(filepath.Base(entry.id))
				if err == nil {
					ids := sortedDataflowNodeIDs(callers)
					for _, id := range ids {
						edge := dataflowEdge{From: id, To: entry.id, Direction: "caller", Evidence: "node_ref"}
						if !edgeSeen[edge] && len(edges) == maxDataflowNodes {
							truncated = true
							break
						}
						if !seen[id] && len(seen) == maxDataflowNodes {
							truncated = true
							break
						}
						if !seen[id] {
							seen[id] = true
							nodeDepth[id] = entry.depth + 1
							queue = append(queue, queueEntry{id: id, depth: entry.depth + 1})
						}
						if !edgeSeen[edge] {
							edgeSeen[edge] = true
							edges = append(edges, edge)
						}
					}
				}
			}
			if truncated {
				break
			}

			if direction == "callees" || direction == "both" {
				callees, err := g.GetCallees(entry.id)
				if err == nil {
					ids := sortedDataflowNodeIDs(callees)
					for _, id := range ids {
						edge := dataflowEdge{From: entry.id, To: id, Direction: "callee", Evidence: "node_ref"}
						if !edgeSeen[edge] && len(edges) == maxDataflowNodes {
							truncated = true
							break
						}
						if !seen[id] && len(seen) == maxDataflowNodes {
							truncated = true
							break
						}
						if !seen[id] {
							seen[id] = true
							nodeDepth[id] = entry.depth + 1
							queue = append(queue, queueEntry{id: id, depth: entry.depth + 1})
						}
						if !edgeSeen[edge] {
							edgeSeen[edge] = true
							edges = append(edges, edge)
						}
					}
				}
			}
		}

		nodes := make([]dataflowNode, 0, len(nodeDepth))
		for path, depth := range nodeDepth {
			nodes = append(nodes, dataflowNode{Path: path, Depth: depth})
		}
		sort.Slice(nodes, func(i, j int) bool {
			if nodes[i].Path != nodes[j].Path {
				return nodes[i].Path < nodes[j].Path
			}
			return nodes[i].Depth < nodes[j].Depth
		})
		sort.Slice(edges, func(i, j int) bool {
			if edges[i].From != edges[j].From {
				return edges[i].From < edges[j].From
			}
			if edges[i].To != edges[j].To {
				return edges[i].To < edges[j].To
			}
			return edges[i].Direction < edges[j].Direction
		})

		data, _ := json.Marshal(dataflowResult{
			Symbol:    symbol,
			Roots:     roots,
			Nodes:     nodes,
			Edges:     edges,
			Truncated: truncated,
		})
		return mcp.NewToolResultText(string(data)), nil
	}
}

func resolveDataflowRoots(defs map[string][]string, symbol string) []string {
	roots := append([]string(nil), defs[symbol]...)
	if len(roots) == 0 {
		var tokens []string
		for token := range defs {
			if strings.EqualFold(token, symbol) {
				tokens = append(tokens, token)
			}
		}
		sort.Strings(tokens)
		for _, token := range tokens {
			roots = append(roots, defs[token]...)
		}
	}
	return sortedUniqueStrings(roots)
}

func sortedDataflowNodeIDs(nodes []*graph.Node) []string {
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node != nil && node.ID != "" {
			ids = append(ids, node.ID)
		}
	}
	return sortedUniqueStrings(ids)
}

func sortedUniqueStrings(values []string) []string {
	sort.Strings(values)
	out := values[:0]
	for _, value := range values {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}
