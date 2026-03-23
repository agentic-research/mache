package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/writeback"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func makeWriteFileHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		path := request.GetString("path", "")
		if path == "" {
			return mcp.NewToolResultError("path is required"), nil
		}
		content := request.GetString("content", "")
		if content == "" {
			return mcp.NewToolResultError("content is required"), nil
		}

		node, err := g.GetNode(path)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("not found: %s", path)), nil
		}
		if node.Mode.IsDir() {
			return mcp.NewToolResultError(fmt.Sprintf("%s is a directory — write to a file node like /source", path)), nil
		}
		if node.Origin == nil {
			return mcp.NewToolResultError(fmt.Sprintf("%s has no source origin — only source-code nodes support write-back", path)), nil
		}

		origin := *node.Origin
		newContent := []byte(content)

		// 1. Validate syntax
		if err := writeback.Validate(newContent, origin.FilePath); err != nil {
			type valResult struct {
				Status string `json:"status"`
				Error  string `json:"error"`
				Path   string `json:"path"`
				File   string `json:"file"`
			}
			data, _ := json.MarshalIndent(valResult{
				Status: "validation_error",
				Error:  err.Error(),
				Path:   path,
				File:   origin.FilePath,
			}, "", "  ")
			return mcp.NewToolResultText(string(data)), nil
		}

		// 2. Format (gofumpt for Go, hclwrite for HCL)
		formatted := writeback.FormatBuffer(newContent, origin.FilePath)

		// 3. Splice into source file
		oldLen := origin.EndByte - origin.StartByte
		if err := writeback.Splice(origin, formatted); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("splice failed: %v", err)), nil
		}

		// 4. Surgical node update
		wb := g.(writeBacker)
		newOrigin := &graph.SourceOrigin{
			FilePath:  origin.FilePath,
			StartByte: origin.StartByte,
			EndByte:   origin.StartByte + uint32(len(formatted)),
		}
		delta := int32(len(formatted)) - int32(oldLen)
		if delta != 0 {
			wb.ShiftOrigins(origin.FilePath, origin.EndByte, delta)
		}

		modTime := time.Now()
		if fi, err := os.Stat(origin.FilePath); err == nil {
			modTime = fi.ModTime()
		}
		_ = wb.UpdateNodeContent(path, formatted, newOrigin, modTime)
		g.Invalidate(path)

		type writeResult struct {
			Status     string              `json:"status"`
			Path       string              `json:"path"`
			Origin     *graph.SourceOrigin `json:"origin"`
			Formatted  bool                `json:"formatted"`
			BytesDelta int32               `json:"bytes_delta"`
		}
		data, _ := json.MarshalIndent(writeResult{
			Status:     "ok",
			Path:       path,
			Origin:     newOrigin,
			Formatted:  string(formatted) != content,
			BytesDelta: delta,
		}, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
}
