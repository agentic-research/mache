package cmd

import (
	"github.com/agentic-research/mache/internal/graph"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// registerMCPTools registers all tool definitions with session-aware handlers.
// All tools are registered unconditionally — lazyGraph delegates to the inner
// graph and returns errors for unsupported operations at call time. This lets
// different sessions (with different graph backends) coexist on one server.
func registerMCPTools(s *server.MCPServer, r *graphRegistry) {
	// Annotation preset FACTORIES. mcp.NewTool pre-seeds destructiveHint
	// and openWorldHint to true (the MCP spec defaults), so overriding only
	// readOnlyHint leaves read tools dishonestly advertising
	// destructive/open-world. WithToolAnnotation REPLACES the whole
	// annotation struct, so we state every hint explicitly
	// (mache-974697 follow-up: the single-field overrides leaked defaults).
	//
	// These are functions, not shared values: ToolAnnotation hints are
	// *bool, so one shared instance reused across every AddTool would alias
	// the same pointers — an in-place mutation on one tool's hint would
	// silently corrupt every other tool sharing the preset. Each call mints
	// a fresh ToolAnnotation with its own pointers, so tools are independent.
	readTool := func() mcp.ToolOption {
		return mcp.WithToolAnnotation(mcp.ToolAnnotation{
			ReadOnlyHint:    mcp.ToBoolPtr(true),
			DestructiveHint: mcp.ToBoolPtr(false),
			OpenWorldHint:   mcp.ToBoolPtr(false),
		})
	}
	// readToolExternal: read-only, but reaches the external ley-line daemon.
	readToolExternal := func() mcp.ToolOption {
		return mcp.WithToolAnnotation(mcp.ToolAnnotation{
			ReadOnlyHint:    mcp.ToBoolPtr(true),
			DestructiveHint: mcp.ToBoolPtr(false),
			OpenWorldHint:   mcp.ToBoolPtr(true),
		})
	}
	// writeTool: the only mutating tool (write_file).
	writeTool := func() mcp.ToolOption {
		return mcp.WithToolAnnotation(mcp.ToolAnnotation{
			ReadOnlyHint:    mcp.ToBoolPtr(false),
			DestructiveHint: mcp.ToBoolPtr(true),
			OpenWorldHint:   mcp.ToBoolPtr(false),
		})
	}

	s.AddTool(
		mcp.NewTool("list_directory",
			readTool(),
			mcp.WithDescription("Browse the projected tree. Use instead of ls/find for 'what's in directory X?', 'list packages under Y'. Use empty path for root."),
			mcp.WithString("path", mcp.Description("Directory path (empty for root)")),
			mcp.WithBoolean("exclude_tests", mcp.Description("Exclude Test* and Benchmark* entries (default false). Recommended for large packages.")),
		),
		r.wrapHandler(makeListDirHandler),
	)

	s.AddTool(
		mcp.NewTool("read_file",
			readTool(),
			mcp.WithDescription("Read the text content of one or more file nodes. Pass a single path or a JSON array of paths for batch reads."),
			mcp.WithString("path", mcp.Description("Single file node path")),
			mcp.WithString("paths", mcp.Description("JSON array of file node paths for batch read, e.g. [\"path/a\", \"path/b\"]")),
			mcp.WithString("mode", mcp.Description("Projection: 'full' (default, whole content), 'signatures' (declaration line of every construct under a directory node — ~18x smaller than reading the file and needs no line offsets, best for orienting in unfamiliar code), or 'map' (names only).")),
		),
		r.wrapHandler(makeReadFileHandler),
	)

	s.AddTool(
		mcp.NewTool("find_callers",
			readTool(),
			mcp.WithDescription("Find all constructs that reference a symbol. Use for 'who calls X?', 'where is X used?', 'find usages of X'. More accurate than grep for symbol lookup."),
			mcp.WithString("token", mcp.Required(), mcp.Description("Symbol or token name (e.g. 'GetCallers', 'ParseVuln')")),
			mcp.WithString("kind", mcp.Description("Optional construct-kind filter applied to the CALLERS (e.g. kind=method narrows to method-callers only). Accepted values: function, method, type, constant, variable, import. Omit to return all kinds.")),
		),
		r.wrapHandler(makeFindCallersHandler),
	)

	s.AddTool(
		mcp.NewTool("find_callees",
			readTool(),
			mcp.WithDescription("Find what a function/method calls. Use for 'what does X invoke?', 'dependencies of X'. Note: generic names (String, New, Error) may have false positives — prefer qualified calls."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Construct directory path (e.g. 'go/graph/methods/GetCallees')")),
		),
		r.wrapHandler(makeFindCalleesHandler),
	)

	s.AddTool(
		mcp.NewTool("search",
			readTool(),
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
			readToolExternal(),
			mcp.WithDescription("Find code by meaning using embedding similarity. Use for 'find code that does X', 'functions related to authentication', 'error handling patterns'. More flexible than pattern search — finds conceptually similar code even without exact name matches. Requires ley-line daemon with --embed flag."),
			mcp.WithString("query", mcp.Required(), mcp.Description("Natural language description of what you're looking for")),
			mcp.WithNumber("k", mcp.Description("Max results (default 10)")),
		),
		r.wrapHandler(makeSemanticSearchHandler),
	)

	s.AddTool(
		mcp.NewTool("get_communities",
			readTool(),
			mcp.WithDescription("Find clusters of related code using Louvain modularity detection. Use for 'what code works together?', 'find subsystems'. Requires dense cross-references. Use summary=true for large codebases."),
			mcp.WithNumber("min_size", mcp.Description("Minimum community size (default 2)")),
			mcp.WithBoolean("summary", mcp.Description("Return summary only (ID, size, top 5 members per community) instead of full member lists. Recommended for large codebases.")),
		),
		r.wrapHandler(makeGetCommunitiesHandler),
	)

	s.AddTool(
		mcp.NewTool("find_definition",
			readTool(),
			mcp.WithDescription("Find where a symbol is declared. Use for 'where is X defined?', 'where does X come from?'. Default match is anchored: case-sensitive exact, then case-insensitive exact. Set fuzzy=true to also fall back to substring suggestions when no anchored match exists (recommended only for short queries in unfamiliar codebases — fuzzy is noisy in monorepos)."),
			mcp.WithString("symbol", mcp.Required(), mcp.Description("Symbol name to find definition for (e.g. 'GetCallers' or 'auth.Validate')")),
			mcp.WithString("kind", mcp.Description("Optional construct-kind filter to disambiguate when the same name appears in multiple kinds. Accepted values: function, method, type, constant, variable, import. Omit to return all kinds.")),
			mcp.WithBoolean("fuzzy", mcp.Description("Fall back to substring matching when no anchored match is found (default false). Symbols shorter than 4 characters are never fuzzy-matched.")),
		),
		r.wrapHandler(makeFindDefinitionHandler),
	)

	s.AddTool(
		mcp.NewTool("get_type_info",
			readToolExternal(),
			mcp.WithDescription("Get type signature and documentation for a symbol from LSP hover data. Returns the language server's type information (e.g. function signatures, struct definitions). If LSP data is missing and 'file' is provided, auto-enriches via ley-line daemon."),
			mcp.WithString("symbol", mcp.Required(), mcp.Description("Symbol name to look up (e.g. 'NewEncoder', 'Model')")),
			mcp.WithString("kind", mcp.Description("Optional construct-kind filter to disambiguate when the same name appears in multiple kinds. Accepted values: function, method, type, constant, variable, import. Omit to return all kinds.")),
			mcp.WithString("file", mcp.Description("Source file path — triggers automatic LSP enrichment if _lsp_hover table is missing")),
		),
		r.wrapHandler(makeGetTypeInfoHandler),
	)

	s.AddTool(
		mcp.NewTool("get_diagnostics",
			readToolExternal(),
			mcp.WithDescription("Get LSP diagnostics (errors, warnings) for symbols. Returns diagnostics from the language server. If LSP data is missing and 'file' is provided, auto-enriches via ley-line daemon."),
			mcp.WithString("symbol", mcp.Description("Symbol name to filter by (optional, returns all if empty)")),
			mcp.WithString("kind", mcp.Description("Optional construct-kind filter applied to result NodeIDs. Accepted values: function, method, type, constant, variable, import. Omit to return all kinds.")),
			mcp.WithNumber("limit", mcp.Description("Max results (default 50)")),
			mcp.WithString("file", mcp.Description("Source file path — triggers automatic LSP enrichment if _lsp table is missing")),
		),
		r.wrapHandler(makeGetDiagnosticsHandler),
	)

	s.AddTool(
		mcp.NewTool("get_overview",
			readTool(),
			mcp.WithDescription("START HERE when exploring a codebase. Returns top-level structure, node counts, cross-reference stats, and a usage guide for all tools."),
		),
		r.wrapHandler(makeGetOverviewHandler),
	)

	// get_sheaf_status surfaces the ley-line daemon's sheaf state to
	// agents — generation (monotonic, advances on every cascade run),
	// valid/total cache entries, defect score. Lets an agent decide
	// whether a cached result is still fresh after an edit, without
	// having to invalidate the whole world. Registered directly (no
	// graph-registry wrapping) because the tool doesn't depend on
	// session state; it talks to the daemon over UDS using the
	// well-known socket path.
	s.AddTool(
		mcp.NewTool("get_sheaf_status",
			readToolExternal(),
			mcp.WithDescription("Returns the ley-line daemon's sheaf cache state (generation counter, valid/total entries, defect score) AND the local subscriber state (whether mache is receiving sheaf.invalidate events, last event seen, last generation observed). Use to decide whether cached find_callers / find_definition / get_architecture results are still fresh after an edit. Returns {available: false, reason: ...} when the daemon is not reachable rather than erroring — safe to call periodically."),
		),
		makeGetSheafStatusHandler(r.sheafSubscriberAccessor()),
	)

	s.AddTool(
		mcp.NewTool("get_impact",
			readTool(),
			mcp.WithDescription("Change impact analysis: given a symbol, trace through the refs graph to show affected callers and/or callees (multi-hop BFS traversal). Use for 'what would be affected if I change X?', 'blast radius of modifying Y'."),
			mcp.WithString("symbol", mcp.Required(), mcp.Description("Symbol name to analyze (e.g. 'GetCallers', 'auth.Validate')")),
			mcp.WithString("kind", mcp.Description("Optional construct-kind filter applied to the STARTING symbol's root (e.g. kind=function picks the function-Encoder as the BFS root when both a type and function are named Encoder). Traversal at depth>0 follows callers/callees regardless of their kind. Accepted values: function, method, type, constant, variable, import. Omit to use all matching roots.")),
			mcp.WithNumber("depth", mcp.Description("Max traversal depth (default 2)")),
			mcp.WithString("direction", mcp.Description("Traversal direction: 'callers' (who calls this), 'callees' (what this calls), 'both' (default 'both')")),
		),
		r.wrapHandler(makeGetImpactHandler),
	)

	s.AddTool(
		mcp.NewTool("get_dataflow",
			readTool(),
			mcp.WithDescription("Bounded interprocedural reference flow for a symbol through the existing node_refs caller/callee projection. Every edge is labeled node_ref; this is not LSP-confirmed binding, SSA, taint, or data-dependence analysis."),
			mcp.WithString("symbol", mcp.Required(), mcp.Description("Function or symbol name to resolve through node_defs")),
			mcp.WithString("kind", mcp.Description("Optional construct-kind filter applied to the starting symbol roots. Accepted values: function, method, type, constant, variable, import.")),
			mcp.WithString("direction", mcp.Description("Traversal direction: 'callers', 'callees', or 'both' (default 'both')")),
			mcp.WithNumber("depth", mcp.Description("Max traversal depth from 1 to 5 (default 2)")),
		),
		r.wrapHandler(makeGetDataflowHandler),
	)

	s.AddTool(
		mcp.NewTool("get_architecture",
			readTool(),
			mcp.WithDescription("Structured architectural analysis of the codebase. Returns entry points (high fan-in), key abstractions (most defs), dependency layers (community-based), test files, API surface (exported symbols), file count, and language breakdown. Use after get_overview for deeper orientation."),
		),
		r.wrapHandler(makeGetArchitectureHandler),
	)

	s.AddTool(
		mcp.NewTool("get_diagram",
			readTool(),
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
			writeTool(),
			mcp.WithDescription("Write new content to a source file node. Pipeline: validate (tree-sitter, always) → format (gofumpt/hclwrite, opt-out via format=false) → atomic splice into source file → update graph. The node must have a source origin. Set format=false when the caller (LLM, pre-commit) already owns formatting and wants mache to splice verbatim."),
			mcp.WithString("path", mcp.Required(), mcp.Description("File node path (e.g. 'go/graph/methods/MemoryStore.GetCallees/source')")),
			mcp.WithString("content", mcp.Required(), mcp.Description("New content to write")),
			mcp.WithBoolean("format", mcp.Description("Run the language formatter (gofumpt for Go, hclwrite for HCL) before splicing. Default true. Set false when formatting is owned upstream — validation still runs.")),
		),
		r.wrapHandler(makeWriteFileHandler),
	)

	s.AddTool(
		mcp.NewTool("resolve_ref",
			readTool(),
			mcp.WithDescription("Resolve a typed cross-language ref token (scheme:locator) to its target. Part of the cross-language ref graph (mache-q43l, ADR-0016). Supports `mod:` (local-relative paths, ./X or ../Y, resolved against base_path — e.g. following a Terraform `module { source = ... }` reference) and `gomod:` (a Go import path, resolved via `go list` against the served project's own go.mod — e.g. `gomod:golang.org/x/sync/singleflight`). Other schemes return a `remote_hint` so the caller knows resolution was skipped. A successful resolution returns `graph_path`: pass it to list_directory/find_definition/find_callers/read_file to query the resolved code directly, no separate clone or ingest step needed."),
			mcp.WithString("token", mcp.Required(), mcp.Description("Typed ref token in `scheme:locator` form (e.g. `mod:./modules/vpc` or `gomod:golang.org/x/sync/singleflight`).")),
			mcp.WithString("base_path", mcp.Description("File or directory the locator is interpreted relative to. Required for `mod:` locators; unused for `gomod:`.")),
		),
		r.wrapHandler(makeResolveRefHandler),
	)

	s.AddTool(
		mcp.NewTool("find_smells",
			readTool(),
			mcp.WithDescription("Run structural code-smell rules against the parsed _ast table. Call with no arguments for the registry listing; call with `rule` to scan. Rules look for patterns like 'magic int literal in a binary expression' (Go) — useful for sweeping a monorepo before adding named constants. Each finding includes line/column and a short snippet so editors can jump to it. Requires a leyline-parsed .db (the _ast table)."),
			mcp.WithString("rule", mcp.Description("Rule ID to run. Omit to list available rules.")),
			mcp.WithString("source_id", mcp.Description("Limit the scan to one parsed source file (matches _source.id, e.g. 'main.go').")),
			mcp.WithNumber("limit", mcp.Description("Max findings (default 200).")),
		),
		r.wrapHandler(func(g graph.Graph) server.ToolHandlerFunc {
			// Bind the startup-resolved rules dir; the handler rescans it
			// per request for live rule reload (mache smell-rules config).
			return makeFindSmellsHandler(g, r.smellRulesDir)
		}),
	)
}
