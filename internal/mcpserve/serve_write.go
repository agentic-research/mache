package mcpserve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/writeback"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func makeWriteFileHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		path := request.GetString("path", "")
		if path == "" {
			return mcp.NewToolResultError("path is required"), nil
		}
		content := request.GetString("content", "")
		if content == "" {
			return mcp.NewToolResultError("content is required"), nil
		}
		// Default true preserves the prior behavior (gofumpt for Go,
		// hclwrite for HCL). Bead mache-2a7605: callers that own
		// formatting upstream (pre-commit, the LLM itself) can pass
		// format=false so mache only validates structure and splices
		// the content verbatim. Validation is non-negotiable; formatting
		// is now opt-out.
		format := request.GetBool("format", true)

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

		// Fail fast on unsupported backends BEFORE splice runs. Otherwise
		// the unchecked type assertion below panics after the source file
		// has already been written, leaving disk and graph out of sync.
		wb, ok := g.(writeBacker)
		if !ok {
			return mcp.NewToolResultError("backend does not support write-back"), nil
		}

		origin := *node.Origin
		newContent := []byte(content)

		// 1. Validate syntax — always runs. Type/structure diagnostics are
		// the mache write path's contract; we never let invalid bytes
		// through to the splice.
		if err := writeback.Validate(newContent, origin.FilePath); err != nil {
			return validationFailure(err, path, origin.FilePath), nil
		}

		// 2. Format (gofumpt for Go, hclwrite for HCL) — opt-out.
		formatted := newContent
		if format {
			formatted = writeback.FormatBuffer(newContent, origin.FilePath)
		}

		// 3. Propose the node's file with the edit, against the blob the
		// node was read from; the daemon writes the file (mache-3a0fb9).
		a, err := writeback.Propose(origin, formatted, wb.Blobs())
		var refused *writeback.RefusedError
		if errors.As(err, &refused) {
			return refusalResult(path, origin.FilePath, refused), nil
		}
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("write refused, source untouched: %v", err)), nil
		}

		return committed(g, wb, path, origin, a, format, string(formatted) != content), nil
	}
}

// validationFailure is a write that did not validate. Syntax failures ("your
// code is broken") and validator-infra failures (daemon unreachable / too
// old) are different facts; the status must not claim validation_error for
// the latter (#527 review).
func validationFailure(err error, path, file string) *mcp.CallToolResult {
	status := "validation_error"
	var ve *writeback.ValidationError
	if !errors.As(err, &ve) {
		status = "validator_unavailable"
	}
	data, _ := json.MarshalIndent(struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Path   string `json:"path"`
		File   string `json:"file"`
	}{status, err.Error(), path, file}, "", "  ")
	return mcp.NewToolResultText(string(data))
}

// committed records an accepted write in the graph and reports it. The write
// is applied; if the graph update fails, "ok_graph_stale" says the file is
// right and the graph may not be. A merged write is not what mache built.
func committed(g graph.Graph, wb writeBacker, path string, origin graph.SourceOrigin, a *writeback.Applied, format, formatChanged bool) *mcp.CallToolResult {
	modTime := time.Now()
	if fi, err := os.Stat(origin.FilePath); err == nil {
		modTime = fi.ModTime()
	}
	status := "ok"
	var graphWarning string
	switch err := writeback.Commit(wb, path, origin, a, modTime); {
	case err != nil:
		status = "ok_graph_stale"
		graphWarning = fmt.Sprintf("graph update failed after the write: %v (the file is correct; re-read to refresh the graph)", err)
	case a.Merged:
		status = "ok_merged"
		graphWarning = "merged with a change made since this was read; re-read the file before writing it again"
	}
	g.Invalidate(path)

	data, _ := json.MarshalIndent(struct {
		Status        string              `json:"status"`
		Path          string              `json:"path"`
		Origin        *graph.SourceOrigin `json:"origin"`
		FormatApplied bool                `json:"format_applied"`           // formatter ran (format=true)
		FormatChanged bool                `json:"format_changed,omitempty"` // formatter actually altered the bytes
		BytesDelta    int32               `json:"bytes_delta"`
		GraphWarning  string              `json:"graph_warning,omitempty"`
	}{
		Status: status,
		Path:   path,
		Origin: &graph.SourceOrigin{
			FilePath:  origin.FilePath,
			StartByte: origin.StartByte,
			EndByte:   origin.StartByte + uint32(len(a.Content)),
		},
		FormatApplied: format,
		FormatChanged: format && formatChanged,
		BytesDelta:    int32(len(a.Content)) - int32(origin.EndByte-origin.StartByte),
		GraphWarning:  graphWarning,
	}, "", "  ")
	return mcp.NewToolResultText(string(data))
}

// refusalResult is a refused write as the tool reports it: the daemon's
// reason, its sentence, and for a stale write the lines (1-based) where the
// edit overlapped a change made since the node was read. A refusal is an
// answer, not a tool failure.
func refusalResult(path, file string, r *writeback.RefusedError) *mcp.CallToolResult {
	type conflict struct {
		File      string `json:"file"`
		StartLine uint32 `json:"start_line"`
		EndLine   uint32 `json:"end_line"`
	}
	var conflicts []conflict
	for _, m := range r.Refusal.Moved {
		for _, c := range m.Conflicts {
			conflicts = append(conflicts, conflict{File: m.Path, StartLine: c.Start + 1, EndLine: c.End})
		}
	}
	data, _ := json.MarshalIndent(struct {
		Status    string     `json:"status"`
		Reason    string     `json:"reason"`
		Error     string     `json:"error"`
		Path      string     `json:"path"`
		File      string     `json:"file"`
		Conflicts []conflict `json:"conflicts,omitempty"`
	}{"write_refused", string(r.Refusal.Reason), r.Message, path, file, conflicts}, "", "  ")
	return mcp.NewToolResultText(string(data))
}
