package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func makeGetOverviewHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		type dirInfo struct {
			Name     string `json:"name"`
			Path     string `json:"path"`
			Children int    `json:"children"`
		}
		type overview struct {
			TopLevel   []dirInfo         `json:"top_level"`
			TotalDirs  int               `json:"total_dirs"`
			TotalFiles int               `json:"total_files"`
			RefTokens  int               `json:"ref_tokens,omitempty"`
			DefTokens  int               `json:"def_tokens,omitempty"`
			Usage      map[string]string `json:"_usage,omitempty"`
		}

		ov := overview{}

		// Top-level structure
		children, err := g.ListChildren("")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("list root: %v", err)), nil
		}

		for _, childID := range children {
			node, err := g.GetNode(childID)
			if err != nil {
				continue
			}
			if node.Mode.IsDir() {
				ov.TotalDirs++
				subChildren, _ := g.ListChildren(childID)
				ov.TopLevel = append(ov.TopLevel, dirInfo{
					Name:     filepath.Base(childID),
					Path:     childID,
					Children: len(subChildren),
				})
			} else {
				ov.TotalFiles++
			}
		}

		// Count refs/defs if available
		if rp, ok := g.(refsMapProvider); ok {
			ov.RefTokens = len(rp.RefsMap())
		}
		if dp, ok := g.(defsMapProvider); ok {
			ov.DefTokens = len(dp.DefsMap())
		}

		// Embed tool routing hints when a code graph is indexed (has cross-references).
		// This teaches the LLM which tool to use for each task without bloating the
		// system prompt — guidance is only in context after get_overview is called.
		if ov.RefTokens > 0 {
			ov.Usage = map[string]string{
				"find_callers":    "who calls a symbol — use instead of grep for 'who uses X?'",
				"find_definition": "where a symbol is declared — use for 'where is X defined?'",
				"find_callees":    "what a function invokes — note: generic names (String, New, Error) may have false positives",
				"search":          "find symbols by name pattern, e.g. '%auth%' or 'Parse%' — use instead of grep -r",
				"list_directory":  "browse the tree structure — use instead of ls/find",
				"get_communities": "find clusters of related code (use summary=true for large repos; requires dense cross-references)",
				"get_impact":      "blast radius of changing a symbol — traces callers/callees to a configurable depth",
			}
		}

		// Walk one level deeper to count total dirs/files
		for _, childID := range children {
			node, _ := g.GetNode(childID)
			if node != nil && node.Mode.IsDir() {
				subChildren, _ := g.ListChildren(childID)
				for _, subID := range subChildren {
					subNode, _ := g.GetNode(subID)
					if subNode != nil {
						if subNode.Mode.IsDir() {
							ov.TotalDirs++
						} else {
							ov.TotalFiles++
						}
					}
				}
			}
		}

		data, _ := json.MarshalIndent(ov, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
}
