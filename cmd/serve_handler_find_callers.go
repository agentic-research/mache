package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func makeFindCallersHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		token := request.GetString("token", "")
		if token == "" {
			return mcp.NewToolResultError("token is required"), nil
		}
		kind := request.GetString("kind", "")
		if _, ok := filterDirIDsByKind(nil, kind); !ok {
			return mcp.NewToolResultError(fmt.Sprintf("unknown kind %q — accepted values: %s", kind, strings.Join(supportedKinds(), ", "))), nil
		}

		callers, err := g.GetCallers(token)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("get callers: %v", err)), nil
		}

		paths := make([]string, 0, len(callers))
		for _, c := range callers {
			paths = append(paths, c.ID)
		}

		// Apply the optional kind filter to caller paths. find_callers
		// returns the construct paths of the call SITES, so the kind
		// filter narrows by what KIND of caller is calling — e.g.
		// "show me only methods that call X." Empty kind is a no-op.
		if kind != "" {
			paths, _ = filterDirIDsByKind(paths, kind)
		}

		// Cross-repo annotation: when running on a CompositeGraph,
		// surface each caller's mount of origin. annotateMounts
		// returns nil if there's nothing useful to annotate; the
		// handler then falls through to the legacy []string shape.
		if scoped := annotateMounts(g, paths); scoped != nil {
			type callersResult struct {
				Callers []scopedItem `json:"callers"`
			}
			data, _ := json.MarshalIndent(callersResult{Callers: scoped}, "", "  ")
			return mcp.NewToolResultText(string(data)), nil
		}

		// Supplement with LSP references if available
		if qg, ok := g.(refsQuerier); ok {
			lspRefs, lspErr := queryLSPRefs(qg, token)
			if lspErr == nil && len(lspRefs) > 0 {
				// Apply kind filter to LSP refs too (same NodeID
				// construct-path encoding).
				if kind != "" {
					nodeIDs := make([]string, len(lspRefs))
					for i, r := range lspRefs {
						nodeIDs[i] = r.NodeID
					}
					filteredIDs, _ := filterDirIDsByKind(nodeIDs, kind)
					keep := make(map[string]bool, len(filteredIDs))
					for _, id := range filteredIDs {
						keep[id] = true
					}
					kept := lspRefs[:0]
					for _, r := range lspRefs {
						if keep[r.NodeID] {
							kept = append(kept, r)
						}
					}
					lspRefs = kept
				}
				type callersResult struct {
					Callers []string         `json:"callers"`
					LSPRefs []lspRefLocation `json:"lsp_refs"`
				}
				data, _ := json.MarshalIndent(callersResult{
					Callers: paths,
					LSPRefs: lspRefs,
				}, "", "  ")
				return mcp.NewToolResultText(string(data)), nil
			}
		}

		// No LSP data — return original format for backward compatibility
		if len(paths) == 0 {
			if serveControl != "" {
				return mcp.NewToolResultText("[] — daemon may still be parsing, retry shortly"), nil
			}
			return mcp.NewToolResultText("[]"), nil
		}
		data, _ := json.MarshalIndent(paths, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
}
