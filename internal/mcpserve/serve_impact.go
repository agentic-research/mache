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
		roots, errResult := impactRoots(g, symbol, kind)
		if errResult != nil {
			return errResult, nil
		}

		w := &impactWalk{g: g, maxDepth: maxDepth, direction: direction, seen: map[string]bool{}}
		w.seed(roots)
		w.run()

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
			Total:     len(w.nodes),
			Truncated: w.truncated,
			Nodes:     w.nodes,
		}
		data, _ := json.MarshalIndent(out, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
}

// impactRoots resolves the definition(s) an impact walk starts from: symbol's
// definitions, matched case-insensitively when there is no exact one, then
// narrowed to kind. get_impact is rooted at a specific symbol-of-a-kind:
// narrowing here means "trace impact of THIS kind of Encoder", not "trace
// impact through nodes whose kind matches"; the walk follows callers and
// callees regardless of their kind. errResult is the tool's answer when there
// is nothing to start from.
func impactRoots(g graph.Graph, symbol, kind string) (roots []string, errResult *mcp.CallToolResult) {
	dp, ok := g.(graph.DefsMapper)
	if !ok {
		return nil, mcp.NewToolResultError("backend does not support impact analysis (no defs map)")
	}
	defs := dp.DefsMap()
	roots, found := defs[symbol]
	if !found {
		symbolLower := strings.ToLower(symbol)
		for token, ids := range defs {
			if strings.ToLower(token) == symbolLower {
				roots = ids
				found = true
				break
			}
		}
	}
	if found && kind != "" {
		roots, _ = filterDirIDsByKindGraph(g, roots, kind)
	}
	if len(roots) == 0 {
		if kind != "" {
			return nil, mcp.NewToolResultText(fmt.Sprintf(`{"symbol":%q,"kind":%q,"error":"no definition found with kind=%s"}`, symbol, kind, kind))
		}
		return nil, mcp.NewToolResultText(fmt.Sprintf(`{"symbol":%q,"error":"no definition found"}`, symbol))
	}
	return roots, nil
}

// impactNode is one node of an impact walk's answer.
type impactNode struct {
	Path      string `json:"path"`
	Depth     int    `json:"depth"`
	Direction string `json:"direction"` // "root", "caller", "callee"
}

// impactStep is a node waiting in the walk's queue.
type impactStep struct {
	id    string
	depth int
}

// impactWalk is a breadth-first walk out from the roots, through callers,
// callees or both, to maxDepth, stopping at maxImpactNodes.
type impactWalk struct {
	g         graph.Graph
	maxDepth  int
	direction string

	seen      map[string]bool
	nodes     []impactNode
	queue     []impactStep
	truncated bool
}

// seed starts the walk at roots. The cap does not apply to them.
func (w *impactWalk) seed(roots []string) {
	for _, root := range roots {
		if !w.seen[root] {
			w.seen[root] = true
			w.nodes = append(w.nodes, impactNode{Path: root, Depth: 0, Direction: "root"})
			w.queue = append(w.queue, impactStep{id: root, depth: 0})
		}
	}
}

func (w *impactWalk) run() {
	for len(w.queue) > 0 {
		if len(w.nodes) >= maxImpactNodes {
			w.truncated = true
			return
		}
		step := w.queue[0]
		w.queue = w.queue[1:]
		if step.depth >= w.maxDepth {
			continue
		}
		if w.direction == "callers" || w.direction == "both" {
			if !w.expand(w.callersOf(step), step.depth+1, "caller") {
				return
			}
		}
		if w.direction == "callees" || w.direction == "both" {
			callees, err := w.g.GetCallees(step.id) // a construct directory path
			if err == nil && !w.expand(callees, step.depth+1, "callee") {
				return
			}
		}
	}
}

// callersOf is what calls step's node, by the symbol the node defines. Generic
// names (Close, String, Error, ...) are not followed past the root: those
// ubiquitous tokens would pull in most of the codebase and drown out the real
// impact signal.
func (w *impactWalk) callersOf(step impactStep) []*graph.Node {
	token := graph.DefTokenFor(w.g, step.id)
	if step.depth > 0 && genericNames[token] {
		return nil
	}
	callers, err := w.g.GetCallers(token)
	if err != nil {
		return nil
	}
	return callers
}

// expand queues each of next not yet seen; false when the cap stopped it.
func (w *impactWalk) expand(next []*graph.Node, depth int, direction string) bool {
	for _, n := range next {
		if len(w.nodes) >= maxImpactNodes {
			w.truncated = true
			return false
		}
		if !w.seen[n.ID] {
			w.seen[n.ID] = true
			w.nodes = append(w.nodes, impactNode{Path: n.ID, Depth: depth, Direction: direction})
			w.queue = append(w.queue, impactStep{id: n.ID, depth: depth})
		}
	}
	return true
}
