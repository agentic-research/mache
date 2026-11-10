package mcpserve

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"github.com/agentic-research/mache/graph"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// overviewIndexReport derives the staleness block for graphs that can answer
// (nil for the rest — omission means unknown, never fresh) plus a
// human-readable warning when drift is nonzero. A capped count renders as a
// floor ("500+"), never as an exact number.
func overviewIndexReport(g graph.Graph) (*graph.IndexStaleness, string) {
	sr, ok := g.(graph.StalenessReporter)
	if !ok {
		return nil, ""
	}
	rep, ok := sr.IndexStaleness()
	if !ok {
		return nil, ""
	}
	if rep.ModifiedSince == 0 && rep.DeletedSince == 0 {
		return &rep, ""
	}
	plus := ""
	if rep.Capped {
		plus = "+"
	}
	var drift []string
	if rep.ModifiedSince > 0 {
		drift = append(drift, fmt.Sprintf("%d%s source file(s) modified", rep.ModifiedSince, plus))
	}
	if rep.DeletedSince > 0 {
		// Deleted files are the worse half of drift: the index still SERVES
		// their nodes, so search/find_definition return results for code that
		// no longer exists — not merely miss new code.
		drift = append(drift, fmt.Sprintf("%d%s indexed file(s) deleted or renamed", rep.DeletedSince, plus))
	}
	return &rep, fmt.Sprintf(
		"index built %s; %s since — answers may not reflect current code (restart the session or rebuild to refresh)",
		rep.BuiltAt.Format("2006-01-02 15:04:05"), strings.Join(drift, " and "))
}

func makeGetOverviewHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		type dirInfo struct {
			Name     string `json:"name"`
			Path     string `json:"path"`
			Children int    `json:"children"`
		}
		type overview struct {
			TopLevel   []dirInfo `json:"top_level"`
			TotalDirs  int       `json:"total_dirs"`
			TotalFiles int       `json:"total_files"`
			RefTokens  int       `json:"ref_tokens,omitempty"`
			DefTokens  int       `json:"def_tokens,omitempty"`
			// Index is the staleness report, present when the graph can
			// answer. The default serve path serves a FROZEN snapshot — edits
			// after session start are invisible to every tool — and until the
			// live-reparse path exists (mache-6c9e1d), the floor is saying so
			// where agents START: this tool's own description is "START HERE".
			// Omission means unknown, never fresh.
			Index        *graph.IndexStaleness `json:"index,omitempty"`
			IndexWarning string                `json:"index_warning,omitempty"`
			Usage        map[string]string     `json:"_usage,omitempty"`
		}

		ov := overview{}
		ov.Index, ov.IndexWarning = overviewIndexReport(g)

		// Top-level structure
		children, err := g.ListChildren("")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("list root: %v", err)), nil
		}

		for _, childID := range children {
			node, err := g.GetNode(childID)
			if err != nil || !node.Mode.IsDir() {
				continue
			}
			subChildren, _ := g.ListChildren(childID)
			ov.TopLevel = append(ov.TopLevel, dirInfo{
				Name:     filepath.Base(childID),
				Path:     childID,
				Children: len(subChildren),
			})
		}
		ov.TotalDirs, ov.TotalFiles = overviewCounts(g, children)

		// Count refs/defs if available
		ov.RefTokens = refTokenCount(g)
		if dp, ok := g.(graph.DefsMapper); ok {
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

		data, _ := json.MarshalIndent(ov, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
}

// overviewCounts is get_overview's directory and file totals. A projection
// with `_tree` counts its whole source tree; there a source file is a
// directory of syntax nodes, so counting the graph's file nodes two levels
// down reported 0 files (mache-7fed6a). Other graphs count the graph's own
// nodes in the top two levels.
func overviewCounts(g graph.Graph, children []string) (dirs, files int) {
	if treeFiles, treeDirs, ok := treeSummary(g); ok {
		return treeDirs, len(treeFiles)
	}
	for _, childID := range children {
		node, _ := g.GetNode(childID)
		if node == nil {
			continue
		}
		if !node.Mode.IsDir() {
			files++
			continue
		}
		dirs++
		subChildren, _ := g.ListChildren(childID)
		for _, subID := range subChildren {
			if subNode, _ := g.GetNode(subID); subNode != nil {
				if subNode.Mode.IsDir() {
					dirs++
				} else {
					files++
				}
			}
		}
	}
	return dirs, files
}

// refTokenCount is how many tokens the graph's references name: counted by the
// graph when it can, else the length of its refs map, and 0 when it has
// neither. A failed count is logged and reported as 0, as RefsMap reports a
// failed read as an empty map.
func refTokenCount(g graph.Graph) int {
	if c, ok := g.(graph.RefTokenCounter); ok {
		n, err := c.RefTokenCount()
		if err != nil {
			log.Printf("get_overview: count reference tokens: %v", err)
		}
		return n
	}
	if rp, ok := g.(graph.RefsMapper); ok {
		return len(rp.RefsMap())
	}
	return 0
}
