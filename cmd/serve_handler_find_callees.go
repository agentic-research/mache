package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/agentic-research/mache/graph"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// genericGoNames is the set of method/function names that commonly cause
// false positives in bare-token callee resolution. When a callee's base name
// matches one of these, the result is flagged as potentially noisy.
var genericGoNames = map[string]bool{
	"String": true, "Error": true, "New": true, "Parse": true,
	"Close": true, "Read": true, "Write": true, "Open": true,
	"Run": true, "Start": true, "Stop": true, "Reset": true,
	"Marshal": true, "Unmarshal": true, "Encode": true, "Decode": true,
	"Format": true, "Scan": true, "Next": true, "Done": true,
}

func makeFindCalleesHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		path := request.GetString("path", "")
		if path == "" {
			return mcp.NewToolResultError("path is required"), nil
		}

		callees, err := g.GetCallees(path)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("get callees: %v", err)), nil
		}
		if len(callees) == 0 {
			// Provide a helpful hint about why callees might be empty
			node, nodeErr := g.GetNode(path)
			if nodeErr != nil {
				return mcp.NewToolResultText(`{"callees":[],"hint":"construct not found — check the path"}`), nil
			}
			if !node.Mode.IsDir() {
				return mcp.NewToolResultText(`{"callees":[],"hint":"path is a file, not a construct directory — use the parent directory path"}`), nil
			}
			hint := "no resolved callees"
			if graph.PropString(node, "lang") != "" {
				hint = "no resolved callees — the construct may call unexported methods or use dynamic dispatch that the static extractor cannot resolve. Try find_callers with the method name instead."
			}
			type emptyResult struct {
				Callees []string `json:"callees"`
				Hint    string   `json:"hint"`
			}
			data, _ := json.MarshalIndent(emptyResult{Callees: []string{}, Hint: hint}, "", "  ")
			return mcp.NewToolResultText(string(data)), nil
		}

		calleePaths := make([]string, 0, len(callees))
		var warnings []string
		seenGeneric := make(map[string]bool)
		for _, c := range callees {
			calleePaths = append(calleePaths, c.ID)
			// Check if this callee's base name is a common generic identifier.
			// Generic names resolved via bare-token fallback may include false
			// positives from unrelated packages.
			name := filepath.Base(c.ID)
			if genericGoNames[name] && !seenGeneric[name] {
				seenGeneric[name] = true
				warnings = append(warnings,
					fmt.Sprintf("'%s' is a common name — results may include false positives from unrelated packages. Use find_callers on the specific implementation path to verify.", name),
				)
			}
		}

		// Cross-repo annotation — same pattern as find_callers.
		if scoped := annotateMounts(g, calleePaths); scoped != nil {
			type calleesResult struct {
				Callees  []scopedItem `json:"callees"`
				Warnings []string     `json:"warnings,omitempty"`
			}
			out := calleesResult{Callees: scoped, Warnings: warnings}
			data, _ := json.MarshalIndent(out, "", "  ")
			return mcp.NewToolResultText(string(data)), nil
		}

		type calleesResult struct {
			Callees  []string `json:"callees"`
			Warnings []string `json:"warnings,omitempty"`
		}
		out := calleesResult{Callees: calleePaths, Warnings: warnings}
		data, _ := json.MarshalIndent(out, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
}
