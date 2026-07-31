package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// maxDataflowItems is one shared response budget across nodes and edges.
// Roots are metadata, but every root also appears in nodes and therefore costs
// one item. Discovering a new neighbor normally costs two items atomically:
// one node plus its connecting edge. We never emit an edge to an omitted node.
const maxDataflowItems = 500

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

// dataflowEdgeIdentity is the underlying node_ref identity. Direction records
// how traversal discovered an edge, not distinct evidence, so it is excluded
// from deduplication: callers and callees may expose the same from/to relation.
type dataflowEdgeIdentity struct {
	From     string
	To       string
	Evidence string
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
		maxDepth := request.GetInt("depth", 2)
		if maxDepth < 1 || maxDepth > 5 {
			return mcp.NewToolResultError("depth must be between 1 and 5"), nil
		}

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
		seen := make(map[string]bool, min(len(roots), maxDataflowItems))
		nodeDepth := make(map[string]int, min(len(roots), maxDataflowItems))
		queue := make([]queueEntry, 0, min(len(roots), maxDataflowItems))
		truncated := false
		responseItems := 0
		for _, root := range roots {
			if responseItems == maxDataflowItems {
				truncated = true
				break
			}
			if !seen[root] {
				seen[root] = true
				nodeDepth[root] = 0
				queue = append(queue, queueEntry{id: root})
				responseItems++
			}
		}
		roots = roots[:len(queue)]

		edges := make([]dataflowEdge, 0)
		edgeSeen := make(map[dataflowEdgeIdentity]bool)
		for len(queue) > 0 && !truncated {
			entry := queue[0]
			queue = queue[1:]
			if entry.depth >= maxDepth {
				continue
			}

			if direction == "callers" || direction == "both" {
				callers, err := g.GetCallers(filepath.Base(entry.id))
				if err != nil {
					return mcp.NewToolResultError(fmt.Sprintf("get callers for %q: %v", entry.id, err)), nil
				}
				ids := sortedDataflowNodeIDs(callers)
				for _, id := range ids {
					edge := dataflowEdge{From: id, To: entry.id, Direction: "caller", Evidence: "node_ref"}
					identity := dataflowEdgeIdentity{From: edge.From, To: edge.To, Evidence: edge.Evidence}
					newNode := !seen[id]
					newEdge := !edgeSeen[identity]
					additionalItems := 0
					if newNode {
						additionalItems++
					}
					if newEdge {
						additionalItems++
					}
					if responseItems+additionalItems > maxDataflowItems {
						truncated = true
						break
					}
					if newNode {
						seen[id] = true
						nodeDepth[id] = entry.depth + 1
						queue = append(queue, queueEntry{id: id, depth: entry.depth + 1})
						responseItems++
					}
					if newEdge {
						edgeSeen[identity] = true
						edges = append(edges, edge)
						responseItems++
					}
				}
			}
			if truncated {
				break
			}

			if direction == "callees" || direction == "both" {
				callees, err := g.GetCallees(entry.id)
				if err != nil {
					return mcp.NewToolResultError(fmt.Sprintf("get callees for %q: %v", entry.id, err)), nil
				}
				ids := sortedDataflowNodeIDs(callees)
				for _, id := range ids {
					edge := dataflowEdge{From: entry.id, To: id, Direction: "callee", Evidence: "node_ref"}
					identity := dataflowEdgeIdentity{From: edge.From, To: edge.To, Evidence: edge.Evidence}
					newNode := !seen[id]
					newEdge := !edgeSeen[identity]
					additionalItems := 0
					if newNode {
						additionalItems++
					}
					if newEdge {
						additionalItems++
					}
					if responseItems+additionalItems > maxDataflowItems {
						truncated = true
						break
					}
					if newNode {
						seen[id] = true
						nodeDepth[id] = entry.depth + 1
						queue = append(queue, queueEntry{id: id, depth: entry.depth + 1})
						responseItems++
					}
					if newEdge {
						edgeSeen[identity] = true
						edges = append(edges, edge)
						responseItems++
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
