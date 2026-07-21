package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/leyline"
	machetmpl "github.com/agentic-research/mache/internal/template"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
)

var serveCmd = &cobra.Command{
	Use:   "serve [data-source]",
	Short: "Serve a Mache graph as an MCP server",
	Long: `Starts an MCP (Model Context Protocol) server that exposes the graph
as tools.

Streamable HTTP is the canonical transport (default, localhost:7532): one
shared daemon serves every project, routing per session via the MCP roots
protocol. 'mache init' registers clients against it. See ADR-0022.

--stdio is an explicit escape hatch for CI, sandboxes, and headless/cron
agents where no shared daemon should run. It is NOT registered by 'mache init'
and should not be used for interactive editor setups.

Examples:
  mache serve ./data.db                  # HTTP on localhost:7532 (canonical)
  mache serve --http :9000 ./data.db     # HTTP on custom port (all interfaces)
  mache serve --stdio ./data.db          # stdio escape hatch (CI / sandbox)
  claude mcp add --transport http mache http://localhost:7532/mcp`,
	Args: cobra.MaximumNArgs(1),
	RunE: runServe,
}

var (
	serveSchema  string
	serveHTTP    string
	serveStdio   bool
	servePath    string
	serveRepo    string
	serveControl string
	serveMounts  []string
)

func init() {
	serveCmd.Flags().StringVarP(&serveSchema, "schema", "s", "", "Path to topology schema")
	serveCmd.Flags().StringVar(&serveHTTP, "http", "localhost:7532", "Listen address for Streamable HTTP transport")
	serveCmd.Flags().BoolVar(&serveStdio, "stdio", false, "Escape-hatch stdio transport for CI/sandbox/headless use (HTTP is canonical; never registered by `mache init`)")
	serveCmd.Flags().StringVar(&servePath, "path", "", "Base directory for project detection (defaults to current working directory)")
	serveCmd.Flags().StringVar(&serveRepo, "repo", "", "Git repo URL to clone and serve (ephemeral: cleaned up on exit)")
	serveCmd.Flags().StringVar(&serveControl, "control", "", "Path to ley-line control block (reads from arena, enables hot-swap)")
	serveCmd.Flags().StringArrayVar(&serveMounts, "mount", nil,
		"Mount a graph at NAME=PATH; repeatable. Each PATH is loaded via the same path-or-.db resolution as the positional source. NAME becomes a top-level virtual directory. Cross-repo find_callers federates across all mounts. Composes with --schema (applied to every mount).")
	rootCmd.AddCommand(serveCmd)
}

func runServe(cmd *cobra.Command, args []string) error {
	// When spawned by the daemon (--control), auto-assign port if --http wasn't
	// explicitly set. Avoids "address already in use" when port 7532 is taken.
	if serveControl != "" && !cmd.Flags().Changed("http") {
		serveHTTP = "localhost:0"
	}

	var repoCloneDir string // set only for HTTP + --repo mode

	// Ephemeral mode: clone repo to temp dir, serve from there, cleanup on exit
	if serveRepo != "" {
		if serveStdio {
			// Stdio: single session — clone directly, use as basePath
			tmpDir, cleanup, err := cloneRepo(serveRepo)
			if err != nil {
				return fmt.Errorf("clone %s: %w", serveRepo, err)
			}
			defer cleanup()
			servePath = tmpDir
			log.Printf("ephemeral stdio mode: serving %s from %s", serveRepo, tmpDir)
		} else {
			// HTTP: multiple sessions — clone into base/ subdir so sessions/
			// is a sibling under the same parent (all cleaned up together).
			parentDir, err := os.MkdirTemp("", "mache-repo-*")
			if err != nil {
				return fmt.Errorf("create temp dir: %w", err)
			}
			defer func() {
				log.Printf("ephemeral cleanup: removing %s", parentDir)
				_ = os.RemoveAll(parentDir)
			}()
			baseDir := filepath.Join(parentDir, "base")
			log.Printf("cloning %s for HTTP mode...", serveRepo)
			cmd := exec.Command("git", "clone", "--depth=1", "--single-branch", serveRepo, baseDir)
			cmd.Stdout = os.Stderr
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("git clone: %w", err)
			}
			repoCloneDir = baseDir
			// Set basePath to base clone as fallback (not CWD)
			servePath = baseDir
			log.Printf("ephemeral HTTP mode: base clone at %s", baseDir)
		}
	}

	registry := newGraphRegistry(servePath, args)
	defer registry.Close()
	registry.repoCloneDir = repoCloneDir

	// Resolve the external smell-rules dir ONCE at startup from
	// $MACHE_SMELL_RULES_DIR or the served project's .mache.json
	// (serve has no explicit flag for it). The find_smells handler
	// rescans this fixed dir per request, so dropping a new rule file
	// into it is picked up live — no restart, no reconnect. Changing
	// the dir itself still needs a restart, which is fine.
	registry.smellRulesDir = resolveSmellRulesDir("", registry.resolvedBasePath())
	if registry.smellRulesDir != "" {
		log.Printf("find_smells: external rules dir = %s (rescanned per request)", registry.smellRulesDir)
	}

	// Configure the leyline daemon's --source so live LSP/embed enrichment
	// (get_type_info / get_diagnostics with file=) can run: op_enrich
	// requires the daemon to know the source tree (mache-303036). Only set
	// when serving a source directory; a pre-baked .db carries its own
	// _lsp* tables and needs no live enrichment.
	if src := servedSourceDir(args, servePath, serveRepo != ""); src != "" {
		leyline.SetDaemonSource(src)
	}

	// Startup wire-compat handshake (mache-8kif): if a leyline daemon is
	// already reachable, refuse to serve on a structural wire-format mismatch
	// rather than decoding garbage on the first enrichment op. No-op when no
	// daemon is running or it predates the leyline_version op.
	if err := leyline.VerifyReachableDaemonVersion(); err != nil {
		return err
	}

	// Start the sheaf-invalidate event subscriber (mache-c14c43).
	// One subscriber per process feeds the router; the router fans
	// events out to every lazyGraph's invalidator. When no daemon
	// socket is reachable at startup, startSheafSubscriber logs and
	// returns nil + a no-op stop — local watcher invalidation still
	// works, just without notifications from other initiators.
	// registry.Close stops it during shutdown.
	subscriberCtx, stopSubscriberCtx := context.WithCancel(context.Background()) // coverage:ignore — serve startup wiring; reduction tracked in mache-89b5dd.
	defer stopSubscriberCtx()                                                    // coverage:ignore — serve startup wiring; reduction tracked in mache-89b5dd.
	sub, stop := startSheafSubscriber(subscriberCtx, registry.sheafRouter)       // coverage:ignore — serve startup wiring; reduction tracked in mache-89b5dd.
	registry.sheafSubscriber = sub                                               // coverage:ignore — serve startup wiring; reduction tracked in mache-89b5dd.
	registry.stopSheafSubscriber = func() {                                      // coverage:ignore — serve startup wiring; reduction tracked in mache-89b5dd.
		stopSubscriberCtx() // coverage:ignore — serve startup wiring; reduction tracked in mache-89b5dd.
		stop()              // coverage:ignore — serve startup wiring; reduction tracked in mache-89b5dd.
	} // coverage:ignore — serve startup wiring; reduction tracked in mache-89b5dd.

	// Clean up session → root mapping on disconnect.
	// Root discovery happens lazily on the first tool call (see wrapHandler)
	// because ListRoots deadlocks inside OnAfterInitialize — the client
	// can't respond until the initialize response is sent.
	hooks := &server.Hooks{}
	hooks.AddOnUnregisterSession(func(_ context.Context, session server.ClientSession) {
		registry.unregisterSession(session.SessionID())
		// Clean up worktree if in repo HTTP mode
		registry.cleanupRepoSession(session.SessionID())
		log.Printf("session %s unregistered", session.SessionID())
	})

	// Create MCP server IMMEDIATELY — respond to health checks fast
	s := server.NewMCPServer("mache", Version,
		server.WithToolCapabilities(false),
		server.WithHooks(hooks),
		server.WithInstructions(`Mache provides structural code intelligence tools. Use mache when you need to:
- Explore unfamiliar codebases (get_overview, list_directory, read_file)
- Understand architecture and key abstractions (get_architecture)
- Find where symbols are defined or used (find_definition, find_callers, find_callees)
- Search for code by pattern (search)
- Understand code structure and communities (get_communities)
- Visualize system structure as a mermaid diagram (get_diagram)
- Get type information and diagnostics from LSP (get_type_info, get_diagnostics)
- Analyze change blast radius (get_impact)
Call get_overview first when exploring a new codebase, then get_architecture for deeper orientation.`),
	)
	registerMCPTools(s, registry)

	// Resolve source label for sidecar metadata
	source := registry.resolvedBasePath()
	if len(args) > 0 {
		source = args[0]
	}

	// Clean up any auto-spawned leyline daemon on exit.
	defer leyline.StopManaged()

	if serveStdio {
		// Stdio: hard exit on signal — no HTTP server to drain.
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-sigCh
			leyline.StopManaged()
			registry.Close()
			os.Exit(0)
		}()

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

	mcpHandler := server.NewStreamableHTTPServer(s,
		server.WithHTTPContextFunc(hostedContextFromRequest),
	)

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpHandler)
	mux.HandleFunc("/", serveLandingPage)

	httpSrv := &http.Server{Addr: serveHTTP, Handler: mux}

	// HTTP: graceful shutdown drains in-flight requests so defers
	// (registry.Close, StopManaged, removeServeSidecar) run normally.
	// signal.NotifyContext auto-cleans up if ListenAndServe returns for
	// a non-signal reason (bind error, etc.) — no leaked goroutine.
	sigCtx, sigStop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer sigStop()
	go func() {
		<-sigCtx.Done()
		log.Println("shutting down…")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutCtx); err != nil {
			log.Printf("HTTP shutdown: %v", err)
		}
	}()

	log.Printf("mache MCP server listening on %s/mcp (Streamable HTTP)", serveHTTP)
	if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// landingPagePath is where rig's Dockerfile injects the landing page HTML.
// Override via MACHE_LANDING_PAGE env var for custom deployments.
var landingPagePath = defaultLandingPagePath()

func defaultLandingPagePath() string {
	if v := os.Getenv("MACHE_LANDING_PAGE"); v != "" {
		return v
	}
	return "/app/static/mache-landing.html"
}

// landingPageCacheControl is the Cache-Control header sent with the landing
// page response. The HTML file is static at deploy time; a 5-minute cache
// is plenty for a status page and reduces disk reads on every "/" hit.
const landingPageCacheControl = "public, max-age=300"

// serveLandingPage serves the rig-managed HTML landing page if available,
// falling back to plain text with the connect URL. Bead mache-ef3de2:
// rejects non-GET/HEAD methods with 405 and sets Cache-Control on the
// response so a CDN or browser can cache the static page.
func serveLandingPage(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		// continue
	default:
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	data, err := os.ReadFile(landingPagePath)
	if err == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", landingPageCacheControl)
		_, _ = w.Write(data)
		return
	}
	if !errors.Is(err, os.ErrNotExist) {
		log.Printf("landing page read error: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	// Fallback: plain text with connect instructions
	scheme := requestScheme(r)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", landingPageCacheControl)
	_, _ = fmt.Fprintln(w, "mache MCP server")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintf(w, "Connect: claude mcp add --transport http mache \"%s://%s/mcp?repo=<your-repo-url>\"\n", scheme, r.Host)
}

// requestScheme returns "https" if behind a TLS-terminating proxy, else "http".
func requestScheme(r *http.Request) string {
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		switch strings.ToLower(proto) {
		case "https":
			return "https"
		case "http":
			return "http"
		}
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
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

// buildServeGraph constructs a read-only Graph from the data source.
// Returns the graph plus a *graph.SheafInvalidator wired into the file
// watcher's onChange path. The invalidator is non-nil iff this build
// constructed a watcher (i.e. dataSource is a directory) — file-shaped
// sources, .db sources, and control-mode init all return a nil
// invalidator since there's no watcher to fire it.
//
// The invalidator is constructed with sheaf=nil and result=nil. The
// MCP get_communities handler swaps in both via SetCommunityResult /
// SetSheaf after running community detection + dialing the daemon.
// Pre-swap, the watcher's cascade attempts fall back to single-node
// Graph.Invalidate — correct behavior, just no cross-region propagation
// until the moat is engaged.
// Returns the graph, a cleanup function, and any error.
func buildServeGraph(dataSource string, schema *api.Topology) (graph.Graph, *graph.SheafInvalidator, func(), error) {
	noop := func() {}

	// Control block path: read from ley-line arena with hot-swap.
	// The daemon produces the .db and loads it into the arena.
	// Mache reads the active buffer and swaps on generation bump.
	if serveControl != "" {
		return buildControlGraph(serveControl, schema)
	}

	if filepath.Ext(dataSource) == ".db" {
		return openDBGraph(dataSource, schema, noop)
	}

	// Auto-invoke leyline: when the source is a directory and `leyline` is
	// available, run `leyline parse <dir> -o <tmp.db>` and open the result.
	// This eliminates CGO tree-sitter at mount time. Falls back silently to
	// the in-process Engine + SitterWalker path on any failure.
	// Disable via MACHE_NO_LEYLINE=1.
	//
	// KNOWN GAP (PR #383 Copilot #4 → bead mache-6c9e1d): this path returns
	// a NIL *SheafInvalidator because openDBGraph's .db backend is frozen
	// at parse time — no watcher is constructed, so the file-watcher →
	// sheaf cascade wiring is disabled in the default leyline-installed
	// setup. Only MACHE_NO_LEYLINE=1 (in-process MemoryStore path below)
	// currently exercises the cascade.
	//
	// Closing this requires either (a) flipping auto-leyline from one-
	// shot `leyline parse` to a managed daemon that watches the source
	// dir + reparses + emits sheaf.invalidate events that mache subscribes
	// to (depends on mache-c14c43), or (b) building the cascade-eligible
	// in-process store alongside the .db (wasteful double-ingest). Tracked
	// for the followup; the current behavior is honest documented
	// degradation — agents using auto-leyline get fast cold-start but
	// stale results after edits until the daemon-mode rewrite ships.
	if info, statErr := os.Stat(dataSource); statErr == nil && info.IsDir() &&
		os.Getenv("MACHE_NO_LEYLINE") == "" {
		if dbPath, dbCleanup, err := autoInvokeLeylineParse(dataSource); err == nil {
			g, si, gCleanup, gerr := openDBGraph(dbPath, schema, dbCleanup)
			if gerr == nil {
				log.Printf("auto-leyline: serving from %s (frozen .db — sheaf cascade NOT engaged for live edits; set MACHE_NO_LEYLINE=1 for the in-process watcher path)", filepath.Base(dbPath))
				return g, si, gCleanup, nil
			}
			log.Printf("auto-leyline: opened .db failed (%v); falling back to in-process ingest", gerr)
			dbCleanup()
		} else {
			log.Printf("auto-leyline: skipping (%v); using in-process ingest", err)
		}
	}

	// MemoryStore path for JSON/source files. This is also the live-edit
	// watcher + SheafInvalidator path (preserved by ADR-0012 step 4 / Option A).
	store := graph.NewMemoryStore()
	resolver := graph.NewSQLiteResolver(machetmpl.Render)
	store.SetResolver(resolver.Resolve)

	engine := ingest.NewEngine(schema, store)

	// Source (tree-sitter S-expression) schemas: ley-line parses source into
	// an `_ast` db and the engine projects it via ASTWalker — in-process CGO
	// tree-sitter was removed (ADR-0012 step 4). The `_ast` db stays open for
	// the store's lifetime so the file-watcher's ReIngestFile can re-project
	// edited files. KNOWN GAP: re-ingest reads the FROZEN `_ast` until the
	// next parse — the same freshness limitation documented for the
	// auto-leyline serve path above; the watcher still fires the sheaf
	// cascade correctly. ley-line is MANDATORY here (guardrail 2): if it
	// can't be resolved this is a hard error, not a silent empty graph.
	astCleanup := noop
	if ingest.SchemaUsesTreeSitter(schema) {
		astDB, cleanup, ppErr := attachLeylineASTWalker(dataSource, engine)
		if ppErr != nil {
			resolver.Close()
			return nil, nil, noop, fmt.Errorf("ley-line parse for source projection: %w", ppErr)
		}
		astCleanup = cleanup
		store.SetCallExtractor(newASTCallExtractor(astDB))
		store.SetScopedCallExtractor(newASTScopedCallExtractor(astDB))
	}

	if err := engine.Ingest(dataSource); err != nil {
		resolver.Close()
		astCleanup()
		return nil, nil, noop, fmt.Errorf("ingestion: %w", err)
	}

	if err := store.InitRefsDB(); err != nil {
		resolver.Close()
		astCleanup()
		return nil, nil, noop, fmt.Errorf("init refs db: %w", err)
	}
	if err := store.FlushRefs(); err != nil {
		log.Printf("Warning: refs flush: %v", err)
	}

	// Start file watcher for incremental re-index if source is a directory.
	// The SheafInvalidator is constructed *only* on this path — it has no
	// purpose without a watcher to fire it. cmd/serve_handlers.go's
	// get_communities handler later calls si.SetCommunityResult + SetSheaf
	// to engage the cross-region cascade; until then the watcher's
	// invalidate calls fall back to single-node Graph.Invalidate.
	var fw *ingest.Watcher
	var si *graph.SheafInvalidator
	if info, statErr := os.Stat(dataSource); statErr == nil && info.IsDir() {
		si = graph.NewSheafInvalidator(store, nil, nil)
		onChange := func(path string) {
			// Snapshot PRE-edit node IDs before DeleteFileNodes wipes
			// the path↔nodes bitmap. The new IDs (post-reingest) may
			// differ if the edit renames/moves/removes constructs —
			// without including the old IDs, the cascade misses
			// regions that the old constructs occupied (PR #383
			// Copilot #3). Union of old + new covers both: regions
			// containing removed-or-renamed nodes get invalidated
			// via the pre-edit IDs; regions containing the new
			// construct names get invalidated via the post-edit IDs.
			oldIDs := store.NodesForPath(path)
			store.DeleteFileNodes(path)
			if reErr := engine.ReIngestFile(path); reErr != nil {
				log.Printf("watcher: re-ingest %s: %v", path, reErr)
				// Still cascade the old IDs — the file's prior
				// state is no longer valid downstream regardless
				// of whether the re-ingest succeeded.
				si.InvalidateNodesWithCascade(oldIDs)
				return
			}
			log.Printf("watcher: re-indexed %s", path)
			newIDs := store.NodesForPath(path)
			affected := unionStringSlices(oldIDs, newIDs)
			// One daemon round-trip per unique region (dedupe in
			// InvalidateNodesWithCascade). Pre-install (no sheaf,
			// no result) this still degrades to single-node
			// invalidates — same intentional graceful behavior.
			si.InvalidateNodesWithCascade(affected)
		}
		onDelete := func(path string) {
			// Snapshot affected nodes BEFORE DeleteFileNodes wipes the
			// path↔nodes bitmap — otherwise NodesForPath returns empty
			// and the cascade has nothing to invalidate. The post-delete
			// graph.Invalidate path is correct for local caches; the
			// cascade is what propagates the boundary change daemon-side.
			affected := store.NodesForPath(path)
			store.DeleteFileNodes(path)
			log.Printf("watcher: deleted nodes for %s", path)
			si.InvalidateNodesWithCascade(affected)
		}
		var watchErr error
		fw, watchErr = ingest.NewWatcher(dataSource, onChange, onDelete,
			ingest.WithGitignore(engine.Gitignore()))
		if watchErr != nil {
			log.Printf("Warning: file watcher failed to start: %v", watchErr)
		} else {
			log.Printf("file watcher started on %s", dataSource)
		}
	}

	return store, si, func() {
		if fw != nil {
			fw.Stop()
		}
		_ = store.Close()
		resolver.Close()
		// Close the ley-line _ast db AFTER the watcher/store stop so any
		// in-flight ReIngestFile finishes against a live handle.
		astCleanup()
	}, nil
}

// attachLeylineASTWalker parses dataSource with ley-line into a temporary
// `_ast` db, opens it, and wires an ASTWalker onto the engine so a source
// (tree-sitter S-expression) schema projects with zero in-process CGO
// (ADR-0012 step 4). Returns the open db (for a call extractor) and a cleanup
// that closes the db and removes the temp file. ley-line is mandatory: a
// resolution failure is returned as an error (guardrail 2), never swallowed.
func attachLeylineASTWalker(dataSource string, engine *ingest.Engine) (*sql.DB, func(), error) {
	dbPath, parseCleanup, err := autoInvokeLeylineParse(dataSource)
	if err != nil {
		return nil, nil, err
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		parseCleanup()
		return nil, nil, fmt.Errorf("open ley-line parse output %s: %w", dbPath, err)
	}
	engine.SetASTWalker(ingest.NewASTWalker(db))
	cleanup := func() {
		_ = db.Close()
		parseCleanup()
	}
	return db, cleanup, nil
}

// unionStringSlices returns the deduped union of a and b. Order
// within the result is undefined. Used by the watcher's onChange to
// build the union of pre- and post-edit node IDs so the cascade
// hits regions containing both states (PR #383 Copilot #3).
func unionStringSlices(a, b []string) []string {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range a {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	for _, s := range b {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

// buildMaybeMultiGraph dispatches between single-source and composite
// (multi-mount) construction. With no --mount flags, it's a thin
// pass-through to buildServeGraph (propagating its *SheafInvalidator).
// With one or more --mount NAME=PATH flags, it builds a CompositeGraph
// by calling buildServeGraph for each mount path and Mount-ing the
// result under NAME. The composite case returns a nil *SheafInvalidator
// — per-mount watchers each have their own invalidator captured in
// their closures, and there's no semantically correct way to expose
// a single composite invalidator to the MCP handler layer (each mount
// has its own CommunityResult). Composite mounts forfeit the cascade
// in exchange for the multi-mount feature; document for the wiring PR
// that revisits this if a use case emerges.
//
// When --mount is set, the positional dataSource is rejected — the
// caller must choose between a single source and a composite.
//
// Cleanup runs in reverse-mount order so child graphs are torn down
// before any shared resources they depend on.
func buildMaybeMultiGraph(dataSource string, schema *api.Topology) (graph.Graph, *graph.SheafInvalidator, func(), error) {
	if len(serveMounts) == 0 {
		return buildServeGraph(dataSource, schema)
	}
	if dataSource != "" {
		return nil, nil, func() {}, fmt.Errorf("cannot use both a positional source (%q) and --mount; use one or the other", dataSource)
	}

	composite := graph.NewCompositeGraph()
	// Global cross-mount fallback extractor. Since ADR-0012 step 4 removed
	// in-process CGO tree-sitter, the fallback is a no-op — cross-mount
	// callees resolve through the per-mount picker below (which reads each
	// mount's `_ast`); a mount with no AST simply contributes no calls.
	composite.SetCallExtractor(noopCallExtractor())

	// Wire the per-mount picker (ADR-0012): for SQLiteGraph mounts that carry
	// `_ast`, use the pure-Go ASTWalker extractor; the picker returns nil for
	// other backends so cross-mount resolution falls through to the global
	// no-op above. Each mount picks independently — heterogeneous fleets
	// (e.g. one ll-open .db and one mache-built .db) get the right extractor.
	composite.SetCallExtractorPicker(func(local graph.Graph) graph.CallExtractor {
		sg, ok := local.(*graph.SQLiteGraph)
		if !ok {
			return nil
		}
		return pickCallExtractor(sg.DB())
	})

	var cleanups []func()
	runAll := func() {
		// Reverse order so later mounts close before earlier ones.
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}

	for _, spec := range serveMounts {
		name, path, ok := strings.Cut(spec, "=")
		if !ok || name == "" || path == "" {
			runAll()
			return nil, nil, func() {}, fmt.Errorf("invalid --mount spec %q (expected NAME=PATH)", spec)
		}
		g, _, cleanup, err := buildServeGraph(path, schema)
		if err != nil {
			runAll()
			return nil, nil, func() {}, fmt.Errorf("mount %s=%s: %w", name, path, err)
		}
		cleanups = append(cleanups, cleanup)
		if err := composite.Mount(name, g); err != nil {
			runAll()
			return nil, nil, func() {}, fmt.Errorf("mount %q: %w", name, err)
		}
		log.Printf("mounted %s -> %s", name, path)
	}
	return composite, nil, runAll, nil
}

// openDBGraph opens a .db file as a SQLiteGraph after materializing virtual
// nodes. The extra cleanup callback runs after the graph is closed (used to
// delete an auto-generated temp .db).
// openDBGraph returns (graph, nil, cleanup, err) — the *SheafInvalidator
// slot is always nil here because .db sources don't run a file watcher
// and have no need for cascade wiring (the contents are frozen at the
// time the .db was built). Callers (e.g. buildServeGraph) propagate the
// nil to their own returns.
func openDBGraph(dbPath string, schema *api.Topology, extraCleanup func()) (graph.Graph, *graph.SheafInvalidator, func(), error) {
	if extraCleanup == nil {
		extraCleanup = func() {}
	}
	if err := materializeVirtuals(dbPath, schema, false); err != nil {
		extraCleanup()
		return nil, nil, func() {}, fmt.Errorf("materialize virtuals: %w", err)
	}
	sg, err := graph.OpenSQLiteGraph(dbPath, schema, machetmpl.Render)
	if err != nil {
		extraCleanup()
		return nil, nil, func() {}, fmt.Errorf("open sqlite graph: %w", err)
	}
	sg.SetCallExtractor(pickCallExtractor(sg.DB()))
	sg.SetScopedCallExtractor(pickScopedCallExtractor(sg.DB()))
	if err := sg.EagerScan(); err != nil {
		_ = sg.Close()
		extraCleanup()
		return nil, nil, func() {}, fmt.Errorf("scan: %w", err)
	}
	return sg, nil, func() {
		_ = sg.Close()
		extraCleanup()
	}, nil
}

// servedSourceDir returns the absolute source-tree directory mache is
// serving, for configuring the leyline daemon's --source. It prefers a
// positional argument that is an existing directory; for --repo mode (no
// positional source) the clone at basePath is the tree. Returns "" when
// serving a pre-baked .db — there's no live source to enrich, and the .db
// carries its own _lsp* tables.
func servedSourceDir(args []string, basePath string, repoMode bool) string {
	for _, a := range args {
		if fi, err := os.Stat(a); err == nil && fi.IsDir() {
			if abs, err := filepath.Abs(a); err == nil {
				return abs
			}
		}
	}
	if repoMode {
		if abs, err := filepath.Abs(basePath); err == nil {
			return abs
		}
	}
	return ""
}

// autoInvokeLeylineParse runs `leyline parse <sourceDir> -o <tmpdb>` and
// returns the path to the produced .db plus a cleanup function that removes
// it. Returns an error if leyline is not available on PATH or in the bundled
// location, or if parsing fails. The caller should fall back to the
// in-process ingest path on any error.
func autoInvokeLeylineParse(sourceDir string) (string, func(), error) {
	// Resolve — and if absent, auto-download — the leyline binary via the same
	// provisioning path mache serve uses, so `mache build` no longer silently
	// degrades to tree-sitter merely because leyline isn't installed.
	// MACHE_NO_LEYLINE opts out (returns an error the auto caller treats as
	// "fall back to tree-sitter").
	leylineBin, err := leyline.ResolveBinary(true)
	if err != nil {
		return "", nil, err
	}

	tmpFile, err := os.CreateTemp("", "mache-leyline-*.db")
	if err != nil {
		return "", nil, fmt.Errorf("create temp .db: %w", err)
	}
	tmpPath := tmpFile.Name()
	_ = tmpFile.Close()

	cleanup := func() {
		_ = os.Remove(tmpPath)
		_ = os.Remove(tmpPath + "-wal")
		_ = os.Remove(tmpPath + "-shm")
	}

	log.Printf("auto-leyline: parsing %s -> %s", sourceDir, tmpPath)
	cmd := exec.Command(leylineBin, "parse", sourceDir, "-o", tmpPath)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("leyline parse: %w", err)
	}
	return tmpPath, cleanup, nil
}

// buildControlGraph connects to the ley-line daemon over UDS.
// The daemon owns SQLite (zero-copy via sqlite3_deserialize on the arena).
// Mache sends structured ops, never opens SQLite directly.
//
// Returns a nil *SheafInvalidator slot: control mode reads from an
// arena that's owned by the daemon (no mache-side watcher, no need
// for cascade wiring — the daemon's own reparse pipeline drives the
// freshness story in this mode).
func buildControlGraph(ctrlPath string, _ *api.Topology) (graph.Graph, *graph.SheafInvalidator, func(), error) {
	// The daemon socket is at <ctrl>.sock (convention)
	sockPath := ctrlPath[:len(ctrlPath)-len(".ctrl")] + ".sock"

	// Wait for socket to appear (daemon may still be starting)
	deadline := time.After(30 * time.Second)
	for {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		select {
		case <-deadline:
			return nil, nil, nil, fmt.Errorf("timed out waiting for daemon socket %s (30s)", sockPath)
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}

	g, err := newUDSGraph(sockPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("connect to daemon: %w", err)
	}
	log.Printf("Connected to ley-line daemon at %s", sockPath)

	return g, nil, func() { _ = g.Close() }, nil
}
