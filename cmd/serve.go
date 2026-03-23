package cmd

import (
	"context"
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

	mcpHandler := server.NewStreamableHTTPServer(s,
		server.WithHTTPContextFunc(hostedContextFromRequest),
	)

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpHandler)
	mux.HandleFunc("/", serveLandingPage)

	log.Printf("mache MCP server listening on %s/mcp (Streamable HTTP)", serveHTTP)
	httpSrv := &http.Server{Addr: serveHTTP, Handler: mux}
	return httpSrv.ListenAndServe()
}

// landingPagePath is where rig's Dockerfile injects the landing page HTML.
const landingPagePath = "/app/static/mache-landing.html"

// serveLandingPage serves the rig-managed HTML landing page if available,
// falling back to plain text with the connect URL.
func serveLandingPage(w http.ResponseWriter, r *http.Request) {
	if data, err := os.ReadFile(landingPagePath); err == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
		return
	}
	// Fallback: plain text with connect instructions
	scheme := requestScheme(r)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = fmt.Fprintln(w, "mache MCP server")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintf(w, "Connect: claude mcp add --transport http mache \"%s://%s/mcp?repo=<your-repo-url>\"\n", scheme, r.Host)
}

// requestScheme returns "https" if behind a TLS-terminating proxy, else "http".
func requestScheme(r *http.Request) string {
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		return proto
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
// Returns the graph, a cleanup function, and any error.
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

	// Start file watcher for incremental re-index if source is a directory.
	var fw *ingest.Watcher
	if info, statErr := os.Stat(dataSource); statErr == nil && info.IsDir() {
		onChange := func(path string) {
			store.DeleteFileNodes(path)
			if reErr := engine.ReIngestFile(path); reErr != nil {
				log.Printf("watcher: re-ingest %s: %v", path, reErr)
			} else {
				log.Printf("watcher: re-indexed %s", path)
			}
		}
		onDelete := func(path string) {
			store.DeleteFileNodes(path)
			log.Printf("watcher: deleted nodes for %s", path)
		}
		var watchErr error
		fw, watchErr = ingest.NewWatcher(dataSource, onChange, onDelete)
		if watchErr != nil {
			log.Printf("Warning: file watcher failed to start: %v", watchErr)
		} else {
			log.Printf("file watcher started on %s", dataSource)
		}
	}

	return store, func() {
		if fw != nil {
			fw.Stop()
		}
		_ = store.Close()
		resolver.Close()
	}, nil
}
