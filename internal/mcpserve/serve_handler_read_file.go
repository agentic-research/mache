package mcpserve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/leyline"
	"github.com/agentic-research/mache/internal/lloschema"
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
		if parent, err := g.GetNode(parentDir); err == nil {
			if loc := graph.PropString(parent, "location"); loc != "" {
				return &fileReadResult{Content: loc}, nil
			}
		}
		return nil, fmt.Errorf("not found: %s", path)
	}

	node, err := g.GetNode(path)
	if err != nil {
		if r, ok, terr := readTreeFile(g, path); ok || terr != nil {
			return r, terr
		}
		return readVirtual(g, path)
	}
	if node.Mode.IsDir() {
		return nil, fmt.Errorf("%s is a directory — use list_directory", path)
	}
	content, err := readNodeContent(g, path, node)
	if err != nil {
		return nil, err
	}
	return &fileReadResult{Content: content, Origin: node.Origin}, nil
}

func makeReadFileHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		path := request.GetString("path", "")
		pathsRaw := request.GetString("paths", "")
		mode := request.GetString("mode", readModeFull)

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
		// Projected read modes (mache-qzsk): signatures/map answer orientation
		// questions in ONE round trip with no positioning, which is the whole
		// point — see cmd/serve_read_modes.go for the measured rationale.
		if out, ok, perr := readProjected(g, path, mode); perr != nil {
			return mcp.NewToolResultError(perr.Error()), nil
		} else if ok {
			return mcp.NewToolResultText(out), nil
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

// readTreeFile reads a file leyline saw but no parser projected: it is in
// `_tree` only, and its bytes in source_blobs (mache-3a375a). ok is false when
// path is no such file, so the caller goes on resolving it. A file that is not
// text has nothing to read.
func readTreeFile(g graph.Graph, path string) (*fileReadResult, bool, error) {
	qg, isQ := g.(lloschema.Querier)
	if !isQ {
		return nil, false, nil
	}
	if has, err := lloschema.HasTree(qg); err != nil || !has {
		return nil, false, nil
	}
	content, ok, err := lloschema.TreeFileBytes(qg, path)
	if err != nil || !ok {
		return nil, false, err
	}
	// A NUL byte is binary (git's test); so is anything that is not UTF-8.
	if bytes.IndexByte(content, 0) >= 0 || !utf8.Valid(content) {
		return nil, true, fmt.Errorf("%s is a binary file (%d bytes) no parser projected; it has no text to read", path, len(content))
	}
	return &fileReadResult{Content: string(content)}, true, nil
}
