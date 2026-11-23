package mcpserve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"path/filepath"
	"slices"
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

// overviewDir is one top-level directory in get_overview's answer.
type overviewDir struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Children int    `json:"children"`
}

// overviewResult is get_overview's answer.
type overviewResult struct {
	TopLevel   []overviewDir `json:"top_level"`
	TotalDirs  int           `json:"total_dirs"`
	TotalFiles int           `json:"total_files"`
	// TotalsTruncated says the totals are a lower bound: the graph
	// could not count itself and the walk stopped at its cap.
	TotalsTruncated bool `json:"totals_truncated,omitempty"`
	RefTokens       int  `json:"ref_tokens,omitempty"`
	DefTokens       int  `json:"def_tokens,omitempty"`
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

func makeGetOverviewHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ov := overviewResult{}
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
			ov.TopLevel = append(ov.TopLevel, overviewDir{
				Name:     filepath.Base(childID),
				Path:     childID,
				Children: len(subChildren),
			})
		}
		ov.TotalDirs, ov.TotalFiles, ov.TotalsTruncated = overviewCounts(g, children)

		// Count refs/defs if available
		ov.RefTokens = refTokenCount(g)
		if dp, ok := g.(graph.DefsMapper); ok {
			ov.DefTokens = len(dp.DefsMap())
		}

		// Embed tool routing hints when a code graph is indexed (has cross-references).
		// This teaches the LLM which tool to use for each task without bloating the
		// system prompt — guidance is only in context after get_overview is called.
		if ov.RefTokens > 0 {
			ov.Usage = toolRoutingHints()
		}

		data, _ := json.MarshalIndent(ov, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
}

// maxOverviewListings bounds the walk overviewCounts falls back to: the
// number of directories it lists. A directory listing is the unit of cost on
// every backend (one query, or one request to the daemon), and files are
// counted from their parent's listing without being visited, so this bounds
// what a graph that cannot count itself costs get_overview. When the cap was
// 100,000 nodes with a lookup per node, --mount mode took 14s per call on
// mache's own tree (mache-553d34).
const maxOverviewListings = 2000

// overviewCounts is get_overview's directory and file totals. In order:
//
//   - A composite is the sum of its mounts, each counted as below, plus one
//     directory per mount.
//   - A projection with `_tree` counts its whole source tree; there a source
//     file is a directory of syntax nodes, so counting the graph's file nodes
//     reported 0 files (mache-7fed6a).
//   - A graph that can count its own nodes is asked.
//   - Any other graph is walked, breadth-first, for at most
//     maxOverviewListings directory listings. A walk that stops there reports
//     truncated, so a partial count is never presented as a total. The walk
//     used to stop two levels down and report that as the total: 316
//     directories and 0 files out of 164k and 658k (#820).
func overviewCounts(g graph.Graph, children []string) (dirs, files int, truncated bool) {
	if ml, ok := g.(graph.MountLister); ok {
		if mounts := ml.Mounts(); len(mounts) > 0 {
			return mountCounts(mounts)
		}
	}
	if treeFiles, treeDirs, ok := treeSummary(g); ok {
		return treeDirs, len(treeFiles), false
	}
	if c, ok := g.(graph.NodeCounter); ok {
		d, f, err := c.NodeCounts()
		if err == nil {
			return d, f, false
		}
		if !errors.Is(err, graph.ErrNodeCountUnavailable) {
			log.Printf("get_overview: count nodes: %v", err)
		}
	}
	return walkCounts(g, children, maxOverviewListings)
}

// mountCounts is the totals of a composite: each mount's own, plus the
// directory each mount appears as.
func mountCounts(mounts map[string]graph.Graph) (dirs, files int, truncated bool) {
	for _, name := range slices.Sorted(maps.Keys(mounts)) {
		m := mounts[name]
		roots, err := m.ListChildren("")
		if err != nil {
			log.Printf("get_overview: list mount %s: %v", name, err)
			truncated = true
			continue
		}
		d, f, t := overviewCounts(m, roots)
		dirs, files, truncated = dirs+d+1, files+f, truncated || t
	}
	return dirs, files, truncated
}

// walkCounts counts the directories and files under roots breadth-first,
// listing at most maxListings directories; truncated says directories were
// left unlisted. Each directory costs one ListChildStats, which says which
// children are directories; a graph that does not answer it costs one
// ListChildren and a GetNode per child.
func walkCounts(g graph.Graph, roots []string, maxListings int) (dirs, files int, truncated bool) {
	queue := make([]string, 0, len(roots))
	for _, id := range roots {
		node, _ := g.GetNode(id)
		switch {
		case node == nil:
		case node.Mode.IsDir():
			dirs++
			queue = append(queue, id)
		default:
			files++
		}
	}
	for listed := 0; len(queue) > 0; listed++ {
		if listed == maxListings {
			return dirs, files, true
		}
		id := queue[0]
		queue = queue[1:]
		for _, child := range childKinds(g, id) {
			if child.IsDir {
				dirs++
				queue = append(queue, child.ID)
			} else {
				files++
			}
		}
	}
	return dirs, files, false
}

// childKinds is the children of a directory with whether each is one.
func childKinds(g graph.Graph, id string) []graph.NodeStat {
	if stats, err := g.ListChildStats(id); err == nil && len(stats) > 0 {
		return stats
	}
	ids, _ := g.ListChildren(id)
	stats := make([]graph.NodeStat, 0, len(ids))
	for _, child := range ids {
		if node, _ := g.GetNode(child); node != nil {
			stats = append(stats, graph.NodeStat{ID: child, IsDir: node.Mode.IsDir()})
		}
	}
	return stats
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

// toolRoutingHints tells the agent which tool answers which question, once a
// code graph with cross-references is indexed.
func toolRoutingHints() map[string]string {
	return map[string]string{
		"find_callers":    "who calls a symbol — use instead of grep for 'who uses X?'",
		"find_definition": "where a symbol is declared — use for 'where is X defined?'",
		"find_callees":    "what a function invokes — note: generic names (String, New, Error) may have false positives",
		"search":          "find symbols by name pattern, e.g. '%auth%' or 'Parse%' — use instead of grep -r",
		"list_directory":  "browse the tree structure — use instead of ls/find",
		"get_communities": "find clusters of related code (use summary=true for large repos; requires dense cross-references)",
		"get_impact":      "blast radius of changing a symbol — traces callers/callees to a configurable depth",
	}
}
