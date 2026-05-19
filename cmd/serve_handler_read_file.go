package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/leyline"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// prioritizeAndAdvise sends a fire-and-forget hint to the ley-line daemon
// to prioritize parsing the given files. Returns a user-facing message, or
// empty string if not in daemon mode or the daemon is unreachable.
func prioritizeAndAdvise(files []string) string {
	if serveControl == "" || len(files) == 0 {
		return ""
	}
	sockPath := strings.TrimSuffix(serveControl, ".ctrl") + ".sock"
	sc, err := leyline.DialSocket(sockPath)
	if err != nil {
		return ""
	}
	defer func() { _ = sc.Close() }()
	_ = sc.Prioritize(files) // fire-and-forget
	return "File not yet parsed — prioritized for next daemon pass, retry shortly."
}

const maxReadFileSize = 32 * 1024 * 1024 // 32 MB per file

type fileReadResult struct {
	Content string              `json:"content"`
	Origin  *graph.SourceOrigin `json:"origin,omitempty"`
}

func readOneFileWithOrigin(g graph.Graph, path string) (*fileReadResult, error) {
	// Handle virtual location file
	if filepath.Base(path) == graph.LocationFile {
		parentDir := filepath.Dir(path)
		if parent, err := g.GetNode(parentDir); err == nil && parent.Properties != nil {
			if loc, ok := parent.Properties["location"]; ok && len(loc) > 0 {
				return &fileReadResult{Content: string(loc)}, nil
			}
		}
		return nil, fmt.Errorf("not found: %s", path)
	}

	node, err := g.GetNode(path)
	if err != nil {
		return nil, fmt.Errorf("not found: %s", path)
	}
	if node.Mode.IsDir() {
		return nil, fmt.Errorf("%s is a directory — use list_directory", path)
	}
	size := node.ContentSize()
	if size == 0 {
		return &fileReadResult{Origin: node.Origin}, nil
	}
	if size > maxReadFileSize {
		return nil, fmt.Errorf("%s too large (%d bytes, max %d)", path, size, maxReadFileSize)
	}
	buf := make([]byte, size)
	n, err := g.ReadContent(path, buf, 0)
	if err != nil {
		return nil, fmt.Errorf("read %s: %v", path, err)
	}
	return &fileReadResult{Content: string(buf[:n]), Origin: node.Origin}, nil
}

func makeReadFileHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		path := request.GetString("path", "")
		pathsRaw := request.GetString("paths", "")

		// Batch mode
		if pathsRaw != "" {
			var paths []string
			if err := json.Unmarshal([]byte(pathsRaw), &paths); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("invalid paths array: %v", err)), nil
			}
			const maxBatchPaths = 100
			if len(paths) > maxBatchPaths {
				return mcp.NewToolResultError(fmt.Sprintf("batch read limited to %d paths, got %d", maxBatchPaths, len(paths))), nil
			}

			type fileResult struct {
				Path    string              `json:"path"`
				Content string              `json:"content,omitempty"`
				Origin  *graph.SourceOrigin `json:"origin,omitempty"`
				Error   string              `json:"error,omitempty"`
			}
			const maxBatchBytes int64 = maxReadFileSize // total content cap for batch
			results := make([]fileResult, 0, len(paths))
			var totalBytes int64
			for i, p := range paths {
				// Pre-check size to avoid allocating before rejecting.
				if node, err := g.GetNode(p); err == nil && !node.Mode.IsDir() {
					totalBytes += node.ContentSize()
					if totalBytes > maxBatchBytes {
						results = append(results, fileResult{Path: p, Error: fmt.Sprintf("batch too large (exceeds %d bytes total)", maxBatchBytes)})
						for _, remaining := range paths[i+1:] {
							results = append(results, fileResult{Path: remaining, Error: "skipped: batch size limit reached"})
						}
						break
					}
				}
				r, err := readOneFileWithOrigin(g, p)
				if err != nil {
					results = append(results, fileResult{Path: p, Error: err.Error()})
					continue
				}
				results = append(results, fileResult{Path: p, Content: r.Content, Origin: r.Origin})
			}
			data, _ := json.MarshalIndent(results, "", "  ")
			return mcp.NewToolResultText(string(data)), nil
		}

		// Single mode
		if path == "" {
			return mcp.NewToolResultError("path or paths is required"), nil
		}
		r, err := readOneFileWithOrigin(g, path)
		if err != nil {
			// In daemon mode, the file may not be parsed yet — ask daemon to prioritize it.
			if msg := prioritizeAndAdvise([]string{path}); msg != "" {
				return mcp.NewToolResultText(msg), nil
			}
			return mcp.NewToolResultError(err.Error()), nil
		}
		// If there's an origin, return it as structured JSON so the consumer
		// knows exactly where to edit in the real filesystem.
		if r.Origin != nil {
			data, _ := json.MarshalIndent(r, "", "  ")
			return mcp.NewToolResultText(string(data)), nil
		}
		return mcp.NewToolResultText(r.Content), nil
	}
}
