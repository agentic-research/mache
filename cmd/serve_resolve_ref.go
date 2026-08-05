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
	"github.com/agentic-research/mache/internal/resolve"
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
	Exists     bool              `json:"exists"`                // resolved path exists on disk / the resolver found it
	IsDir      bool              `json:"is_dir"`                // resolved path is a directory
	Files      []resolveRefEntry `json:"files,omitempty"`       // direct children, for orientation
	GraphPath  string            `json:"graph_path,omitempty"`  // mount prefix under which the resolved sub-graph is queryable (list_directory, find_definition, find_callers, ...)
	RemoteHint string            `json:"remote_hint,omitempty"` // non-empty for non-local schemes the resolver can't follow yet
	Error      string            `json:"error,omitempty"`       // human-readable error, when Exists is false despite a recognized locator
}

type resolveRefEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size,omitempty"`
	Lang  string `json:"lang,omitempty"` // detected language for files
}

// resolveMounter is the subset of graph.Graph implementations that can
// mount a resolved sub-graph so other MCP tools (list_directory,
// find_definition, find_callers) can query it — currently only *lazyGraph
// (cmd/serve_registry.go). Handlers that receive some other graph.Graph
// (e.g. a plain fixture in tests) still resolve the token and return its
// flat filesystem metadata; they just skip the graph_path enrichment.
type resolveMounter interface {
	resolverRegistry() *resolve.Registry
	mountResolved(cacheKey string, build func() (graph.Graph, error)) (string, error)
}

// makeResolveRefHandler returns the MCP handler for resolve_ref. It takes a
// scheme:locator token and an optional base_path (the file or directory the
// locator is interpreted relative to) and returns the resolved target.
//
// First milestone of mache-q43l (cross-language ref graph). Supports the
// `mod:` scheme (local relative paths, ./foo or ../bar, resolved against
// base_path) and the `gomod:` scheme (a Go import path, resolved via `go
// list` against the served project's own go.mod). Other schemes return
// RemoteHint so callers know the resolver passed them over rather than
// mis-resolving. On a session graph that supports mounting (mache-be0b9f),
// a successful resolution is also mounted under graph_path so
// list_directory/find_definition/find_callers can query it immediately.
func makeResolveRefHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		mounter, canMount := g.(resolveMounter)

		switch scheme {
		case "mod":
			resp = resolveModScheme(locator, basePath)
			mountModScheme(ctx, mounter, canMount, &resp)
		case "gomod":
			resp = resolveGomodScheme(ctx, mounter, canMount, locator)
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

// mountModScheme mounts a successful mod: resolution's directory so other
// MCP tools can query it, setting resp.GraphPath on success. Split out of
// makeResolveRefHandler's closure so that closure stays a thin dispatcher
// (parse token -> per-scheme resolve -> encode) rather than also carrying
// the mount branch's own conditions and calls inline.
//
// A mount failure (e.g. leyline can't parse this language) doesn't
// invalidate the flat filesystem metadata resolveModScheme already
// gathered — degrade quietly, graph_path just stays empty.
func mountModScheme(ctx context.Context, mounter resolveMounter, canMount bool, resp *resolveRefResponse) {
	if !canMount || resp.Error != "" || !resp.Exists || !resp.IsDir {
		return
	}
	resolved := resp.Resolved
	prefix, err := mounter.mountResolved(resolved, func() (graph.Graph, error) {
		return mounter.resolverRegistry().Resolve(ctx, "mod", resolved)
	})
	if err == nil {
		resp.GraphPath = prefix
	}
}

// resolveGomodScheme handles the `gomod:` scheme: locator is a Go import
// path, resolved via GoModResolver (go list against the served project's
// go.mod). Unlike `mod:`, there's no separate filesystem-metadata pass —
// the only way to inspect a Go import path is through the resolver itself
// — so this scheme requires a mounter; on a graph that can't mount (e.g. a
// direct-call test fixture), it reports that plainly via RemoteHint rather
// than silently no-op-ing.
func resolveGomodScheme(ctx context.Context, mounter resolveMounter, canMount bool, locator string) resolveRefResponse {
	resp := resolveRefResponse{Scheme: "gomod", Locator: locator}
	if !canMount {
		resp.RemoteHint = "gomod: resolution requires a session graph that supports mounting"
		return resp
	}

	cacheKey := "gomod:" + locator
	prefix, err := mounter.mountResolved(cacheKey, func() (graph.Graph, error) {
		return mounter.resolverRegistry().Resolve(ctx, "gomod", locator)
	})
	if err != nil {
		resp.Error = err.Error()
		return resp
	}

	resp.Exists = true
	resp.GraphPath = prefix

	// Populate Files for orientation, same as the mod scheme — best
	// effort: a listing failure doesn't invalidate the successful
	// resolution, graph_path is already usable on its own.
	mg, ok := mounter.(graph.Graph)
	if !ok {
		return resp
	}
	children, err := mg.ListChildren(prefix)
	if err != nil {
		return resp
	}
	resp.IsDir = true
	for _, childID := range children {
		// CompositeGraph.ListChildren returns fully-qualified
		// "resolve/<hash>/name" IDs; Files lists bare names, matching the
		// mod scheme's convention (os.ReadDir's e.Name()).
		name := filepath.Base(childID)
		resp.Files = append(resp.Files, resolveRefEntry{Name: name, Lang: detectLang(name)})
	}
	return resp
}

// resolveModScheme handles the `mod:` scheme. Local-relative locators (./X
// or ../X) resolve against base_path; everything else returns RemoteHint.
func resolveModScheme(locator, basePath string) resolveRefResponse {
	resp := resolveRefResponse{Scheme: "mod", Locator: locator}

	if !resolve.IsLocalRelativeLocator(locator) {
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
