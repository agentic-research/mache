package mcpserve

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/agentic-research/mache/graph"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func makeGetArchitectureHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		arch := archReport{TopLevelBreakdown: make(map[string]int)}
		if err := arch.walkTree(g); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if rp, ok := g.(graph.RefsMapper); ok {
			arch.addReferences(rp.RefsMap())
		}
		if dp, ok := g.(graph.DefsMapper); ok {
			arch.addDefinitions(dp.DefsMap())
		}
		data, _ := json.MarshalIndent(arch, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
}

// archReport is get_architecture's answer.
type archReport struct {
	MostReferenced    []archEntryPoint     `json:"most_referenced"`
	KeyAbstractions   []archKeyAbstraction `json:"key_abstractions"`
	DependencyLayers  []archLayer          `json:"dependency_layers"`
	TestFiles         []string             `json:"test_files"`
	APISurface        []string             `json:"api_surface"`
	FileCount         int                  `json:"file_count"`
	TopLevelBreakdown map[string]int       `json:"top_level_breakdown"`
}

type archEntryPoint struct {
	Symbol string `json:"symbol"`
	FanIn  int    `json:"fan_in"`
}

type archKeyAbstraction struct {
	Symbol string   `json:"symbol"`
	DefIDs []string `json:"def_ids"`
}

type archLayer struct {
	ID         int      `json:"id"`
	Size       int      `json:"size"`
	TopMembers []string `json:"top_members"`
	Note       string   `json:"note,omitempty"`
}

// walkTree BFS-walks the graph to count files, tally top-level segments, and
// find test files.
func (a *archReport) walkTree(g graph.Graph) error {
	var testFiles []string
	roots, err := g.ListChildren("")
	if err != nil {
		return fmt.Errorf("list root: %w", err)
	}
	queue := make([]string, 0, len(roots))
	seen := make(map[string]bool, len(roots))
	for _, r := range roots {
		seen[r] = true
	}
	queue = append(queue, roots...)
	const maxNodes = 50000
	visited := 0
	for len(queue) > 0 && visited < maxNodes {
		id := queue[0]
		queue = queue[1:]
		visited++

		node, nodeErr := g.GetNode(id)
		if nodeErr != nil {
			continue
		}
		if node.Mode.IsDir() {
			children, _ := g.ListChildren(id)
			for _, childID := range children {
				if !seen[childID] {
					seen[childID] = true
					queue = append(queue, childID)
				}
			}
			continue
		}

		a.FileCount++

		parts := strings.SplitN(id, "/", 2)
		if len(parts) > 0 {
			a.TopLevelBreakdown[parts[0]]++
		}

		name := filepath.Base(id)
		dirName := filepath.Base(filepath.Dir(id))
		if strings.HasPrefix(dirName, "Test") || strings.HasPrefix(dirName, "Benchmark") ||
			strings.HasSuffix(name, "_test") || strings.Contains(id, "/test/") ||
			strings.Contains(id, "/tests/") {
			testFiles = append(testFiles, strings.TrimSuffix(id, "/source"))
		}
	}
	if len(testFiles) > 50 {
		testFiles = testFiles[:50]
	}
	a.TestFiles = testFiles
	return nil
}

// addReferences fills the symbols with the highest fan-in (most referencing
// nodes) and the dependency layers.
func (a *archReport) addReferences(refs map[string][]string) {
	for _, token := range topKeysBySize(refs, 20) {
		a.MostReferenced = append(a.MostReferenced, archEntryPoint{
			Symbol: token,
			FanIn:  len(refs[token]),
		})
	}
	a.DependencyLayers = dependencyLayers(refs)
}

// dependencyLayers groups the graph via community detection.
//
// This used to skip entirely when len(refs) exceeded 5000, which measured the
// WRONG QUANTITY: the projection's cost is Σ K(K-1)/2 over tokens, not the
// token count. On mache that is 12,275 tokens against 84,813,280 edge
// insertions — the guard was off by ~7000x, so it refused a graph it could
// have handled while a pathological one with few hot tokens would have sailed
// through.
//
// DetectCommunities now prunes hub tokens and clamps itself to a pair budget,
// so it is bounded by construction and returns a (possibly tightened) answer
// instead of nothing. Measured: 32.5s and modularity 0.543 before, 215ms and
// 0.985 after — the guard was hiding a wrong answer, not just a slow one.
func dependencyLayers(refs map[string][]string) []archLayer {
	result := graph.DetectCommunities(refs, 2)
	// Bead mache-9cd921: get_architecture previously appended every community
	// returned by Louvain, blowing past the MCP response budget on medium
	// codebases (486K chars on 28K LOC). Cap to the top N largest communities;
	// the elided count rides as a note in the final layer.
	const maxLayers = 25
	sort.Slice(result.Communities, func(i, j int) bool {
		return len(result.Communities[i].Members) > len(result.Communities[j].Members)
	})
	keep := result.Communities
	elided := 0
	if len(keep) > maxLayers {
		elided = len(keep) - maxLayers
		keep = keep[:maxLayers]
	}
	var layers []archLayer
	for _, c := range keep {
		top := c.Members
		if len(top) > 5 {
			top = top[:5]
		}
		cleaned := make([]string, len(top))
		for i, m := range top {
			cleaned[i] = strings.TrimSuffix(m, "/source")
		}
		layers = append(layers, archLayer{
			ID:         c.ID,
			Size:       len(c.Members),
			TopMembers: cleaned,
		})
	}
	if elided > 0 {
		layers = append(layers, archLayer{
			Note: fmt.Sprintf("+%d smaller communities elided (showing top %d by size)", elided, maxLayers),
		})
	}
	return layers
}

// addDefinitions fills the symbols with the most definition sites, and the API
// surface (exported symbols: uppercase first rune).
func (a *archReport) addDefinitions(defs map[string][]string) {
	// Cap DefIDs per symbol — a common-name symbol like "Error" can have dozens
	// of defs (interfaces, test mocks, etc.). Without a cap, KeyAbstractions
	// alone could push the response past the MCP budget. Bead mache-9cd921.
	const maxDefsPerSymbol = 8
	for _, symbol := range topKeysBySize(defs, 20) {
		ids := defs[symbol]
		if len(ids) > maxDefsPerSymbol {
			ids = ids[:maxDefsPerSymbol]
		}
		a.KeyAbstractions = append(a.KeyAbstractions, archKeyAbstraction{
			Symbol: symbol,
			DefIDs: ids,
		})
	}

	var exported []string
	for symbol := range defs {
		if len(symbol) > 0 {
			r, _ := utf8.DecodeRuneInString(symbol)
			if unicode.IsUpper(r) {
				exported = append(exported, symbol)
			}
		}
	}
	sort.Strings(exported)
	if len(exported) > 100 {
		exported = exported[:100]
	}
	a.APISurface = exported
}

// topKeysBySize returns the n keys of m with the most values, largest first.
//
// The order is TOTAL: ties break on the key. m is a Go map, whose iteration
// order is randomized, so without the tie-break equal sizes came out in a
// different order on every call and a tie at the cut changed which keys were
// returned at all (mache-5e2cf4).
func topKeysBySize(m map[string][]string, n int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(m[keys[i]]) != len(m[keys[j]]) {
			return len(m[keys[i]]) > len(m[keys[j]])
		}
		return keys[i] < keys[j]
	})
	return keys[:min(len(keys), n)]
}
