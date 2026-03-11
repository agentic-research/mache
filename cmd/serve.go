package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
)

var serveCmd = &cobra.Command{
	Use:   "serve [data-source]",
	Short: "Serve a Mache graph as an MCP server over stdio",
	Long: `Starts an MCP (Model Context Protocol) server that exposes the graph
as tools over stdin/stdout JSON-RPC. Any MCP client (Claude Code, Claude Desktop,
etc.) can connect to browse and query the projected data.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runServe,
}

var serveSchema string

func init() {
	serveCmd.Flags().StringVarP(&serveSchema, "schema", "s", "", "Path to topology schema")
	rootCmd.AddCommand(serveCmd)
}

func runServe(cmd *cobra.Command, args []string) error {
	// Create a lazy graph that defers schema detection + graph building
	// to the first tool call. This lets the MCP server respond to
	// initialize immediately instead of blocking on auto-detection.
	lg := &lazyGraph{args: args}

	// Create MCP server IMMEDIATELY — respond to health checks fast
	s := server.NewMCPServer("mache", Version,
		server.WithToolCapabilities(false),
	)
	registerMCPTools(s, lg)

	log.Println("mache MCP server ready on stdio")
	return server.ServeStdio(s)
}

// lazyGraph wraps a Graph that is built on first access.
// This allows the MCP server to start and respond to initialize/tools/list
// before the potentially slow schema detection + ingestion completes.
type lazyGraph struct {
	args    []string
	once    sync.Once
	inner   graph.Graph
	cleanup func()
	err     error
}

func (lg *lazyGraph) init() {
	lg.once.Do(func() {
		var dataSource string
		var schema *api.Topology

		if len(lg.args) == 0 {
			cfg, err := loadProjectConfig(".")
			if err != nil {
				if !os.IsNotExist(err) {
					lg.err = err
					return
				}
				log.Printf("No %s found; auto-detecting project languages...", ConfigFileName)
				dataSource = "."
				schema, err = inferDirSchema(".")
				if err != nil {
					lg.err = fmt.Errorf("auto-detect schema: %w", err)
					return
				}
			} else {
				if len(cfg.Sources) > 1 {
					log.Printf("Warning: %s has %d sources but serve only uses the first; additional sources ignored", ConfigFileName, len(cfg.Sources))
				}
				src := cfg.Sources[0]
				dataSource, err = resolveDataSource(src.Path, ".")
				if err != nil {
					lg.err = fmt.Errorf("resolve data source: %w", err)
					return
				}
				schema, err = resolveSchema(src.Schema, ".")
				if err != nil {
					lg.err = fmt.Errorf("resolve schema: %w", err)
					return
				}
				if schema == nil {
					schema = &api.Topology{Version: api.SchemaVersion}
				}
				log.Printf("Loaded config from %s (source: %s)", ConfigFileName, dataSource)
			}
		} else {
			dataSource = lg.args[0]

			if serveSchema != "" {
				data, err := os.ReadFile(serveSchema)
				if err != nil {
					lg.err = fmt.Errorf("read schema: %w", err)
					return
				}
				schema = &api.Topology{}
				if err := json.Unmarshal(data, schema); err != nil {
					lg.err = fmt.Errorf("parse schema: %w", err)
					return
				}
			} else if filepath.Ext(dataSource) != ".db" {
				info, err := os.Stat(dataSource)
				if err == nil && info.IsDir() {
					schema, err = inferDirSchema(dataSource)
					if err != nil {
						lg.err = fmt.Errorf("auto-detect schema: %w", err)
						return
					}
				} else {
					schema = &api.Topology{Version: api.SchemaVersion}
				}
			} else {
				schema = &api.Topology{Version: api.SchemaVersion}
			}
		}

		g, cleanup, err := buildServeGraph(dataSource, schema)
		if err != nil {
			lg.err = err
			return
		}
		lg.inner = g
		lg.cleanup = cleanup
		log.Println("graph ready")
	})
}

func (lg *lazyGraph) get() (graph.Graph, error) {
	lg.init()
	if lg.err != nil {
		return nil, lg.err
	}
	return lg.inner, nil
}

func (lg *lazyGraph) GetNode(id string) (*graph.Node, error) {
	g, err := lg.get()
	if err != nil {
		return nil, err
	}
	return g.GetNode(id)
}

func (lg *lazyGraph) ListChildren(id string) ([]string, error) {
	g, err := lg.get()
	if err != nil {
		return nil, err
	}
	return g.ListChildren(id)
}

func (lg *lazyGraph) ReadContent(id string, buf []byte, offset int64) (int, error) {
	g, err := lg.get()
	if err != nil {
		return 0, err
	}
	return g.ReadContent(id, buf, offset)
}

func (lg *lazyGraph) GetCallers(token string) ([]*graph.Node, error) {
	g, err := lg.get()
	if err != nil {
		return nil, err
	}
	return g.GetCallers(token)
}

func (lg *lazyGraph) GetCallees(id string) ([]*graph.Node, error) {
	g, err := lg.get()
	if err != nil {
		return nil, err
	}
	return g.GetCallees(id)
}

func (lg *lazyGraph) Invalidate(id string) {
	g, _ := lg.get()
	if g != nil {
		g.Invalidate(id)
	}
}

func (lg *lazyGraph) Act(id, action, payload string) (*graph.ActionResult, error) {
	g, err := lg.get()
	if err != nil {
		return nil, err
	}
	return g.Act(id, action, payload)
}

// lazyGraph also implements refsQuerier, refsMapProvider, and defsMapProvider
// by delegating to the inner graph if it supports those interfaces.

func (lg *lazyGraph) QueryRefs(query string, args ...any) (*sql.Rows, error) {
	g, err := lg.get()
	if err != nil {
		return nil, err
	}
	if qg, ok := g.(refsQuerier); ok {
		return qg.QueryRefs(query, args...)
	}
	return nil, fmt.Errorf("backend does not support QueryRefs")
}

func (lg *lazyGraph) RefsMap() map[string][]string {
	g, err := lg.get()
	if err != nil || g == nil {
		return nil
	}
	if rp, ok := g.(refsMapProvider); ok {
		return rp.RefsMap()
	}
	return nil
}

func (lg *lazyGraph) DefsMap() map[string][]string {
	g, err := lg.get()
	if err != nil || g == nil {
		return nil
	}
	if dp, ok := g.(defsMapProvider); ok {
		return dp.DefsMap()
	}
	return nil
}

// buildServeGraph constructs a read-only Graph from the data source.
// Returns the graph, a cleanup function, and any error.
func buildServeGraph(dataSource string, schema *api.Topology) (graph.Graph, func(), error) {
	noop := func() {}

	if filepath.Ext(dataSource) == ".db" {
		sg, err := graph.OpenSQLiteGraph(dataSource, schema, ingest.RenderTemplate)
		if err != nil {
			return nil, noop, fmt.Errorf("open sqlite graph: %w", err)
		}
		sg.SetCallExtractor(newCallExtractor())
		if err := sg.EagerScan(); err != nil {
			_ = sg.Close()
			return nil, noop, fmt.Errorf("scan: %w", err)
		}
		return sg, func() { _ = sg.Close() }, nil
	}

	// MemoryStore path for JSON/source files
	store := graph.NewMemoryStore()
	resolver := ingest.NewSQLiteResolver()
	store.SetResolver(resolver.Resolve)
	store.SetCallExtractor(newCallExtractor())

	engine := ingest.NewEngine(schema, store)
	if err := engine.Ingest(dataSource); err != nil {
		resolver.Close()
		return nil, noop, fmt.Errorf("ingestion: %w", err)
	}

	if err := store.InitRefsDB(); err != nil {
		resolver.Close()
		return nil, noop, fmt.Errorf("init refs db: %w", err)
	}
	if err := store.FlushRefs(); err != nil {
		log.Printf("Warning: refs flush: %v", err)
	}

	return store, func() {
		_ = store.Close()
		resolver.Close()
	}, nil
}

// refsQuerier is the subset of Graph backends that support SQL queries.
type refsQuerier interface {
	QueryRefs(query string, args ...any) (*sql.Rows, error)
}

// refsMapProvider is the subset of Graph backends that expose their refs map
// for community detection (Louvain).
type refsMapProvider interface {
	RefsMap() map[string][]string
}

// defsMapProvider is the subset of Graph backends that expose their defs map
// for find_definition (symbol → where it's defined).
type defsMapProvider interface {
	DefsMap() map[string][]string
}

func registerMCPTools(s *server.MCPServer, g graph.Graph) {
	s.AddTool(
		mcp.NewTool("list_directory",
			mcp.WithDescription("List children of a directory node. Use empty path for root."),
			mcp.WithString("path", mcp.Description("Directory path (empty for root)")),
		),
		makeListDirHandler(g),
	)

	s.AddTool(
		mcp.NewTool("read_file",
			mcp.WithDescription("Read the text content of one or more file nodes. Pass a single path or a JSON array of paths for batch reads."),
			mcp.WithString("path", mcp.Description("Single file node path")),
			mcp.WithString("paths", mcp.Description("JSON array of file node paths for batch read, e.g. [\"path/a\", \"path/b\"]")),
		),
		makeReadFileHandler(g),
	)

	s.AddTool(
		mcp.NewTool("find_callers",
			mcp.WithDescription("Find all nodes that reference a given symbol or token."),
			mcp.WithString("token", mcp.Required(), mcp.Description("Symbol or token name")),
		),
		makeFindCallersHandler(g),
	)

	s.AddTool(
		mcp.NewTool("find_callees",
			mcp.WithDescription("Find all symbols called by a given construct."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Construct directory path")),
		),
		makeFindCalleesHandler(g),
	)

	// Only add search if the backend supports QueryRefs
	if _, ok := g.(refsQuerier); ok {
		s.AddTool(
			mcp.NewTool("search",
				mcp.WithDescription("Search for symbols matching a pattern (SQL LIKE: % = wildcard). Optionally filter by construct type."),
				mcp.WithString("pattern", mcp.Required(), mcp.Description("Search pattern, e.g. 'Login%' or '%auth%'")),
				mcp.WithString("type", mcp.Description("Filter by construct type in path, e.g. 'functions', 'methods', 'types', 'structs'")),
				mcp.WithNumber("limit", mcp.Description("Max results (default 100)")),
			),
			makeSearchHandler(g),
		)
	}

	// Only add get_communities if the backend exposes its refs map
	if _, ok := g.(refsMapProvider); ok {
		s.AddTool(
			mcp.NewTool("get_communities",
				mcp.WithDescription("Detect communities of densely co-referencing nodes using Louvain modularity optimization. Returns clusters of nodes that share symbols. Use summary=true for large codebases to get community sizes and top members without full member lists."),
				mcp.WithNumber("min_size", mcp.Description("Minimum community size (default 2)")),
				mcp.WithBoolean("summary", mcp.Description("Return summary only (ID, size, top 5 members per community) instead of full member lists. Recommended for large codebases.")),
			),
			makeGetCommunitiesHandler(g),
		)
	}

	// Only add find_definition if the backend exposes its defs map
	if _, ok := g.(defsMapProvider); ok {
		s.AddTool(
			mcp.NewTool("find_definition",
				mcp.WithDescription("Find where a symbol is defined. Returns the construct directory path(s) where the symbol is declared. Complements find_callers (who uses it) and find_callees (what it calls)."),
				mcp.WithString("symbol", mcp.Required(), mcp.Description("Symbol name to find definition for (e.g. 'GetCallers' or 'auth.Validate')")),
			),
			makeFindDefinitionHandler(g),
		)
	}

	// get_overview: aggregate first-contact orientation tool
	s.AddTool(
		mcp.NewTool("get_overview",
			mcp.WithDescription("Get a structural overview of the projected data: top-level directories, node counts, available cross-reference stats. Call this first when exploring an unfamiliar codebase."),
		),
		makeGetOverviewHandler(g),
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

		children, err := g.ListChildren(path)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("list %q: %v", path, err)), nil
		}

		entries := make([]nodeEntry, 0, len(children))
		for _, childID := range children {
			node, err := g.GetNode(childID)
			if err != nil {
				continue
			}
			typ := "file"
			if node.Mode.IsDir() {
				typ = "dir"
			}
			entries = append(entries, nodeEntry{
				Name: filepath.Base(childID),
				Path: childID,
				Type: typ,
				Size: node.ContentSize(),
			})
		}

		// Surface callers/ and callees/ virtual dirs on construct directories
		if path != "" {
			token := filepath.Base(path)
			if callers, err := g.GetCallers(token); err == nil && len(callers) > 0 {
				entries = append(entries, nodeEntry{
					Name: "callers",
					Path: path + "/callers",
					Type: "virtual",
				})
			}
			if callees, err := g.GetCallees(path); err == nil && len(callees) > 0 {
				entries = append(entries, nodeEntry{
					Name: "callees",
					Path: path + "/callees",
					Type: "virtual",
				})
			}
		}

		data, _ := json.MarshalIndent(entries, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
}

func readOneFile(g graph.Graph, path string) (string, error) {
	node, err := g.GetNode(path)
	if err != nil {
		return "", fmt.Errorf("not found: %s", path)
	}
	if node.Mode.IsDir() {
		return "", fmt.Errorf("%s is a directory — use list_directory", path)
	}
	size := node.ContentSize()
	if size == 0 {
		return "", nil
	}
	buf := make([]byte, size)
	n, err := g.ReadContent(path, buf, 0)
	if err != nil {
		return "", fmt.Errorf("read %s: %v", path, err)
	}
	return string(buf[:n]), nil
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

			type fileResult struct {
				Path    string `json:"path"`
				Content string `json:"content,omitempty"`
				Error   string `json:"error,omitempty"`
			}
			results := make([]fileResult, 0, len(paths))
			for _, p := range paths {
				content, err := readOneFile(g, p)
				if err != nil {
					results = append(results, fileResult{Path: p, Error: err.Error()})
				} else {
					results = append(results, fileResult{Path: p, Content: content})
				}
			}
			data, _ := json.MarshalIndent(results, "", "  ")
			return mcp.NewToolResultText(string(data)), nil
		}

		// Single mode
		if path == "" {
			return mcp.NewToolResultError("path or paths is required"), nil
		}
		content, err := readOneFile(g, path)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(content), nil
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
		if len(callers) == 0 {
			return mcp.NewToolResultText("[]"), nil
		}

		paths := make([]string, 0, len(callers))
		for _, c := range callers {
			paths = append(paths, c.ID)
		}
		data, _ := json.MarshalIndent(paths, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
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
			return mcp.NewToolResultText("[]"), nil
		}

		paths := make([]string, 0, len(callees))
		for _, c := range callees {
			paths = append(paths, c.ID)
		}
		data, _ := json.MarshalIndent(paths, "", "  ")
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
		limit := request.GetInt("limit", 100)

		qg := g.(refsQuerier)
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

		type searchResult struct {
			Token string `json:"token"`
			Path  string `json:"path"`
		}

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

func makeGetCommunitiesHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		minSize := request.GetInt("min_size", 2)
		summary := request.GetBool("summary", false)

		rp := g.(refsMapProvider)
		refs := rp.RefsMap()
		if len(refs) == 0 {
			return mcp.NewToolResultText("[]"), nil
		}

		result := graph.DetectCommunities(refs, minSize)

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
				sr.Communities = append(sr.Communities, communitySummary{
					ID:         c.ID,
					Size:       len(c.Members),
					TopMembers: top,
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
			TopLevel   []dirInfo `json:"top_level"`
			TotalDirs  int       `json:"total_dirs"`
			TotalFiles int       `json:"total_files"`
			RefTokens  int       `json:"ref_tokens,omitempty"`
			DefTokens  int       `json:"def_tokens,omitempty"`
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
