package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func makeGetDiagramHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		layout := request.GetString("layout", "")
		name := request.GetString("name", "")
		excludeTests := request.GetBool("exclude_tests", false)
		compact := request.GetBool("compact", false)

		// Validate layout direction if provided.
		if layout != "" {
			switch strings.ToUpper(layout) {
			case "TD", "LR", "BT", "RL":
				layout = strings.ToUpper(layout)
			default:
				return mcp.NewToolResultError(fmt.Sprintf("invalid layout %q: must be TD, LR, BT, or RL", layout)), nil
			}
		}

		// If a diagram name is provided, look up layout from schema diagrams.
		// The name parameter is resolved via schemaProvider; the layout parameter
		// takes precedence when both are specified.
		if name != "" && layout == "" {
			if sp, ok := g.(schemaProvider); ok {
				if schema := sp.Schema(); schema != nil && schema.Diagrams != nil {
					if def, ok := schema.Diagrams[name]; ok {
						layout = def.Layout
					} else if name != "system" {
						return mcp.NewToolResultError(fmt.Sprintf("diagram %q not defined in schema", name)), nil
					}
				}
			}
		}

		rp, ok := g.(refsMapProvider)
		if !ok {
			return mcp.NewToolResultError("get_diagram requires a graph with cross-reference data"), nil
		}
		refs := rp.RefsMap()
		if len(refs) == 0 {
			return mcp.NewToolResultError(
				"No cross-references indexed. Diagram rendering requires constructs that share symbols. " +
					"Ensure the source was ingested with a schema that captures references. " +
					"Use get_overview to check ref_tokens count.",
			), nil
		}

		if excludeTests {
			refs = graph.FilterTestRefs(refs)
		}

		cr := graph.DetectCommunities(refs, 2)
		if len(cr.Communities) == 0 {
			return mcp.NewToolResultError("No communities detected — not enough cross-references to form clusters."), nil
		}

		q := graph.ComputeQuotient(cr, refs)
		mermaidText := q.MermaidWithOpts(graph.MermaidOpts{
			Layout:  layout,
			Compact: compact,
		})

		return mcp.NewToolResultText(mermaidText), nil
	}
}
