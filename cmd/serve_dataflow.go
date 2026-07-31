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

type dataflowRequest struct {
	symbol    string
	direction string
	kind      string
	maxDepth  int
}

type dataflowQueueEntry struct {
	id    string
	depth int
}

type dataflowTraversal struct {
	g             graph.Graph
	direction     string
	maxDepth      int
	seen          map[string]bool
	nodeDepth     map[string]int
	queue         []dataflowQueueEntry
	edges         []dataflowEdge
	edgeSeen      map[dataflowEdgeIdentity]bool
	responseItems int
	truncated     bool
}

// makeGetDataflowHandler returns a deterministic, bounded traversal of the
// existing node_refs-backed caller/callee projection. It reports reference
// flow only; the edges do not claim LSP-confirmed binding, SSA, taint, or data
// dependence.
func makeGetDataflowHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		params, errResult := parseDataflowRequest(request)
		if errResult != nil {
			return errResult, nil
		}

		dp, ok := g.(defsMapProvider)
		if !ok {
			return mcp.NewToolResultError("backend does not support reference flow (no defs map)"), nil
		}
		roots := resolveDataflowRoots(dp.DefsMap(), params.symbol)
		if params.kind != "" {
			roots, _ = filterDirIDsByKindGraph(g, roots, params.kind)
		}
		if len(roots) == 0 {
			return marshalDataflowResult(dataflowResult{
				Symbol: params.symbol,
				Roots:  []string{},
				Nodes:  []dataflowNode{},
				Edges:  []dataflowEdge{},
			}), nil
		}

		traversal := newDataflowTraversal(g, params.direction, params.maxDepth, len(roots))
		roots = traversal.seedRoots(roots)
		if err := traversal.walk(); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return marshalDataflowResult(traversal.result(params.symbol, roots)), nil
	}
}

func parseDataflowRequest(request mcp.CallToolRequest) (dataflowRequest, *mcp.CallToolResult) {
	params := dataflowRequest{
		symbol:    request.GetString("symbol", ""),
		direction: request.GetString("direction", "both"),
		maxDepth:  request.GetInt("depth", 2),
	}
	if params.symbol == "" {
		return dataflowRequest{}, mcp.NewToolResultError("symbol is required")
	}
	if params.direction != "callers" && params.direction != "callees" && params.direction != "both" {
		return dataflowRequest{}, mcp.NewToolResultError("direction must be 'callers', 'callees', or 'both'")
	}
	kind, errResult := validateKindParam(request)
	if errResult != nil {
		return dataflowRequest{}, errResult
	}
	params.kind = kind
	if params.maxDepth < 1 || params.maxDepth > 5 {
		return dataflowRequest{}, mcp.NewToolResultError("depth must be between 1 and 5")
	}
	return params, nil
}

func newDataflowTraversal(g graph.Graph, direction string, maxDepth, rootCount int) *dataflowTraversal {
	capacity := min(rootCount, maxDataflowItems)
	return &dataflowTraversal{
		g:         g,
		direction: direction,
		maxDepth:  maxDepth,
		seen:      make(map[string]bool, capacity),
		nodeDepth: make(map[string]int, capacity),
		queue:     make([]dataflowQueueEntry, 0, capacity),
		edges:     make([]dataflowEdge, 0),
		edgeSeen:  make(map[dataflowEdgeIdentity]bool),
	}
}

func (t *dataflowTraversal) seedRoots(roots []string) []string {
	seeded := make([]string, 0, min(len(roots), maxDataflowItems))
	for _, root := range roots {
		if t.responseItems == maxDataflowItems {
			t.truncated = true
			break
		}
		if t.seen[root] {
			continue
		}
		t.seen[root] = true
		t.nodeDepth[root] = 0
		t.queue = append(t.queue, dataflowQueueEntry{id: root})
		seeded = append(seeded, root)
		t.responseItems++
	}
	return seeded
}

func (t *dataflowTraversal) walk() error {
	for len(t.queue) > 0 && !t.truncated {
		entry := t.queue[0]
		t.queue = t.queue[1:]
		if entry.depth >= t.maxDepth {
			continue
		}
		if err := t.expandCallers(entry); err != nil {
			return err
		}
		if !t.truncated {
			if err := t.expandCallees(entry); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *dataflowTraversal) expandCallers(entry dataflowQueueEntry) error {
	if t.direction != "callers" && t.direction != "both" {
		return nil
	}
	callers, err := t.g.GetCallers(filepath.Base(entry.id))
	if err != nil {
		return fmt.Errorf("get callers for %q: %v", entry.id, err)
	}
	t.admitNeighbors(entry, sortedDataflowCallerIDs(t.g, callers), "caller")
	return nil
}

func (t *dataflowTraversal) expandCallees(entry dataflowQueueEntry) error {
	if t.direction != "callees" && t.direction != "both" {
		return nil
	}
	callees, err := t.g.GetCallees(entry.id)
	if err != nil {
		return fmt.Errorf("get callees for %q: %v", entry.id, err)
	}
	t.admitNeighbors(entry, sortedDataflowNodeIDs(callees), "callee")
	return nil
}

func (t *dataflowTraversal) admitNeighbors(entry dataflowQueueEntry, ids []string, direction string) {
	for _, id := range ids {
		edge := dataflowEdge{From: entry.id, To: id, Direction: direction, Evidence: "node_ref"}
		if direction == "caller" {
			edge.From, edge.To = id, entry.id
		}
		identity := dataflowEdgeIdentity{From: edge.From, To: edge.To, Evidence: edge.Evidence}
		newNode, newEdge := !t.seen[id], !t.edgeSeen[identity]
		additionalItems := boolInt(newNode) + boolInt(newEdge)
		if t.responseItems+additionalItems > maxDataflowItems {
			t.truncated = true
			return
		}
		if newNode {
			t.seen[id] = true
			t.nodeDepth[id] = entry.depth + 1
			t.queue = append(t.queue, dataflowQueueEntry{id: id, depth: entry.depth + 1})
			t.responseItems++
		}
		if newEdge {
			t.edgeSeen[identity] = true
			t.edges = append(t.edges, edge)
			t.responseItems++
		}
	}
}

func (t *dataflowTraversal) result(symbol string, roots []string) dataflowResult {
	nodes := make([]dataflowNode, 0, len(t.nodeDepth))
	for path, depth := range t.nodeDepth {
		nodes = append(nodes, dataflowNode{Path: path, Depth: depth})
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Path != nodes[j].Path {
			return nodes[i].Path < nodes[j].Path
		}
		return nodes[i].Depth < nodes[j].Depth
	})
	sort.Slice(t.edges, func(i, j int) bool {
		if t.edges[i].From != t.edges[j].From {
			return t.edges[i].From < t.edges[j].From
		}
		if t.edges[i].To != t.edges[j].To {
			return t.edges[i].To < t.edges[j].To
		}
		return t.edges[i].Direction < t.edges[j].Direction
	})
	return dataflowResult{Symbol: symbol, Roots: roots, Nodes: nodes, Edges: t.edges, Truncated: t.truncated}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func marshalDataflowResult(result dataflowResult) *mcp.CallToolResult {
	data, _ := json.Marshal(result)
	return mcp.NewToolResultText(string(data))
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

// sortedDataflowCallerIDs converts the production node_refs call-site shape
// (a construct's non-directory "source" child) back to the construct ID that
// Graph.GetCallers/GetCallees can traverse. Older and synthetic backends may
// already return construct directories, which remain unchanged.
func sortedDataflowCallerIDs(g graph.Graph, nodes []*graph.Node) []string {
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node == nil || node.ID == "" {
			continue
		}
		id := node.ID
		if !node.Mode.IsDir() && filepath.Base(id) == "source" {
			parentID := filepath.Dir(id)
			if parent, err := g.GetNode(parentID); err == nil && parent.Mode.IsDir() {
				id = parentID
			}
		}
		ids = append(ids, id)
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
