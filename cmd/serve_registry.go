package cmd

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/leyline"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// ---------------------------------------------------------------------------
// graphRegistry: multi-tenant session → graph routing
// ---------------------------------------------------------------------------

// graphRegistry maps MCP sessions to per-workspace graphs.
// Each session's workspace root (from ListRoots) gets its own lazily-built
// graph. Sessions without roots fall back to basePath (--path flag or CWD).
type graphRegistry struct {
	basePath      string   // --path flag default
	args          []string // positional args from command line
	graphs        sync.Map // rootPath -> *lazyGraph
	sessions      sync.Map // sessionID -> rootPath
	repoCloneDir  string   // base clone dir for --repo mode (empty otherwise)
	worktrees     sync.Map // sessionID -> worktree path (for cleanup)
	worktreeOnces sync.Map // sessionID -> *sync.Once (serialize creation)
	repoClones    sync.Map // repo URL → *repoClone (hosted mode cache)
	sessionRepos  sync.Map // sessionID → repo URL (for cleanup on disconnect)
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
	// Release hosted-mode repo clone ref if this session used one.
	if repoURL, ok := r.sessionRepos.LoadAndDelete(sessionID); ok {
		r.releaseRepoClone(repoURL.(string))
	}
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

	// Repo HTTP mode: each session gets its own worktree.
	// Short-circuit BEFORE ListRoots — in repo mode, client workspace roots
	// are irrelevant; we always serve from the cloned repo.
	if r.repoCloneDir != "" {
		wtDir, err := r.ensureRepoWorktree(sid)
		if err != nil {
			log.Printf("create worktree for session %s: %v (using base clone)", sid, err)
			r.registerSession(sid, r.repoCloneDir)
			return r.getOrCreateGraph(r.repoCloneDir)
		}
		r.registerSession(sid, wtDir)
		log.Printf("session %s → worktree %s", sid, wtDir)
		return r.getOrCreateGraph(wtDir)
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

// ---------------------------------------------------------------------------
// lazyGraph: deferred graph construction
// ---------------------------------------------------------------------------

// lazyGraph wraps a Graph that is built on first access.
// This allows the MCP server to start and respond to initialize/tools/list
// before the potentially slow schema detection + ingestion completes.
type lazyGraph struct {
	args      []string
	basePath  string // optional; defaults to "." (CWD) when empty
	once      sync.Once
	embedOnce sync.Once // triggers embedding on first successful get()
	inner     graph.Graph
	schema    *api.Topology // retained after init for schema-aware tools
	cleanup   func()
	err       error
}

// schemaProvider exposes the Topology used during graph construction.
// Handlers like get_diagram use this to resolve named diagram definitions.
type schemaProvider interface {
	Schema() *api.Topology
}

// Schema returns the schema used to build this graph, or nil if not yet initialized.
func (lg *lazyGraph) Schema() *api.Topology {
	lg.init()
	return lg.schema
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
		lg.schema = schema
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

// ---------------------------------------------------------------------------
// Interface types for optional graph backend capabilities
// ---------------------------------------------------------------------------

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
