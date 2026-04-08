package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/leyline"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// registerMCPTools registers all tool definitions with session-aware handlers.
// All tools are registered unconditionally — lazyGraph delegates to the inner
// graph and returns errors for unsupported operations at call time. This lets
// different sessions (with different graph backends) coexist on one server.
func registerMCPTools(s *server.MCPServer, r *graphRegistry) {
	s.AddTool(
		mcp.NewTool("list_directory",
			mcp.WithDescription("Browse the projected tree. Use instead of ls/find for 'what's in directory X?', 'list packages under Y'. Use empty path for root."),
			mcp.WithString("path", mcp.Description("Directory path (empty for root)")),
			mcp.WithBoolean("exclude_tests", mcp.Description("Exclude Test* and Benchmark* entries (default false). Recommended for large packages.")),
		),
		r.wrapHandler(makeListDirHandler),
	)

	s.AddTool(
		mcp.NewTool("read_file",
			mcp.WithDescription("Read the text content of one or more file nodes. Pass a single path or a JSON array of paths for batch reads."),
			mcp.WithString("path", mcp.Description("Single file node path")),
			mcp.WithString("paths", mcp.Description("JSON array of file node paths for batch read, e.g. [\"path/a\", \"path/b\"]")),
		),
		r.wrapHandler(makeReadFileHandler),
	)

	s.AddTool(
		mcp.NewTool("find_callers",
			mcp.WithDescription("Find all constructs that reference a symbol. Use for 'who calls X?', 'where is X used?', 'find usages of X'. More accurate than grep for symbol lookup."),
			mcp.WithString("token", mcp.Required(), mcp.Description("Symbol or token name (e.g. 'GetCallers', 'ParseVuln')")),
		),
		r.wrapHandler(makeFindCallersHandler),
	)

	s.AddTool(
		mcp.NewTool("find_callees",
			mcp.WithDescription("Find what a function/method calls. Use for 'what does X invoke?', 'dependencies of X'. Note: generic names (String, New, Error) may have false positives — prefer qualified calls."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Construct directory path (e.g. 'go/graph/methods/GetCallees')")),
		),
		r.wrapHandler(makeFindCalleesHandler),
	)

	s.AddTool(
		mcp.NewTool("search",
			mcp.WithDescription("Search for symbols by name pattern. Use instead of grep -r for 'find functions named X', 'find all X*', 'search for *auth*'. SQL LIKE wildcards: % = any chars. role=definition finds declarations."),
			mcp.WithString("pattern", mcp.Required(), mcp.Description("Search pattern, e.g. 'Login%' or '%auth%'")),
			mcp.WithString("type", mcp.Description("Filter by construct type in path, e.g. 'functions', 'methods', 'types', 'structs'")),
			mcp.WithString("role", mcp.Description("Filter by role: 'definition' (where symbol is declared), 'reference' (where symbol is used). Default: returns references.")),
			mcp.WithNumber("limit", mcp.Description("Max results (default 100)")),
		),
		r.wrapHandler(makeSearchHandler),
	)

	s.AddTool(
		mcp.NewTool("semantic_search",
			mcp.WithDescription("Find code by meaning using embedding similarity. Use for 'find code that does X', 'functions related to authentication', 'error handling patterns'. More flexible than pattern search — finds conceptually similar code even without exact name matches. Requires ley-line daemon with --embed flag."),
			mcp.WithString("query", mcp.Required(), mcp.Description("Natural language description of what you're looking for")),
			mcp.WithNumber("k", mcp.Description("Max results (default 10)")),
		),
		r.wrapHandler(makeSemanticSearchHandler),
	)

	s.AddTool(
		mcp.NewTool("get_communities",
			mcp.WithDescription("Find clusters of related code using Louvain modularity detection. Use for 'what code works together?', 'find subsystems'. Requires dense cross-references. Use summary=true for large codebases."),
			mcp.WithNumber("min_size", mcp.Description("Minimum community size (default 2)")),
			mcp.WithBoolean("summary", mcp.Description("Return summary only (ID, size, top 5 members per community) instead of full member lists. Recommended for large codebases.")),
		),
		r.wrapHandler(makeGetCommunitiesHandler),
	)

	s.AddTool(
		mcp.NewTool("find_definition",
			mcp.WithDescription("Find where a symbol is declared. Use for 'where is X defined?', 'where does X come from?'. Complements find_callers (who uses it) and find_callees (what it calls)."),
			mcp.WithString("symbol", mcp.Required(), mcp.Description("Symbol name to find definition for (e.g. 'GetCallers' or 'auth.Validate')")),
		),
		r.wrapHandler(makeFindDefinitionHandler),
	)

	s.AddTool(
		mcp.NewTool("get_type_info",
			mcp.WithDescription("Get type signature and documentation for a symbol from LSP hover data. Returns the language server's type information (e.g. function signatures, struct definitions). If LSP data is missing and 'file' is provided, auto-enriches via ley-line daemon."),
			mcp.WithString("symbol", mcp.Required(), mcp.Description("Symbol name to look up (e.g. 'NewEncoder', 'Model')")),
			mcp.WithString("file", mcp.Description("Source file path — triggers automatic LSP enrichment if _lsp_hover table is missing")),
		),
		r.wrapHandler(makeGetTypeInfoHandler),
	)

	s.AddTool(
		mcp.NewTool("get_diagnostics",
			mcp.WithDescription("Get LSP diagnostics (errors, warnings) for symbols. Returns diagnostics from the language server. If LSP data is missing and 'file' is provided, auto-enriches via ley-line daemon."),
			mcp.WithString("symbol", mcp.Description("Symbol name to filter by (optional, returns all if empty)")),
			mcp.WithNumber("limit", mcp.Description("Max results (default 50)")),
			mcp.WithString("file", mcp.Description("Source file path — triggers automatic LSP enrichment if _lsp table is missing")),
		),
		r.wrapHandler(makeGetDiagnosticsHandler),
	)

	s.AddTool(
		mcp.NewTool("get_overview",
			mcp.WithDescription("START HERE when exploring a codebase. Returns top-level structure, node counts, cross-reference stats, and a usage guide for all tools."),
		),
		r.wrapHandler(makeGetOverviewHandler),
	)

	s.AddTool(
		mcp.NewTool("get_impact",
			mcp.WithDescription("Change impact analysis: given a symbol, trace through the refs graph to show affected callers and/or callees (multi-hop BFS traversal). Use for 'what would be affected if I change X?', 'blast radius of modifying Y'."),
			mcp.WithString("symbol", mcp.Required(), mcp.Description("Symbol name to analyze (e.g. 'GetCallers', 'auth.Validate')")),
			mcp.WithNumber("depth", mcp.Description("Max traversal depth (default 2)")),
			mcp.WithString("direction", mcp.Description("Traversal direction: 'callers' (who calls this), 'callees' (what this calls), 'both' (default 'both')")),
		),
		r.wrapHandler(makeGetImpactHandler),
	)

	s.AddTool(
		mcp.NewTool("get_architecture",
			mcp.WithDescription("Structured architectural analysis of the codebase. Returns entry points (high fan-in), key abstractions (most defs), dependency layers (community-based), test files, API surface (exported symbols), file count, and language breakdown. Use after get_overview for deeper orientation."),
		),
		r.wrapHandler(makeGetArchitectureHandler),
	)

	s.AddTool(
		mcp.NewTool("get_diagram",
			mcp.WithDescription("Render a mermaid diagram of the projected system's structure. Uses community detection to group related code, then renders the quotient graph (classes + cross-class edges) as mermaid syntax. Edge labels show the most significant boundary tokens (above-mean weight)."),
			mcp.WithString("name", mcp.Description("Diagram name from schema (default: full system view)")),
			mcp.WithString("layout", mcp.Description("Layout direction: TD (top-down), LR (left-right), BT (bottom-top), RL (right-left). Default: TD")),
			mcp.WithBoolean("exclude_tests", mcp.Description("Exclude test files (*_test.go, Test*, Benchmark*) from community detection. Produces cleaner domain-focused labels.")),
			mcp.WithBoolean("compact", mcp.Description("Compact mode: render classes as labeled nodes with member count instead of subgraphs with full member listings. Better for large codebases.")),
		),
		r.wrapHandler(makeGetDiagramHandler),
	)

	s.AddTool(
		mcp.NewTool("write_file",
			mcp.WithDescription("Write new content to a source file node. Uses the splice pipeline: validate (tree-sitter) → format (gofumpt/hclwrite) → atomic splice into source file → update graph. The node must have a source origin (i.e., was ingested from a real file). Returns the result including any validation errors."),
			mcp.WithString("path", mcp.Required(), mcp.Description("File node path (e.g. 'go/graph/methods/MemoryStore.GetCallees/source')")),
			mcp.WithString("content", mcp.Required(), mcp.Description("New content to write")),
		),
		r.wrapHandler(makeWriteFileHandler),
	)
}

type nodeEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"`
	Size int64  `json:"size,omitempty"`
}

func makeListDirHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		path := request.GetString("path", "")
		excludeTests := request.GetBool("exclude_tests", false)

		children, err := g.ListChildren(path)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("list %q: %v", path, err)), nil
		}

		entries := make([]nodeEntry, 0, len(children))
		for _, childID := range children {
			name := filepath.Base(childID)
			if excludeTests && (strings.HasPrefix(name, "Test") || strings.HasPrefix(name, "Benchmark")) {
				continue
			}
			node, err := g.GetNode(childID)
			if err != nil {
				continue
			}
			typ := "file"
			if node.Mode.IsDir() {
				typ = "dir"
			}
			entries = append(entries, nodeEntry{
				Name: name,
				Path: childID,
				Type: typ,
				Size: node.ContentSize(),
			})
		}

		// Surface virtual entries on construct directories
		// (skip if already materialized in the db)
		if path != "" {
			seen := make(map[string]bool, len(entries))
			for _, e := range entries {
				seen[e.Name] = true
			}

			// Location: source file coordinates for orientation
			if !seen["location"] {
				if node, err := g.GetNode(path); err == nil && node.Properties != nil {
					if loc, ok := node.Properties["location"]; ok && len(loc) > 0 {
						entries = append(entries, nodeEntry{
							Name: "location",
							Path: path + "/location",
							Type: "virtual",
							Size: int64(len(loc)),
						})
					}
				}
			}

			token := filepath.Base(path)
			if !seen["callers"] {
				if callers, err := g.GetCallers(token); err == nil && len(callers) > 0 {
					entries = append(entries, nodeEntry{
						Name: "callers",
						Path: path + "/callers",
						Type: "virtual",
					})
				}
			}
			if !seen["callees"] {
				if callees, err := g.GetCallees(path); err == nil && len(callees) > 0 {
					entries = append(entries, nodeEntry{
						Name: "callees",
						Path: path + "/callees",
						Type: "virtual",
					})
				}
			}
		}

		data, _ := json.MarshalIndent(entries, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
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

func makeFindCallersHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		token := request.GetString("token", "")
		if token == "" {
			return mcp.NewToolResultError("token is required"), nil
		}

		callers, err := g.GetCallers(token)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("get callers: %v", err)), nil
		}

		paths := make([]string, 0, len(callers))
		for _, c := range callers {
			paths = append(paths, c.ID)
		}

		// Supplement with LSP references if available
		if qg, ok := g.(refsQuerier); ok {
			lspRefs, lspErr := queryLSPRefs(qg, token)
			if lspErr == nil && len(lspRefs) > 0 {
				type callersResult struct {
					Callers []string         `json:"callers"`
					LSPRefs []lspRefLocation `json:"lsp_refs"`
				}
				data, _ := json.MarshalIndent(callersResult{
					Callers: paths,
					LSPRefs: lspRefs,
				}, "", "  ")
				return mcp.NewToolResultText(string(data)), nil
			}
		}

		// No LSP data — return original format for backward compatibility
		if len(paths) == 0 {
			return mcp.NewToolResultText("[]"), nil
		}
		data, _ := json.MarshalIndent(paths, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
}

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
			if node.Properties != nil {
				if _, hasLang := node.Properties["lang"]; hasLang {
					hint = "no resolved callees — the construct may call unexported methods or use dynamic dispatch that the static extractor cannot resolve. Try find_callers with the method name instead."
				}
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

		type calleesResult struct {
			Callees  []string `json:"callees"`
			Warnings []string `json:"warnings,omitempty"`
		}
		out := calleesResult{Callees: calleePaths, Warnings: warnings}
		data, _ := json.MarshalIndent(out, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
}

func makeSearchHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		pattern := request.GetString("pattern", "")
		if pattern == "" {
			return mcp.NewToolResultError("pattern is required"), nil
		}

		typeFilter := request.GetString("type", "")
		role := request.GetString("role", "")
		limit := request.GetInt("limit", 100)

		type searchResult struct {
			Token string `json:"token"`
			Path  string `json:"path"`
			Role  string `json:"role,omitempty"`
		}

		// Definition search: scan the defs map with LIKE-style matching
		if role == "definition" {
			dp, ok := g.(defsMapProvider)
			if !ok {
				return mcp.NewToolResultError("backend does not support definition search"), nil
			}
			defs := dp.DefsMap()
			var results []searchResult
			seenPaths := make(map[string]bool)
			for token, ids := range defs {
				if !sqlLikeMatch(pattern, token) {
					continue
				}
				for _, id := range ids {
					if typeFilter != "" && !strings.Contains(id, "/"+typeFilter+"/") {
						continue
					}
					// Dedup: both "Foo.Bar" and "pkg.Foo.Bar" map to the same path
					if seenPaths[id] {
						continue
					}
					seenPaths[id] = true
					results = append(results, searchResult{Token: token, Path: id, Role: "definition"})
					if len(results) >= limit {
						break
					}
				}
				if len(results) >= limit {
					break
				}
			}
			if results == nil {
				results = []searchResult{}
			}
			data, _ := json.MarshalIndent(results, "", "  ")
			return mcp.NewToolResultText(string(data)), nil
		}

		// Reference search (default): query mache_refs
		qg, ok := g.(refsQuerier)
		if !ok {
			return mcp.NewToolResultError("reference search requires a SQLite-backed graph; use role=definition for in-memory search"), nil
		}
		var rows *sql.Rows
		var err error
		if typeFilter != "" {
			rows, err = qg.QueryRefs(
				"SELECT token, path FROM mache_refs WHERE token LIKE ? AND path LIKE ? LIMIT ?",
				pattern, "%/"+typeFilter+"/%", limit,
			)
		} else {
			rows, err = qg.QueryRefs(
				"SELECT token, path FROM mache_refs WHERE token LIKE ? LIMIT ?",
				pattern, limit,
			)
		}
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("query: %v", err)), nil
		}
		defer func() { _ = rows.Close() }()

		var results []searchResult
		for rows.Next() {
			var r searchResult
			if err := rows.Scan(&r.Token, &r.Path); err != nil {
				continue
			}
			results = append(results, r)
		}
		if results == nil {
			results = []searchResult{}
		}

		data, _ := json.MarshalIndent(results, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
}

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
		if err != nil {
			return mcp.NewToolResultError(
				"semantic search not available — ley-line daemon not responding.\n" +
					"This is an optional feature. Use 'search' for pattern-based code search instead.",
			), nil
		}
		defer func() { _ = sock.Close() }()

		sc := leyline.NewSemanticClient(sock)
		results, err := sc.Search(query, k)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("semantic search: %v", err)), nil
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

// sqlLikeMatch performs a simple SQL LIKE pattern match (% = wildcard).
func sqlLikeMatch(pattern, value string) bool {
	pattern = strings.ToLower(pattern)
	value = strings.ToLower(value)

	// Fast paths for common patterns
	if pattern == "%" {
		return true
	}
	if !strings.Contains(pattern, "%") {
		return pattern == value
	}

	parts := strings.Split(pattern, "%")
	pos := 0
	for i, part := range parts {
		if part == "" {
			continue
		}
		idx := strings.Index(value[pos:], part)
		if idx < 0 {
			return false
		}
		if i == 0 && idx != 0 {
			// First part must match start if pattern doesn't begin with %
			return false
		}
		pos += idx + len(part)
	}
	// If pattern doesn't end with %, value must end at pos
	if !strings.HasSuffix(pattern, "%") && pos != len(value) {
		return false
	}
	return true
}

func makeGetCommunitiesHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		minSize := request.GetInt("min_size", 2)
		summary := request.GetBool("summary", false)

		rp, ok := g.(refsMapProvider)
		if !ok {
			return mcp.NewToolResultError("community detection requires a graph with cross-reference data (SQLite backend or MemoryStore with refs)"), nil
		}
		refs := rp.RefsMap()
		if len(refs) == 0 {
			type emptyResult struct {
				Communities []any   `json:"communities"`
				NumNodes    int     `json:"num_nodes"`
				NumEdges    int     `json:"num_edges"`
				Modularity  float64 `json:"modularity"`
				Message     string  `json:"message"`
			}
			data, _ := json.Marshal(emptyResult{
				Communities: []any{},
				Message: "No cross-references indexed. Community detection requires constructs that share symbols. " +
					"Ensure the source was ingested with a schema that captures references (sitter_walker or json_walker with refs). " +
					"Use get_overview to check ref_tokens count.",
			})
			return mcp.NewToolResultText(string(data)), nil
		}

		result := graph.DetectCommunities(refs, minSize)

		// Push topology to ley-line sheaf cache (fire-and-forget).
		go func() {
			sockPath, err := leyline.DiscoverOrStart()
			if err != nil {
				return // no daemon — skip silently
			}
			sock, err := leyline.DialSocket(sockPath)
			if err != nil {
				return
			}
			defer func() { _ = sock.Close() }()
			sc := leyline.NewSheafClient(sock)
			if pushErr := sc.PushTopology(result, refs); pushErr != nil {
				log.Printf("sheaf topology push: %v", pushErr)
			}
		}()

		if summary {
			type communitySummary struct {
				ID         int      `json:"id"`
				Size       int      `json:"size"`
				TopMembers []string `json:"top_members"`
			}
			type summaryResult struct {
				NumCommunities int                `json:"num_communities"`
				NumNodes       int                `json:"num_nodes"`
				NumEdges       int                `json:"num_edges"`
				Modularity     float64            `json:"modularity"`
				Communities    []communitySummary `json:"communities"`
			}
			sr := summaryResult{
				NumCommunities: len(result.Communities),
				NumNodes:       result.NumNodes,
				NumEdges:       result.NumEdges,
				Modularity:     result.Modularity,
			}
			for _, c := range result.Communities {
				top := c.Members
				if len(top) > 5 {
					top = top[:5]
				}
				// Strip trailing /source from member paths — it's noise in summaries
				cleaned := make([]string, len(top))
				for i, m := range top {
					cleaned[i] = strings.TrimSuffix(m, "/source")
				}
				sr.Communities = append(sr.Communities, communitySummary{
					ID:         c.ID,
					Size:       len(c.Members),
					TopMembers: cleaned,
				})
			}
			data, _ := json.MarshalIndent(sr, "", "  ")
			return mcp.NewToolResultText(string(data)), nil
		}

		data, _ := json.MarshalIndent(result, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
}

func makeFindDefinitionHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		symbol := request.GetString("symbol", "")
		if symbol == "" {
			return mcp.NewToolResultError("symbol is required"), nil
		}

		dp := g.(defsMapProvider)
		defs := dp.DefsMap()

		// Try exact match first
		dirIDs, ok := defs[symbol]
		if !ok {
			// Try case-insensitive prefix/suffix matching
			var matches []string
			symbolLower := strings.ToLower(symbol)
			for token, ids := range defs {
				if strings.ToLower(token) == symbolLower {
					dirIDs = ids
					ok = true
					break
				}
				// Also collect partial matches for suggestions
				tokenLower := strings.ToLower(token)
				if strings.Contains(tokenLower, symbolLower) || strings.Contains(symbolLower, tokenLower) {
					for _, id := range ids {
						matches = append(matches, token+" → "+id)
					}
				}
			}
			if !ok {
				if len(matches) > 0 {
					if len(matches) > 20 {
						matches = matches[:20]
					}
					type suggestion struct {
						Message     string   `json:"message"`
						Suggestions []string `json:"suggestions"`
					}
					data, _ := json.MarshalIndent(suggestion{
						Message:     fmt.Sprintf("no exact definition for %q, but found similar symbols", symbol),
						Suggestions: matches,
					}, "", "  ")
					return mcp.NewToolResultText(string(data)), nil
				}

				// LSP fallback: try _lsp_defs from ley-line pre-baked DB
				if qg, ok := g.(refsQuerier); ok {
					lspDefs, err := queryLSPDefs(qg, symbol)
					if err == nil && len(lspDefs) > 0 {
						type lspResult struct {
							Symbol      string           `json:"symbol"`
							Source      string           `json:"source"`
							Definitions []lspDefLocation `json:"definitions"`
						}
						data, _ := json.MarshalIndent(lspResult{
							Symbol:      symbol,
							Source:      "lsp",
							Definitions: lspDefs,
						}, "", "  ")
						return mcp.NewToolResultText(string(data)), nil
					}
				}

				return mcp.NewToolResultText(fmt.Sprintf("no definition found for %q", symbol)), nil
			}
		}

		type defResult struct {
			Symbol      string   `json:"symbol"`
			Definitions []string `json:"definitions"`
		}
		data, _ := json.MarshalIndent(defResult{
			Symbol:      symbol,
			Definitions: dirIDs,
		}, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
}

func makeGetOverviewHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		type dirInfo struct {
			Name     string `json:"name"`
			Path     string `json:"path"`
			Children int    `json:"children"`
		}
		type overview struct {
			TopLevel   []dirInfo         `json:"top_level"`
			TotalDirs  int               `json:"total_dirs"`
			TotalFiles int               `json:"total_files"`
			RefTokens  int               `json:"ref_tokens,omitempty"`
			DefTokens  int               `json:"def_tokens,omitempty"`
			Usage      map[string]string `json:"_usage,omitempty"`
		}

		ov := overview{}

		// Top-level structure
		children, err := g.ListChildren("")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("list root: %v", err)), nil
		}

		for _, childID := range children {
			node, err := g.GetNode(childID)
			if err != nil {
				continue
			}
			if node.Mode.IsDir() {
				ov.TotalDirs++
				subChildren, _ := g.ListChildren(childID)
				ov.TopLevel = append(ov.TopLevel, dirInfo{
					Name:     filepath.Base(childID),
					Path:     childID,
					Children: len(subChildren),
				})
			} else {
				ov.TotalFiles++
			}
		}

		// Count refs/defs if available
		if rp, ok := g.(refsMapProvider); ok {
			ov.RefTokens = len(rp.RefsMap())
		}
		if dp, ok := g.(defsMapProvider); ok {
			ov.DefTokens = len(dp.DefsMap())
		}

		// Embed tool routing hints when a code graph is indexed (has cross-references).
		// This teaches the LLM which tool to use for each task without bloating the
		// system prompt — guidance is only in context after get_overview is called.
		if ov.RefTokens > 0 {
			ov.Usage = map[string]string{
				"find_callers":    "who calls a symbol — use instead of grep for 'who uses X?'",
				"find_definition": "where a symbol is declared — use for 'where is X defined?'",
				"find_callees":    "what a function invokes — note: generic names (String, New, Error) may have false positives",
				"search":          "find symbols by name pattern, e.g. '%auth%' or 'Parse%' — use instead of grep -r",
				"list_directory":  "browse the tree structure — use instead of ls/find",
				"get_communities": "find clusters of related code (use summary=true for large repos; requires dense cross-references)",
				"get_impact":      "blast radius of changing a symbol — traces callers/callees to a configurable depth",
			}
		}

		// Walk one level deeper to count total dirs/files
		for _, childID := range children {
			node, _ := g.GetNode(childID)
			if node != nil && node.Mode.IsDir() {
				subChildren, _ := g.ListChildren(childID)
				for _, subID := range subChildren {
					subNode, _ := g.GetNode(subID)
					if subNode != nil {
						if subNode.Mode.IsDir() {
							ov.TotalDirs++
						} else {
							ov.TotalFiles++
						}
					}
				}
			}
		}

		data, _ := json.MarshalIndent(ov, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
}
