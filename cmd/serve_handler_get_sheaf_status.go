package cmd

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/agentic-research/mache/internal/leyline"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// makeGetSheafStatusHandler returns the handler for the get_sheaf_status
// MCP tool. The tool surfaces the daemon's sheaf cache state to agents
// so they can decide whether cached results are still fresh.
//
// Design contract: the handler MUST NOT surface daemon unavailability
// as an MCP error. Agents polling this tool would otherwise see
// transport failures whenever the daemon is down or hasn't been
// dialed yet. Instead, return a structured {available: false, reason:
// "..."} response. This matches the documented graceful-degradation
// pattern in the wider sheaf wiring (cmd/serve.go).
//
// DiscoverSocket (not DiscoverOrStart) is the right primitive here:
// a status check should never trigger a daemon auto-spawn (which can
// download the binary, take seconds, etc.).
func makeGetSheafStatusHandler() server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		unavailable := func(reason string) (*mcp.CallToolResult, error) {
			data, _ := json.Marshal(map[string]any{
				"available": false,
				"reason":    reason,
			})
			return mcp.NewToolResultText(string(data)), nil
		}

		sockPath, err := leyline.DiscoverSocket()
		if err != nil {
			return unavailable("no ley-line daemon socket — set LEYLINE_SOCKET or start `leyline daemon`")
		}
		sock, err := leyline.DialSocket(sockPath)
		if err != nil {
			return unavailable(fmt.Sprintf("dial %s: %v", sockPath, err))
		}
		defer func() { _ = sock.Close() }()

		sc := leyline.NewSheafClient(sock)
		s, err := sc.Status()
		if err != nil {
			return unavailable(fmt.Sprintf("sheaf_status: %v", err))
		}

		data, _ := json.Marshal(map[string]any{
			"available":  true,
			"generation": s.Generation,
			"valid":      s.Valid,
			"total":      s.Total,
			"defect":     s.Defect,
		})
		return mcp.NewToolResultText(string(data)), nil
	}
}
