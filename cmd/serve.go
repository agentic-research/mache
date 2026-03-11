package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

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
	var dataSource string
	var schema *api.Topology

	if len(args) == 0 {
		// Zero-arg mode: load from .mache.json
		cfg, err := loadProjectConfig(".")
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("no data source specified and no %s found\n\nRun 'mache init' to create one, or specify a data source: mache serve <path>", ConfigFileName)
			}
			return err
		}
		if len(cfg.Sources) > 1 {
			log.Printf("Warning: %s has %d sources but serve only uses the first; additional sources ignored", ConfigFileName, len(cfg.Sources))
		}
		src := cfg.Sources[0]
		dataSource, err = resolveDataSource(src.Path, ".")
		if err != nil {
			return fmt.Errorf("resolve data source: %w", err)
		}
		schema, err = resolveSchema(src.Schema, ".")
		if err != nil {
			return fmt.Errorf("resolve schema: %w", err)
		}
		if schema == nil {
			schema = &api.Topology{Version: api.SchemaVersion}
		}
		log.Printf("Loaded config from %s (source: %s)", ConfigFileName, dataSource)
	} else {
		dataSource = args[0]

		// Explicit --schema flag
		if serveSchema != "" {
			data, err := os.ReadFile(serveSchema)
			if err != nil {
				return fmt.Errorf("read schema: %w", err)
			}
			schema = &api.Topology{}
			if err := json.Unmarshal(data, schema); err != nil {
				return fmt.Errorf("parse schema: %w", err)
			}
		} else {
			schema = &api.Topology{Version: api.SchemaVersion}
		}
	}

	// Build graph
	g, cleanup, err := buildServeGraph(dataSource, schema)
	if err != nil {
		return err
	}
	defer cleanup()

	// Create MCP server
	s := server.NewMCPServer("mache", Version,
		server.WithToolCapabilities(false),
	)
	registerMCPTools(s, g)

	log.Println("mache MCP server ready on stdio")
	return server.ServeStdio(s)
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
			mcp.WithDescription("Read the text content of a file node."),
			mcp.WithString("path", mcp.Required(), mcp.Description("File node path")),
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
				mcp.WithDescription("Search for symbols matching a pattern (SQL LIKE: % = wildcard)."),
				mcp.WithString("pattern", mcp.Required(), mcp.Description("Search pattern, e.g. 'Login%' or '%auth%'")),
				mcp.WithNumber("limit", mcp.Description("Max results (default 100)")),
			),
			makeSearchHandler(g),
		)
	}

	// Only add get_communities if the backend exposes its refs map
	if _, ok := g.(refsMapProvider); ok {
		s.AddTool(
			mcp.NewTool("get_communities",
				mcp.WithDescription("Detect communities of densely co-referencing nodes using Louvain modularity optimization. Returns clusters of nodes that share symbols."),
				mcp.WithNumber("min_size", mcp.Description("Minimum community size (default 2)")),
			),
			makeGetCommunitiesHandler(g),
		)
	}
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

		data, _ := json.MarshalIndent(entries, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
}

func makeReadFileHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		path := request.GetString("path", "")
		if path == "" {
			return mcp.NewToolResultError("path is required"), nil
		}

		node, err := g.GetNode(path)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("not found: %s", path)), nil
		}
		if node.Mode.IsDir() {
			return mcp.NewToolResultError(fmt.Sprintf("%s is a directory — use list_directory", path)), nil
		}

		size := node.ContentSize()
		if size == 0 {
			return mcp.NewToolResultText(""), nil
		}

		buf := make([]byte, size)
		n, err := g.ReadContent(path, buf, 0)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("read %s: %v", path, err)), nil
		}
		return mcp.NewToolResultText(string(buf[:n])), nil
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

		limit := request.GetInt("limit", 100)

		qg := g.(refsQuerier)
		rows, err := qg.QueryRefs(
			"SELECT token, path FROM mache_refs WHERE token LIKE ? LIMIT ?",
			pattern, limit,
		)
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

		rp := g.(refsMapProvider)
		refs := rp.RefsMap()
		if len(refs) == 0 {
			return mcp.NewToolResultText("[]"), nil
		}

		result := graph.DetectCommunities(refs, minSize)

		data, _ := json.MarshalIndent(result, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
}
