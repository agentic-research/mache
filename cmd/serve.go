package cmd

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/leyline"
	"github.com/agentic-research/mache/internal/writeback"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
)

var serveCmd = &cobra.Command{
	Use:   "serve [data-source]",
	Short: "Serve a Mache graph as an MCP server",
	Long: `Starts an MCP (Model Context Protocol) server that exposes the graph
as tools. By default starts a Streamable HTTP server on localhost:7532.
Use --stdio for subprocess mode (client manages lifecycle).

Examples:
  mache serve ./data.db                  # HTTP on localhost:7532 (default)
  mache serve --http :9000 ./data.db     # HTTP on custom port (all interfaces)
  mache serve --stdio ./data.db          # stdio (subprocess mode)
  claude mcp add --transport http mache http://localhost:7532/mcp`,
	Args: cobra.MaximumNArgs(1),
	RunE: runServe,
}

var (
	serveSchema string
	serveHTTP   string
	serveStdio  bool
	servePath   string
	serveRepo   string
)

func init() {
	serveCmd.Flags().StringVarP(&serveSchema, "schema", "s", "", "Path to topology schema")
	serveCmd.Flags().StringVar(&serveHTTP, "http", "localhost:7532", "Listen address for Streamable HTTP transport")
	serveCmd.Flags().BoolVar(&serveStdio, "stdio", false, "Use stdio transport instead of HTTP (for subprocess mode)")
	serveCmd.Flags().StringVar(&servePath, "path", "", "Base directory for project detection (defaults to current working directory)")
	serveCmd.Flags().StringVar(&serveRepo, "repo", "", "Git repo URL to clone and serve (ephemeral: cleaned up on exit)")
	rootCmd.AddCommand(serveCmd)
}

func runServe(cmd *cobra.Command, args []string) error {
	// Ephemeral mode: clone repo to temp dir, serve from there, cleanup on exit
	if serveRepo != "" {
		tmpDir, cleanup, err := cloneRepo(serveRepo)
		if err != nil {
			return fmt.Errorf("clone %s: %w", serveRepo, err)
		}
		defer cleanup()
		// Override basePath to the cloned directory
		servePath = tmpDir
		log.Printf("ephemeral mode: serving %s from %s", serveRepo, tmpDir)
	}

	registry := newGraphRegistry(servePath, args)
	defer registry.Close()

	// Clean up session → root mapping on disconnect.
	// Root discovery happens lazily on the first tool call (see wrapHandler)
	// because ListRoots deadlocks inside OnAfterInitialize — the client
	// can't respond until the initialize response is sent.
	hooks := &server.Hooks{}
	hooks.AddOnUnregisterSession(func(_ context.Context, session server.ClientSession) {
		registry.unregisterSession(session.SessionID())
		log.Printf("session %s unregistered", session.SessionID())
	})

	// Create MCP server IMMEDIATELY — respond to health checks fast
	s := server.NewMCPServer("mache", Version,
		server.WithToolCapabilities(false),
		server.WithHooks(hooks),
		server.WithInstructions(`Mache provides structural code intelligence tools. Use mache when you need to:
- Explore unfamiliar codebases (get_overview, list_directory, read_file)
- Find where symbols are defined or used (find_definition, find_callers, find_callees)
- Search for code by pattern (search)
- Understand code structure and communities (get_communities)
- Get type information and diagnostics from LSP (get_type_info, get_diagnostics)
- Analyze change blast radius (get_impact)
Call get_overview first when exploring a new codebase.`),
	)
	registerMCPTools(s, registry)

	// Resolve source label for sidecar metadata
	source := registry.resolvedBasePath()
	if len(args) > 0 {
		source = args[0]
	}

	// Clean up any auto-spawned leyline daemon on exit.
	// Defer handles normal returns; signal handler covers SIGTERM/SIGINT.
	defer leyline.StopManaged()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		leyline.StopManaged()
		registry.Close()
		os.Exit(0)
	}()

	if serveStdio {
		meta := registerServeSidecar(source, "mcp-stdio", "")
		defer removeServeSidecar(meta)
		log.Println("mache MCP server ready on stdio")
		return server.ServeStdio(s)
	}

	// MCP spec: servers MUST NOT bind to 0.0.0.0 — loopback only.
	if err := validateHTTPAddr(serveHTTP); err != nil {
		return err
	}

	meta := registerServeSidecar(source, "mcp-http", serveHTTP)
	defer removeServeSidecar(meta)

	httpServer := server.NewStreamableHTTPServer(s)
	log.Printf("mache MCP server listening on %s/mcp (Streamable HTTP)", serveHTTP)
	return httpServer.Start(serveHTTP)
}

// registerServeSidecar writes a sidecar metadata file so `mache list` can discover
// running MCP servers alongside FUSE/NFS mounts.
func registerServeSidecar(source, typ, addr string) *MountMetadata {
	mountsDir, err := getAgentMountsDir()
	if err != nil {
		log.Printf("Warning: could not register serve instance: %v", err)
		return nil
	}
	// Use a stable name derived from type + addr/pid
	name := fmt.Sprintf("serve-%d", os.Getpid())
	mountPoint := filepath.Join(mountsDir, name)

	meta := &MountMetadata{
		PID:        os.Getpid(),
		Source:     source,
		MountPoint: mountPoint,
		Type:       typ,
		Addr:       addr,
		Timestamp:  time.Now(),
	}
	if err := saveMountMetadata(mountPoint, meta); err != nil {
		log.Printf("Warning: could not save serve metadata: %v", err)
		return nil
	}
	return meta
}

// validateHTTPAddr rejects non-loopback bind addresses per MCP spec:
// "servers MUST only bind to localhost and MUST NOT bind to 0.0.0.0".
func validateHTTPAddr(addr string) error {
	host := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
	}
	// Empty host (e.g. ":7532") means all interfaces in Go — reject it.
	if host == "" || host == "0.0.0.0" || host == "[::]" {
		return fmt.Errorf("MCP spec prohibits binding to all interfaces (%q); use localhost:%s instead", addr, addr[strings.LastIndex(addr, ":")+1:])
	}
	// Allow localhost, 127.x.x.x, ::1
	if host == "localhost" || host == "127.0.0.1" || host == "[::1]" || strings.HasPrefix(host, "127.") {
		return nil
	}
	return fmt.Errorf("MCP spec requires loopback binding; %q is not localhost — use localhost:<port> instead", addr)
}

// removeServeSidecar cleans up the sidecar file on shutdown.
func removeServeSidecar(meta *MountMetadata) {
	if meta == nil {
		return
	}
	_ = os.Remove(sidecarPath(meta.MountPoint))
}

// ---------------------------------------------------------------------------
// graphRegistry: multi-tenant session → graph routing
// ---------------------------------------------------------------------------

// graphRegistry maps MCP sessions to per-workspace graphs.
// Each session's workspace root (from ListRoots) gets its own lazily-built
// graph. Sessions without roots fall back to basePath (--path flag or CWD).
type graphRegistry struct {
	basePath string   // --path flag default
	args     []string // positional args from command line
	graphs   sync.Map // rootPath -> *lazyGraph
	sessions sync.Map // sessionID -> rootPath
}

func newGraphRegistry(basePath string, args []string) *graphRegistry {
	return &graphRegistry{basePath: basePath, args: args}
}

// resolvedBasePath returns basePath if set, otherwise ".".
func (r *graphRegistry) resolvedBasePath() string {
	if r.basePath != "" {
		return r.basePath
	}
	return "."
}

func (r *graphRegistry) registerSession(sessionID, rootPath string) {
	r.sessions.Store(sessionID, rootPath)
}

func (r *graphRegistry) unregisterSession(sessionID string) {
	r.sessions.Delete(sessionID)
}

// Close calls the cleanup function on every lazily-built graph.
// Use on server shutdown to release SQLite connections and temp files.
func (r *graphRegistry) Close() {
	r.graphs.Range(func(_, v any) bool {
		lg := v.(*lazyGraph)
		if lg.cleanup != nil {
			lg.cleanup()
		}
		return true
	})
}

// getOrCreateGraph returns an existing graph for rootPath or creates a new one.
// The cache key includes the current git HEAD commit hash so that switching
// branches at the same path produces a fresh graph instead of a stale one.
func (r *graphRegistry) getOrCreateGraph(rootPath string) *lazyGraph {
	cacheKey := rootPath
	if commit := getGitHead(rootPath); commit != "" {
		cacheKey = rootPath + "@" + commit
	}
	// Fast path: return an existing graph if present for this exact cache key.
	if v, ok := r.graphs.Load(cacheKey); ok {
		return v.(*lazyGraph)
	}
	// Evict any prior graphs for the same rootPath but a different commit hash.
	// This prevents unbounded accumulation of *lazyGraph instances (and their
	// associated SQLite connections/temp files) across branch switches.
	prefix := rootPath + "@"
	r.graphs.Range(func(k, v any) bool {
		keyStr := k.(string)
		if keyStr != cacheKey && (keyStr == rootPath || strings.HasPrefix(keyStr, prefix)) {
			if oldLg, ok := v.(*lazyGraph); ok && oldLg.cleanup != nil {
				oldLg.cleanup()
			}
			r.graphs.Delete(k)
		}
		return true
	})
	lg := &lazyGraph{args: r.args, basePath: rootPath}
	actual, _ := r.graphs.LoadOrStore(cacheKey, lg)
	return actual.(*lazyGraph)
}

// graphForSession returns the graph for a session, falling back to basePath.
func (r *graphRegistry) graphForSession(sessionID string) *lazyGraph {
	if rootPath, ok := r.sessions.Load(sessionID); ok {
		return r.getOrCreateGraph(rootPath.(string))
	}
	return r.getOrCreateGraph(r.resolvedBasePath())
}

// wrapHandler turns a handler factory (graph → handler) into a session-aware
// handler that resolves the correct graph per-session at call time.
//
// On the first tool call for an unmapped session, it calls ListRoots to
// discover the client's workspace root and caches the mapping. This is done
// here (not in OnAfterInitialize) because ListRoots deadlocks during the
// initialize handshake — the client can't respond until initialize completes.
func (r *graphRegistry) wrapHandler(handlerFactory func(graph.Graph) server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		session := server.ClientSessionFromContext(ctx)
		var lg *lazyGraph
		if session != nil {
			lg = r.resolveSession(ctx, session)
		} else {
			lg = r.getOrCreateGraph(r.resolvedBasePath())
		}
		return handlerFactory(lg)(ctx, req)
	}
}

// resolveSession returns the graph for a session, calling ListRoots on first
// access to discover the client's workspace root.
func (r *graphRegistry) resolveSession(ctx context.Context, session server.ClientSession) *lazyGraph {
	sid := session.SessionID()

	// Fast path: already mapped
	if rootPath, ok := r.sessions.Load(sid); ok {
		return r.getOrCreateGraph(rootPath.(string))
	}

	// Slow path: ask the client for its workspace roots.
	// Use a short timeout — if the client doesn't support roots or can't
	// respond, fall back immediately rather than blocking the tool call.
	if rootsSession, ok := session.(server.SessionWithRoots); ok {
		rootsCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		result, err := rootsSession.ListRoots(rootsCtx, mcp.ListRootsRequest{})
		if err == nil && len(result.Roots) > 0 {
			if rootPath := rootURIToPath(result.Roots[0].URI); rootPath != "" {
				r.registerSession(sid, rootPath)
				log.Printf("session %s → %s", sid, rootPath)
				return r.getOrCreateGraph(rootPath)
			}
		} else if err != nil {
			log.Printf("ListRoots for session %s: %v (using default path)", sid, err)
		}
	}

	// Fallback: use --path or CWD, and cache so we don't retry ListRoots
	fallback := r.resolvedBasePath()
	r.registerSession(sid, fallback)
	return r.getOrCreateGraph(fallback)
}

// getGitHead returns the current git commit hash (first 12 chars) for the
// repository at rootPath, by reading git metadata and resolving any ref pointer.
// Supports worktrees and submodules (where .git is a file with a gitdir pointer)
// and falls back to packed-refs when loose refs are missing.
// Returns empty string if rootPath is not a git repository or the ref cannot
// be resolved to an actual commit hash.
func getGitHead(rootPath string) string {
	gitPath := filepath.Join(rootPath, ".git")
	fi, err := os.Stat(gitPath)
	if err != nil {
		return ""
	}
	gitDir := gitPath

	// Handle worktrees/submodules where .git is a file containing "gitdir: <path>".
	if !fi.IsDir() {
		data, err := os.ReadFile(gitPath)
		if err != nil {
			return ""
		}
		line := strings.TrimSpace(string(data))
		if !strings.HasPrefix(line, "gitdir: ") {
			return ""
		}
		gitDirPath := strings.TrimSpace(strings.TrimPrefix(line, "gitdir: "))
		if !filepath.IsAbs(gitDirPath) {
			gitDirPath = filepath.Join(rootPath, gitDirPath)
		}
		gitDir = gitDirPath
	}

	headFile := filepath.Join(gitDir, "HEAD")
	data, err := os.ReadFile(headFile)
	if err != nil {
		return ""
	}
	head := strings.TrimSpace(string(data))

	if strings.HasPrefix(head, "ref: ") {
		ref := strings.TrimPrefix(head, "ref: ")
		refPath := filepath.Join(gitDir, filepath.FromSlash(ref))
		if data2, err := os.ReadFile(refPath); err == nil {
			// Loose ref found — use its content.
			head = strings.TrimSpace(string(data2))
		} else if hash := resolvePackedRef(gitDir, ref); hash != "" {
			// Loose ref missing — try packed-refs.
			head = hash
		} else {
			// Ref cannot be resolved to a hash — disable git isolation
			// rather than returning an unstable non-commit cache key.
			return ""
		}
	}

	if len(head) > 12 {
		return head[:12]
	}
	return head
}

// resolvePackedRef searches the packed-refs file in gitDir for the given ref
// and returns the commit hash if found, or empty string otherwise.
func resolvePackedRef(gitDir, ref string) string {
	f, err := os.Open(filepath.Join(gitDir, "packed-refs"))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || line[0] == '#' || line[0] == '^' {
			continue
		}
		fields := strings.SplitN(line, " ", 2)
		if len(fields) == 2 && strings.TrimSpace(fields[1]) == ref {
			return strings.TrimSpace(fields[0])
		}
	}
	return ""
}

// rootURIToPath converts a file:// URI to a filesystem path.
func rootURIToPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	return filepath.Clean(u.Path)
}

// lazyGraph wraps a Graph that is built on first access.
// This allows the MCP server to start and respond to initialize/tools/list
// before the potentially slow schema detection + ingestion completes.
type lazyGraph struct {
	args      []string
	basePath  string // optional; defaults to "." (CWD) when empty
	once      sync.Once
	embedOnce sync.Once // triggers embedding on first successful get()
	inner     graph.Graph
	cleanup   func()
	err       error
}

// resolvedBasePath returns basePath if set, otherwise ".".
func (lg *lazyGraph) resolvedBasePath() string {
	if lg.basePath != "" {
		return lg.basePath
	}
	return "."
}

func (lg *lazyGraph) init() {
	lg.once.Do(func() {
		var dataSource string
		var schema *api.Topology
		base := lg.resolvedBasePath()

		if len(lg.args) == 0 {
			cfg, err := loadProjectConfig(base)
			if err != nil {
				if !os.IsNotExist(err) {
					lg.err = err
					return
				}
				log.Printf("No %s found; auto-detecting project languages...", ConfigFileName)
				dataSource = base
				schema, err = inferDirSchema(base)
				if err != nil {
					lg.err = fmt.Errorf("auto-detect schema: %w", err)
					return
				}
			} else {
				if len(cfg.Sources) > 1 {
					log.Printf("Warning: %s has %d sources but serve only uses the first; additional sources ignored", ConfigFileName, len(cfg.Sources))
				}
				src := cfg.Sources[0]
				dataSource, err = resolveDataSource(src.Path, base)
				if err != nil {
					lg.err = fmt.Errorf("resolve data source: %w", err)
					return
				}
				schema, err = resolveSchema(src.Schema, base)
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
				resolved, err := resolveSchema(serveSchema, base)
				if err != nil {
					lg.err = fmt.Errorf("resolve schema: %w", err)
					return
				}
				schema = resolved
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
	// Trigger embedding after first successful graph access.
	// This ensures SQLiteGraph's lazy scan has completed before we walk nodes.
	lg.embedOnce.Do(func() {
		go leyline.TriggerEmbedding(lg.inner, 100)
	})
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

func (lg *lazyGraph) UpdateNodeContent(id string, data []byte, origin *graph.SourceOrigin, modTime time.Time) error {
	g, err := lg.get()
	if err != nil {
		return err
	}
	if wb, ok := g.(writeBacker); ok {
		return wb.UpdateNodeContent(id, data, origin, modTime)
	}
	return fmt.Errorf("backend does not support write-back")
}

func (lg *lazyGraph) ShiftOrigins(filePath string, afterByte uint32, delta int32) {
	g, _ := lg.get()
	if g != nil {
		if wb, ok := g.(writeBacker); ok {
			wb.ShiftOrigins(filePath, afterByte, delta)
		}
	}
}

// buildServeGraph constructs a read-only Graph from the data source.
// Returns the graph, a cleanup function, and any error.
// cloneRepo clones a git repo to a temp directory for ephemeral serving.
// Returns the temp dir path and a cleanup function that removes it.
// Uses shallow clone (depth=1) for speed.
func cloneRepo(repoURL string) (string, func(), error) {
	tmpDir, err := os.MkdirTemp("", "mache-ephemeral-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temp dir: %w", err)
	}
	cleanup := func() {
		log.Printf("ephemeral cleanup: removing %s", tmpDir)
		_ = os.RemoveAll(tmpDir)
	}

	log.Printf("cloning %s (shallow)...", repoURL)
	cmd := exec.Command("git", "clone", "--depth=1", "--single-branch", repoURL, tmpDir)
	cmd.Stdout = os.Stderr // show progress on stderr (not MCP stdout)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("git clone: %w", err)
	}
	log.Printf("cloned to %s", tmpDir)

	return tmpDir, cleanup, nil
}

func buildServeGraph(dataSource string, schema *api.Topology) (graph.Graph, func(), error) {
	noop := func() {}

	if filepath.Ext(dataSource) == ".db" {
		// Materialize virtual nodes (callers, callees, content sources)
		// into the .db before opening it as a graph.
		if err := materializeVirtuals(dataSource, schema, false); err != nil {
			return nil, noop, fmt.Errorf("materialize virtuals: %w", err)
		}
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

// writeBacker is the subset of Graph backends that support surgical write-back
// (validate → format → splice → update node).
type writeBacker interface {
	UpdateNodeContent(id string, data []byte, origin *graph.SourceOrigin, modTime time.Time) error
	ShiftOrigins(filePath string, afterByte uint32, delta int32)
}

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
			results := make([]fileResult, 0, len(paths))
			for _, p := range paths {
				r, err := readOneFileWithOrigin(g, p)
				if err != nil {
					results = append(results, fileResult{Path: p, Error: err.Error()})
				} else {
					results = append(results, fileResult{Path: p, Content: r.Content, Origin: r.Origin})
				}
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
				"semantic search requires ley-line daemon (not found). Start with: leyline daemon --embed",
			), nil
		}

		sock, err := leyline.DialSocket(sockPath)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("connect to ley-line: %v", err)), nil
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

func makeGetTypeInfoHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		symbol := request.GetString("symbol", "")
		if symbol == "" {
			return mcp.NewToolResultError("symbol is required"), nil
		}

		qg := g.(refsQuerier)

		// Check if _lsp_hover table exists
		rows, err := qg.QueryRefs(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='_lsp_hover'`)
		if err != nil {
			return mcp.NewToolResultError("LSP data not available — run `leyline lsp` to enrich the database"), nil
		}
		var tableExists int
		if rows.Next() {
			_ = rows.Scan(&tableExists)
		}
		_ = rows.Close()
		if tableExists == 0 {
			// Auto-enrichment: if file param is provided, trigger LSP via ley-line daemon
			filePath := request.GetString("file", "")
			if filePath == "" {
				return mcp.NewToolResultError("no _lsp_hover table — pass 'file' param to auto-enrich or run `leyline lsp`"), nil
			}

			result, err := enrichAndQueryTypeInfo(filePath, symbol)
			if err != nil {
				log.Printf("LSP auto-enrichment failed: %v", err)
				return mcp.NewToolResultError(fmt.Sprintf("no _lsp_hover table — auto-enrichment failed: %v", err)), nil
			}
			return result, nil
		}

		// LSP tables exist in mache's graph — query directly
		return queryTypeInfoFromGraph(qg, symbol)
	}
}

func makeGetDiagnosticsHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		symbol := request.GetString("symbol", "")
		limit := request.GetInt("limit", 50)

		qg := g.(refsQuerier)

		// Check if _lsp table exists
		rows, err := qg.QueryRefs(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='_lsp'`)
		if err != nil {
			return mcp.NewToolResultError("LSP data not available — run `leyline lsp` to enrich the database"), nil
		}
		var tableExists int
		if rows.Next() {
			_ = rows.Scan(&tableExists)
		}
		_ = rows.Close()
		if tableExists == 0 {
			filePath := request.GetString("file", "")
			if filePath == "" {
				return mcp.NewToolResultError("no _lsp table — pass 'file' param to auto-enrich or run `leyline lsp`"), nil
			}

			result, err := enrichAndQueryDiagnostics(filePath, symbol, limit)
			if err != nil {
				log.Printf("LSP auto-enrichment failed: %v", err)
				return mcp.NewToolResultError(fmt.Sprintf("no _lsp table — auto-enrichment failed: %v", err)), nil
			}
			return result, nil
		}

		return queryDiagnosticsFromGraph(qg, symbol, limit)
	}
}

// enrichAndQueryTypeInfo triggers LSP enrichment and queries hover data
// directly from the ley-line daemon's arena via UDS, bypassing mache's
// in-memory graph (which doesn't have _lsp* tables).
// Uses a single connection for both enrichment and query.
func enrichAndQueryTypeInfo(filePath, symbol string) (*mcp.CallToolResult, error) {
	sockPath, err := leyline.DiscoverOrStart()
	if err != nil {
		return nil, fmt.Errorf("discover/start leyline: %w", err)
	}
	client, err := leyline.DialSocket(sockPath)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = client.Close() }()

	// Enrichment phase — 30s timeout for LSP
	if err := client.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}
	resp, err := client.Tool("lsp", map[string]any{"file": filePath})
	if err != nil {
		return nil, err
	}
	log.Printf("LSP enrichment via ley-line daemon: %v", resp)

	// Query phase — reuse same connection, reset deadline
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return nil, fmt.Errorf("set query deadline: %w", err)
	}

	sanitized := strings.ReplaceAll(symbol, "'", "''")
	rows, err := client.Query(fmt.Sprintf(
		`SELECT node_id, hover_text FROM _lsp_hover WHERE node_id LIKE '%%%s'`, sanitized))
	if err != nil {
		return nil, fmt.Errorf("query _lsp_hover via daemon: %w", err)
	}

	type hoverResult struct {
		NodeID    string `json:"node_id"`
		HoverText string `json:"hover_text"`
	}

	var results []hoverResult
	for _, row := range rows {
		if len(row) >= 2 {
			results = append(results, hoverResult{
				NodeID:    fmt.Sprint(row[0]),
				HoverText: fmt.Sprint(row[1]),
			})
		}
	}

	// Fallback: broader match
	if len(results) == 0 {
		rows, err = client.Query(fmt.Sprintf(
			`SELECT node_id, hover_text FROM _lsp_hover WHERE node_id LIKE '%%%s%%'`, sanitized))
		if err == nil {
			for _, row := range rows {
				if len(row) >= 2 {
					results = append(results, hoverResult{
						NodeID:    fmt.Sprint(row[0]),
						HoverText: fmt.Sprint(row[1]),
					})
				}
			}
		}
	}

	if len(results) == 0 {
		return mcp.NewToolResultText(fmt.Sprintf("LSP enrichment completed but no hover info found for %q", symbol)), nil
	}

	data, _ := json.MarshalIndent(results, "", "  ")
	return mcp.NewToolResultText(string(data)), nil
}

// queryTypeInfoFromGraph queries _lsp_hover from mache's in-memory graph
// (used when LSP tables already exist in the graph, e.g. from leyline lsp CLI).
func queryTypeInfoFromGraph(qg refsQuerier, symbol string) (*mcp.CallToolResult, error) {
	type hoverResult struct {
		NodeID    string `json:"node_id"`
		HoverText string `json:"hover_text"`
	}

	rows, err := qg.QueryRefs(
		`SELECT node_id, hover_text FROM _lsp_hover WHERE node_id LIKE ?`,
		"%/"+symbol,
	)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("query _lsp_hover: %v", err)), nil
	}

	var results []hoverResult
	for rows.Next() {
		var r hoverResult
		if err := rows.Scan(&r.NodeID, &r.HoverText); err != nil {
			continue
		}
		results = append(results, r)
	}
	_ = rows.Close()

	if len(results) == 0 {
		rows, err = qg.QueryRefs(
			`SELECT node_id, hover_text FROM _lsp_hover WHERE node_id LIKE ?`,
			"%"+symbol+"%",
		)
		if err == nil {
			for rows.Next() {
				var r hoverResult
				if err := rows.Scan(&r.NodeID, &r.HoverText); err != nil {
					continue
				}
				results = append(results, r)
			}
			_ = rows.Close()
		}
	}

	if len(results) == 0 {
		return mcp.NewToolResultText(fmt.Sprintf("no type info found for %q", symbol)), nil
	}

	data, _ := json.MarshalIndent(results, "", "  ")
	return mcp.NewToolResultText(string(data)), nil
}

// enrichAndQueryDiagnostics triggers LSP enrichment and queries diagnostics
// directly from the ley-line daemon's arena via UDS.
// Uses a single connection for both enrichment and query.
func enrichAndQueryDiagnostics(filePath, symbol string, limit int) (*mcp.CallToolResult, error) {
	sockPath, err := leyline.DiscoverOrStart()
	if err != nil {
		return nil, fmt.Errorf("discover/start leyline: %w", err)
	}
	client, err := leyline.DialSocket(sockPath)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = client.Close() }()

	if err := client.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}
	resp, err := client.Tool("lsp", map[string]any{"file": filePath})
	if err != nil {
		return nil, err
	}
	log.Printf("LSP enrichment via ley-line daemon: %v", resp)

	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return nil, fmt.Errorf("set query deadline: %w", err)
	}

	var query string
	if symbol != "" {
		sanitized := strings.ReplaceAll(symbol, "'", "''")
		query = fmt.Sprintf(
			`SELECT node_id, symbol_kind, diagnostics FROM _lsp WHERE diagnostics IS NOT NULL AND diagnostics != '' AND node_id LIKE '%%%s%%' LIMIT %d`,
			sanitized, limit)
	} else {
		query = fmt.Sprintf(
			`SELECT node_id, symbol_kind, diagnostics FROM _lsp WHERE diagnostics IS NOT NULL AND diagnostics != '' LIMIT %d`,
			limit)
	}

	rows, err := client.Query(query)
	if err != nil {
		return nil, fmt.Errorf("query _lsp via daemon: %w", err)
	}

	type diagResult struct {
		NodeID      string `json:"node_id"`
		SymbolKind  string `json:"symbol_kind"`
		Diagnostics string `json:"diagnostics"`
	}

	var results []diagResult
	for _, row := range rows {
		if len(row) >= 3 {
			results = append(results, diagResult{
				NodeID:      fmt.Sprint(row[0]),
				SymbolKind:  fmt.Sprint(row[1]),
				Diagnostics: fmt.Sprint(row[2]),
			})
		}
	}

	if len(results) == 0 {
		if symbol != "" {
			return mcp.NewToolResultText(fmt.Sprintf("no diagnostics found for %q", symbol)), nil
		}
		return mcp.NewToolResultText("no diagnostics found"), nil
	}

	data, _ := json.MarshalIndent(results, "", "  ")
	return mcp.NewToolResultText(string(data)), nil
}

// queryDiagnosticsFromGraph queries _lsp from mache's in-memory graph.
func queryDiagnosticsFromGraph(qg refsQuerier, symbol string, limit int) (*mcp.CallToolResult, error) {
	type diagResult struct {
		NodeID      string `json:"node_id"`
		SymbolKind  string `json:"symbol_kind"`
		Diagnostics string `json:"diagnostics"`
	}

	var query string
	var args []any
	if symbol != "" {
		query = `SELECT node_id, symbol_kind, diagnostics FROM _lsp WHERE diagnostics IS NOT NULL AND diagnostics != '' AND node_id LIKE ? LIMIT ?`
		args = []any{"%" + symbol + "%", limit}
	} else {
		query = `SELECT node_id, symbol_kind, diagnostics FROM _lsp WHERE diagnostics IS NOT NULL AND diagnostics != '' LIMIT ?`
		args = []any{limit}
	}

	rows, err := qg.QueryRefs(query, args...)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("query _lsp: %v", err)), nil
	}

	var results []diagResult
	for rows.Next() {
		var r diagResult
		if err := rows.Scan(&r.NodeID, &r.SymbolKind, &r.Diagnostics); err != nil {
			continue
		}
		results = append(results, r)
	}
	_ = rows.Close()

	if len(results) == 0 {
		if symbol != "" {
			return mcp.NewToolResultText(fmt.Sprintf("no diagnostics found for %q", symbol)), nil
		}
		return mcp.NewToolResultText("no diagnostics found"), nil
	}

	data, _ := json.MarshalIndent(results, "", "  ")
	return mcp.NewToolResultText(string(data)), nil
}

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
