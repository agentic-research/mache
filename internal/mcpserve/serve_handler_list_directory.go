package mcpserve

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/agentic-research/mache/graph"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type nodeEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"`
	Size int64  `json:"size,omitempty"`
}

// containerNames are schema-level dir names that group constructs but are
// never themselves a callable token. Skipping the GetCallers/GetCallees
// virtual-entry check at these paths avoids an O(N) SQL query and a
// tree-sitter parse per directory listing — the dominant cost on large
// monorepos (bead mache-og26).
//
// Adding a new schema container here is safe; the worst case is missing a
// virtual `callers/` or `callees/` entry on a dir that wasn't going to
// have meaningful refs anyway.
var containerNames = map[string]bool{
	"methods":    true,
	"functions":  true,
	"types":      true,
	"interfaces": true,
	"structs":    true,
	"classes":    true,
	"imports":    true,
	"constants":  true,
	"variables":  true,
	"namespaces": true,
	"enums":      true,
	"protocols":  true,
	"objects":    true,
	"traits":     true,
	"exports":    true,
	"fields":     true,
}

func makeListDirHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		path := request.GetString("path", "")
		excludeTests := request.GetBool("exclude_tests", false)

		// Single-RLock snapshot of all children. Replaces the prior
		// pattern of ListChildren + N×GetNode (one lookup per child).
		// On a large dir (300+ children) this is the difference between
		// one map iteration and 300 lock acquisitions.
		stats, err := g.ListChildStats(path)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("list %q: %v", path, err)), nil
		}

		entries := make([]nodeEntry, 0, len(stats))
		for _, s := range stats {
			name := filepath.Base(s.ID)
			if excludeTests && (strings.HasPrefix(name, "Test") || strings.HasPrefix(name, "Benchmark")) {
				continue
			}
			typ := "file"
			if s.IsDir {
				typ = "dir"
			}
			entries = append(entries, nodeEntry{
				Name: name,
				Path: s.ID,
				Type: typ,
				Size: s.ContentSize,
			})
		}

		// Surface virtual entries on construct directories
		// (skip if already materialized in the db)
		if path != "" {
			entries = append(entries, virtualEntries(g, path, entries)...)
		}

		data, _ := json.MarshalIndent(entries, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
}

// virtualEntries are the entries list_directory adds to a construct
// directory's listing: its location, and callers/ and callees/ when it has
// any, each skipped when the db already materialized it among listed.
func virtualEntries(g graph.Graph, path string, listed []nodeEntry) []nodeEntry {
	var virtual []nodeEntry
	seen := make(map[string]bool, len(listed))
	for _, e := range listed {
		seen[e.Name] = true
	}

	// Location: source file coordinates for orientation
	if !seen["location"] {
		if node, err := g.GetNode(path); err == nil {
			if loc := graph.PropString(node, "location"); loc != "" {
				virtual = append(virtual, nodeEntry{
					Name: "location",
					Path: path + "/location",
					Type: "virtual",
					Size: int64(len(loc)),
				})
			}
		}
	}

	// Skip GetCallers / GetCallees probes at schema container
	// levels (methods/, functions/, types/, ...). Their basename
	// is never a callable token, so the SQL/parse cost is wasted
	// every readdir.
	isContainer := containerNames[filepath.Base(path)]

	// The token a construct defines, not its last path segment: on a
	// leyline projection that segment is a tree-sitter kind
	// (function_declaration_0), never the symbol (mache-cb4369).
	if !isContainer && !seen["callers"] {
		if callers, err := g.GetCallers(tokenForNode(g, path)); err == nil && len(callers) > 0 {
			virtual = append(virtual, nodeEntry{
				Name: "callers",
				Path: path + "/callers",
				Type: "virtual",
			})
		}
	}
	if !isContainer && !seen["callees"] {
		if callees, err := g.GetCallees(path); err == nil && len(callees) > 0 {
			virtual = append(virtual, nodeEntry{
				Name: "callees",
				Path: path + "/callees",
				Type: "virtual",
			})
		}
	}
	return virtual
}
