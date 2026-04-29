package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/lang"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// resolveRefResponse is the shape returned by the resolve_ref MCP tool.
// It covers both the local-path case (Files populated, Resolved set) and
// the remote case (RemoteHint set, Files empty) so callers can choose to
// follow up with a clone or skip.
type resolveRefResponse struct {
	Scheme     string            `json:"scheme"`
	Locator    string            `json:"locator"`
	Resolved   string            `json:"resolved,omitempty"`    // absolute filesystem path (local locators only)
	Exists     bool              `json:"exists"`                // resolved path exists on disk
	IsDir      bool              `json:"is_dir"`                // resolved path is a directory
	Files      []resolveRefEntry `json:"files,omitempty"`       // direct children when Resolved is a dir
	RemoteHint string            `json:"remote_hint,omitempty"` // non-empty for non-local schemes the resolver can't follow yet
	Error      string            `json:"error,omitempty"`       // human-readable error, when Exists is false despite a recognized locator
}

type resolveRefEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size,omitempty"`
	Lang  string `json:"lang,omitempty"` // detected language for files
}

// makeResolveRefHandler returns the MCP handler for resolve_ref. It takes a
// scheme:locator token and an optional base_path (the file or directory the
// locator is interpreted relative to) and returns the resolved target.
//
// First milestone of mache-q43l (cross-language ref graph). Currently
// supports the `mod:` scheme with local relative paths (./foo, ../bar).
// Other schemes return RemoteHint so callers know the resolver passed them
// over rather than mis-resolving.
func makeResolveRefHandler(_ graph.Graph) server.ToolHandlerFunc {
	return func(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		token := strings.TrimSpace(request.GetString("token", ""))
		basePath := strings.TrimSpace(request.GetString("base_path", ""))
		if token == "" {
			return mcp.NewToolResultError("token is required (e.g. \"mod:./modules/vpc\")"), nil
		}

		scheme, locator, ok := strings.Cut(token, ":")
		if !ok || scheme == "" || locator == "" {
			return mcp.NewToolResultError(fmt.Sprintf("token must be scheme:locator, got %q", token)), nil
		}

		resp := resolveRefResponse{Scheme: scheme, Locator: locator}

		switch scheme {
		case "mod":
			resp = resolveModScheme(locator, basePath)
		default:
			resp.RemoteHint = fmt.Sprintf("scheme %q not yet supported by resolve_ref", scheme)
		}

		body, err := json.MarshalIndent(resp, "", "  ")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("encode response: %v", err)), nil
		}
		return mcp.NewToolResultText(string(body)), nil
	}
}

// resolveModScheme handles the `mod:` scheme. Local-relative locators (./X
// or ../X) resolve against base_path; everything else returns RemoteHint.
func resolveModScheme(locator, basePath string) resolveRefResponse {
	resp := resolveRefResponse{Scheme: "mod", Locator: locator}

	if !isLocalRelativeLocator(locator) {
		resp.RemoteHint = fmt.Sprintf("locator %q is not a local relative path; remote resolution not yet implemented", locator)
		return resp
	}

	if basePath == "" {
		resp.Error = "local-relative locators require base_path (the file or directory the locator is relative to)"
		return resp
	}

	baseDir := basePath
	if info, err := os.Stat(basePath); err == nil && !info.IsDir() {
		baseDir = filepath.Dir(basePath)
	}

	resolved, err := filepath.Abs(filepath.Join(baseDir, locator))
	if err != nil {
		resp.Error = fmt.Sprintf("resolve %q: %v", locator, err)
		return resp
	}
	resp.Resolved = resolved

	info, err := os.Stat(resolved)
	if err != nil {
		resp.Error = fmt.Sprintf("stat %s: %v", resolved, err)
		return resp
	}
	resp.Exists = true
	resp.IsDir = info.IsDir()

	if !info.IsDir() {
		return resp
	}

	entries, err := os.ReadDir(resolved)
	if err != nil {
		resp.Error = fmt.Sprintf("read dir %s: %v", resolved, err)
		return resp
	}
	for _, e := range entries {
		entry := resolveRefEntry{Name: e.Name(), IsDir: e.IsDir()}
		if !e.IsDir() {
			if fi, err := e.Info(); err == nil {
				entry.Size = fi.Size()
			}
			entry.Lang = detectLang(e.Name())
		}
		resp.Files = append(resp.Files, entry)
	}
	return resp
}

// isLocalRelativeLocator reports whether the locator should be resolved
// against the local filesystem. Conservative: only ./ and ../ prefixes.
// Bare paths like "foo/bar" are ambiguous (could be a registry slug); we
// treat them as remote until a registry resolver is added.
func isLocalRelativeLocator(locator string) bool {
	return strings.HasPrefix(locator, "./") || strings.HasPrefix(locator, "../")
}

// detectLang returns a coarse language label for a filename based on its
// extension, leaning on the same registry mache uses elsewhere. Empty
// string when the extension isn't a recognized source language.
func detectLang(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if ext == "" {
		return ""
	}
	if l := lang.ForExt(ext); l != nil {
		return l.Name
	}
	return ""
}
