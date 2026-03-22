package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func makeGetArchitectureHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		type entryPoint struct {
			Symbol string `json:"symbol"`
			FanIn  int    `json:"fan_in"`
		}
		type keyAbstraction struct {
			Symbol string   `json:"symbol"`
			DefIDs []string `json:"def_ids"`
		}
		type dependencyLayer struct {
			ID         int      `json:"id"`
			Size       int      `json:"size"`
			TopMembers []string `json:"top_members"`
			Note       string   `json:"note,omitempty"`
		}
		type architecture struct {
			MostReferenced    []entryPoint      `json:"most_referenced"`
			KeyAbstractions   []keyAbstraction  `json:"key_abstractions"`
			DependencyLayers  []dependencyLayer `json:"dependency_layers"`
			TestFiles         []string          `json:"test_files"`
			APISurface        []string          `json:"api_surface"`
			FileCount         int               `json:"file_count"`
			TopLevelBreakdown map[string]int    `json:"top_level_breakdown"`
		}

		arch := architecture{
			TopLevelBreakdown: make(map[string]int),
		}

		// BFS walk to count files, detect top-level segments, find test files.
		var testFiles []string
		roots, err := g.ListChildren("")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("list root: %v", err)), nil
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

			arch.FileCount++

			parts := strings.SplitN(id, "/", 2)
			if len(parts) > 0 {
				arch.TopLevelBreakdown[parts[0]]++
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
		arch.TestFiles = testFiles

		// Most referenced: symbols with highest fan-in (most referencing nodes).
		if rp, ok := g.(refsMapProvider); ok {
			refs := rp.RefsMap()
			type tokenCount struct {
				token string
				count int
			}
			counts := make([]tokenCount, 0, len(refs))
			for token, nodeIDs := range refs {
				counts = append(counts, tokenCount{token, len(nodeIDs)})
			}
			sort.Slice(counts, func(i, j int) bool {
				return counts[i].count > counts[j].count
			})
			limit := 20
			if len(counts) < limit {
				limit = len(counts)
			}
			for _, tc := range counts[:limit] {
				arch.MostReferenced = append(arch.MostReferenced, entryPoint{
					Symbol: tc.token,
					FanIn:  tc.count,
				})
			}

			// Dependency layers via community detection.
			// Skip for large graphs to avoid O(n^2) cost.
			const communityLimit = 5000
			if len(refs) > communityLimit {
				arch.DependencyLayers = []dependencyLayer{{
					Note: fmt.Sprintf("skipped: refs count %d exceeds %d threshold", len(refs), communityLimit),
				}}
			} else {
				result := graph.DetectCommunities(refs, 2)
				for _, c := range result.Communities {
					top := c.Members
					if len(top) > 5 {
						top = top[:5]
					}
					cleaned := make([]string, len(top))
					for i, m := range top {
						cleaned[i] = strings.TrimSuffix(m, "/source")
					}
					arch.DependencyLayers = append(arch.DependencyLayers, dependencyLayer{
						ID:         c.ID,
						Size:       len(c.Members),
						TopMembers: cleaned,
					})
				}
			}
		}

		// Key abstractions: symbols with most definition sites.
		if dp, ok := g.(defsMapProvider); ok {
			defs := dp.DefsMap()
			type symDef struct {
				symbol string
				ids    []string
			}
			syms := make([]symDef, 0, len(defs))
			for symbol, ids := range defs {
				syms = append(syms, symDef{symbol, ids})
			}
			sort.Slice(syms, func(i, j int) bool {
				return len(syms[i].ids) > len(syms[j].ids)
			})
			limit := 20
			if len(syms) < limit {
				limit = len(syms)
			}
			for _, sd := range syms[:limit] {
				arch.KeyAbstractions = append(arch.KeyAbstractions, keyAbstraction{
					Symbol: sd.symbol,
					DefIDs: sd.ids,
				})
			}

			// API surface: exported symbols (uppercase first rune).
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
			arch.APISurface = exported
		}

		data, _ := json.MarshalIndent(arch, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
}
