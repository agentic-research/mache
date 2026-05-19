package cmd

import (
	"context"
	"encoding/json"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/leyline"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func makeSemanticSearchHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query := request.GetString("query", "")
		if query == "" {
			return mcp.NewToolResultError("query is required"), nil
		}
		k := request.GetInt("k", 10)

		sockPath, err := leyline.DiscoverOrStart()
		if err != nil {
			return mcp.NewToolResultError(
				"semantic search not available — requires ley-line daemon with embeddings.\n" +
					"This is an optional feature. Use 'search' for pattern-based code search instead.",
			), nil
		}

		sock, err := leyline.DialSocket(sockPath)
		if err != nil { // coverage:ignore — DiscoverOrStart already proves the socket dials via its internal isSocketAlive liveness probe (see internal/leyline/socket.go L132-139); the only way to reach here is a daemon that SIGKILL'd between the liveness check and this dial — a sub-millisecond race the test fixtures can't deterministically construct. The branch exists for defensive resilience.
			return mcp.NewToolResultError( // coverage:ignore
				"semantic search not available — ley-line daemon not responding.\n" + // coverage:ignore
					"This is an optional feature. Use 'search' for pattern-based code search instead.", // coverage:ignore
			), nil // coverage:ignore
		} // coverage:ignore — closing brace of the unreachable dial-fail block above
		defer func() { _ = sock.Close() }()

		sc := leyline.NewSemanticClient(sock)
		results, err := sc.Search(query, k)
		if err != nil {
			return mcp.NewToolResultError(
				"semantic search not available — ley-line daemon does not support embeddings.\n" +
					"This is an optional feature. Use 'search' for pattern-based code search instead.",
			), nil
		}

		if len(results) == 0 {
			return mcp.NewToolResultText("[]"), nil
		}

		type enrichedResult struct {
			Path     string  `json:"path"`
			Distance float64 `json:"distance"`
			Type     string  `json:"type,omitempty"`
			Snippet  string  `json:"snippet,omitempty"`
		}

		enriched := make([]enrichedResult, 0, len(results))
		for _, r := range results {
			er := enrichedResult{
				Path:     r.ID,
				Distance: r.Distance,
			}

			// Enrich with graph metadata
			node, nodeErr := g.GetNode(r.ID)
			if nodeErr == nil && node != nil {
				if node.Mode.IsDir() {
					er.Type = "directory"
				} else {
					er.Type = "file"
					// Read a content snippet (first 200 bytes)
					buf := make([]byte, 200)
					n, _ := g.ReadContent(r.ID, buf, 0)
					if n > 0 {
						snippet := string(buf[:n])
						if n == 200 {
							snippet += "..."
						}
						er.Snippet = snippet
					}
				}
			}

			enriched = append(enriched, er)
		}

		data, _ := json.MarshalIndent(enriched, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
}
