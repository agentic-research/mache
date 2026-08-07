package cmd

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/leyline"
	"github.com/agentic-research/mache/resolve"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"golang.org/x/sync/singleflight"
)

// ---------------------------------------------------------------------------
// graphRegistry: multi-tenant session → graph routing
// ---------------------------------------------------------------------------

// graphRegistry maps MCP sessions to per-workspace graphs.
// Each session's workspace root (from ListRoots) gets its own lazily-built
// graph. Sessions without roots use an explicit basePath (--path); a shared
// daemon with no base path returns a diagnostic rather than inheriting its CWD.
type graphRegistry struct {
	basePath        string   // --path flag default
	args            []string // positional args from command line
	graphs          sync.Map // rootPath -> *lazyGraph
	sessions        sync.Map // sessionID -> rootPath
	sessionErrors   sync.Map // sessionID -> *lazyGraph when workspace root discovery failed
	repoCloneDir    string   // base clone dir for --repo mode (empty otherwise)
	smellRulesDir   string   // external smell-rules dir resolved once at startup (env/.mache.json); find_smells rescans it per request
	worktrees       sync.Map // sessionID -> worktree path (for cleanup)
	worktreeOnces   sync.Map // sessionID -> *sync.Once (serialize creation)
	repoClones      sync.Map // repo URL → *repoClone (hosted mode cache)
	sessionRepos    sync.Map // sessionID → repo URL (for cleanup on disconnect)
	sessionBaseDirs sync.Map // sessionID → base clone dir (for hosted worktree cleanup)
	hostedOnces     sync.Map // sessionID → *sync.Once (serialize hosted worktree creation)

	// sheafRouter is the process-wide router for daemon-pushed
	// sheaf.invalidate events. lazyGraph.init registers its invalidator
	// here (if non-nil); the subscriber's handler walks the snapshot
	// and dispatches to whichever invalidator's CommunityResult claims
	// the affected regions. See cmd/sheaf_subscribe.go for the
	// routing contract and mache-c14c43 for design rationale.
	sheafRouter *sheafEventRouter

	// stopSheafSubscriber halts the long-running subscriber goroutine
	// at Close(). nil when no subscriber was started (e.g. no daemon
	// socket reachable at runServe time).
	stopSheafSubscriber func()

	// sheafSubscriber is the running subscriber instance, exposed so
	// the get_sheaf_status MCP tool can surface its state (connected /
	// disconnected, last-seen generation, reason). nil when no
	// subscriber was started at runServe time.
	sheafSubscriber *leyline.SheafSubscriber
}

// resolveMountIDPrefix is the id-namespace prefix that routes a lazyGraph
// delegate call to resolveMounts instead of the base graph. Paths under it
// are only ever handed out by resolve_ref's graph_path response field, so
// callers never need to guess the prefix.
const resolveMountIDPrefix = "resolve/"

// shortHash returns a short, stable, filesystem/URL-safe identifier for s —
// used to name resolve_ref mount prefixes ("resolve/abc12345") so repeated
// resolutions of the same target are visually recognizable. Not
// security-sensitive (unlike mache-6ec106's salted project tokens): this
// only dedupes mount points within one already-trusted local session, so a
// plain truncated SHA-256 is sufficient.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

func newGraphRegistry(basePath string, args []string) *graphRegistry {
	return &graphRegistry{
		basePath:    basePath,
		args:        args,
		sheafRouter: newSheafEventRouter(),
	}
}

// sheafSubscriberAccessor returns a closure that reads the current
// subscriber's status. Closing over the registry rather than the
// subscriber pointer directly lets the handler see post-startup
// changes (e.g. a future "restart subscriber" admin op replacing the
// pointer). When no subscriber was started, the accessor returns a
// zero SubscriberStatus and ok=false so the handler can render an
// honest "not subscribed" response.
func (r *graphRegistry) sheafSubscriberAccessor() func() (leyline.SubscriberStatus, bool) {
	return func() (leyline.SubscriberStatus, bool) { // coverage:ignore — defensive accessor; reduction tracked in mache-89b5dd.
		if r.sheafSubscriber == nil { // coverage:ignore — defensive guard; reduction tracked in mache-89b5dd.
			return leyline.SubscriberStatus{}, false // coverage:ignore — defensive guard; reduction tracked in mache-89b5dd.
		} // coverage:ignore — defensive guard; reduction tracked in mache-89b5dd.
		return r.sheafSubscriber.Status(), true // coverage:ignore — defensive accessor; reduction tracked in mache-89b5dd.
	} // coverage:ignore — defensive accessor; reduction tracked in mache-89b5dd.
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
	r.sessionErrors.Delete(sessionID)
	// Release hosted-mode repo clone ref if this session used one.
	if repoURL, ok := r.sessionRepos.LoadAndDelete(sessionID); ok {
		r.releaseRepoClone(repoURL.(string))
	}
}

// Close calls the cleanup function on every lazily-built graph.
// Use on server shutdown to release SQLite connections and temp files.
func (r *graphRegistry) Close() {
	// Stop the sheaf subscriber FIRST so it doesn't try to dispatch
	// to invalidators whose graphs are tearing down. Stop blocks
	// until the loop returns, so this is ordered correctly without
	// further synchronization.
	if r.stopSheafSubscriber != nil { // coverage:ignore — registry shutdown wiring; reduction tracked in mache-89b5dd.
		r.stopSheafSubscriber() // coverage:ignore — registry shutdown wiring; reduction tracked in mache-89b5dd.
	} // coverage:ignore — registry shutdown wiring; reduction tracked in mache-89b5dd.

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
	lg := &lazyGraph{args: r.args, basePath: rootPath, sheafRouter: r.sheafRouter}
	actual, _ := r.graphs.LoadOrStore(cacheKey, lg)
	return actual.(*lazyGraph)
}

// graphForSession returns the graph for a session, falling back to basePath.
func (r *graphRegistry) graphForSession(sessionID string) *lazyGraph {
	if rootPath, ok := r.sessions.Load(sessionID); ok {
		return r.getOrCreateGraph(rootPath.(string))
	}
	return r.fallbackGraphForSession(sessionID, nil)
}

// fallbackGraphForSession uses an explicitly configured base path when one is
// available. A shared HTTP daemon has no safe implicit working directory:
// launchd commonly starts it at filesystem root, so treating "." as a project
// after ListRoots fails can recursively scan the entire machine.
func (r *graphRegistry) fallbackGraphForSession(sessionID string, rootsErr error) *lazyGraph {
	if r.basePath != "" {
		r.registerSession(sessionID, r.basePath)
		rememberResolvedRoot(r.basePath, "--path")
		return r.getOrCreateGraph(r.basePath)
	}
	if serveStdio && len(r.args) == 1 {
		source, err := filepath.Abs(r.args[0])
		if err == nil {
			r.registerSession(sessionID, source)
			rememberResolvedRoot(source, "stdio arg")
			return r.getOrCreateGraph(source)
		}
	}
	if cached, ok := r.sessionErrors.Load(sessionID); ok {
		return cached.(*lazyGraph)
	}

	detail := "the MCP client returned no roots"
	if rootsErr != nil {
		detail = rootsErr.Error()
	}
	// `mache init` leads because for the clients that reach this branch it is
	// the only remedy that works — see rememberResolvedRoot (mache-6ec106).
	errGraph := newErrorLazyGraph(fmt.Errorf(
		"workspace root unavailable (%s). Run `mache init` in the project you want served: "+
			"it registers the project and writes a ?project= token into this client's MCP URL, "+
			"which resolves without the client needing to answer roots/list. "+
			"Alternatives: start mache with an explicit --path, or use a client that supports MCP roots",
		detail,
	))
	actual, _ := r.sessionErrors.LoadOrStore(sessionID, errGraph)
	return actual.(*lazyGraph)
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

		// Readiness gate: in daemon mode, check if the graph has any content.
		// The daemon may still be parsing — return a helpful message instead of
		// empty results that confuse the agent.
		if serveControl != "" {
			if children, err := lg.ListChildren(""); err == nil && len(children) == 0 {
				return mcp.NewToolResultText(
					"Graph is still loading — the daemon is parsing source files. Please retry in a few seconds.",
				), nil
			}
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
	if errGraph, ok := r.sessionErrors.Load(sid); ok {
		return errGraph.(*lazyGraph)
	}

	// ?project= token from HTTP context — checked BEFORE ListRoots; see
	// resolveProjectSession's doc comment (mache-6ec106).
	if lg, ok := r.resolveProjectSession(ctx, sid); ok {
		return lg
	}

	// Hosted mode: ?repo= URL from HTTP context.
	// This runs BEFORE CLI repo mode check because hosted mode has per-repo
	// clones, not a single global repoCloneDir.
	if repoURL, ok := repoFromContext(ctx); ok {
		baseDir, err := r.getOrCreateRepoClone(repoURL)
		if err != nil {
			log.Printf("clone %s for session %s: %v", repoURL, sid, err)
			// Return an error-producing graph — don't silently serve wrong repo.
			errLg := newErrorLazyGraph(fmt.Errorf("clone %s: %w", repoURL, err))
			return errLg
		}
		r.sessionRepos.Store(sid, repoURL)
		// Track baseDir per session for correct worktree cleanup.
		r.sessionBaseDirs.Store(sid, baseDir)

		// Create worktree with per-session serialization.
		wtDir, err := r.ensureHostedWorktree(sid, baseDir)
		if err != nil {
			log.Printf("worktree for session %s: %v (using base clone)", sid, err)
			r.registerSession(sid, baseDir)
			return r.getOrCreateGraph(baseDir)
		}
		r.registerSession(sid, wtDir)
		log.Printf("hosted session %s → %s (repo: %s)", sid, wtDir, repoURL)

		lg := r.getOrCreateGraph(wtDir)
		if preset, ok := schemaFromContext(ctx); ok {
			lg.schemaPreset = preset
		}
		return lg
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

	rootPath, rootsErr := discoverSessionRoot(ctx, session)
	if rootPath != "" {
		r.registerSession(sid, rootPath)
		rememberResolvedRoot(rootPath, "client roots")
		log.Printf("session %s → %s", sid, rootPath)
		return r.getOrCreateGraph(rootPath)
	}

	// Fall back only to an explicit --path. In shared-daemon mode, an empty
	// base path must not inherit the supervisor's CWD (often filesystem root).
	return r.fallbackGraphForSession(sid, rootsErr)
}

// resolveProjectSession resolves a session from a ?project= token, if the
// HTTP context carries one. `mache init` registered this project's path
// locally (mache-6ec106) and baked the resulting token into the URL it wrote
// to .claude/mcp.json, so a session can resolve its root without depending
// on the client ever answering ListRoots — a client that supplies this has
// already told us exactly what it wants; there's no reason to also wait on
// roots discovery. An unrecognized token is a distinct, actionable error
// (stale/wiped registry) rather than a silent fall-through to the generic
// "no roots" diagnostic, and — like the ListRoots failure path — is cached
// per session so a bad token doesn't re-pay a lookup on every tool call.
//
// The second return value is false when no ?project= param was present at
// all — the caller should fall through to the next discovery mechanism.
func (r *graphRegistry) resolveProjectSession(ctx context.Context, sid string) (*lazyGraph, bool) {
	token, ok := projectTokenFromContext(ctx)
	if !ok {
		return nil, false
	}
	rootPath, found := resolveProjectToken(token)
	if !found {
		errGraph := newErrorLazyGraph(fmt.Errorf(
			"?project= token not recognized; re-run `mache init` in this project to re-register it"))
		actual, _ := r.sessionErrors.LoadOrStore(sid, errGraph)
		return actual.(*lazyGraph), true
	}
	r.registerSession(sid, rootPath)
	log.Printf("session %s → %s (via ?project=)", sid, rootPath)
	return r.getOrCreateGraph(rootPath), true
}

// rememberResolvedRoot records a workspace root the daemon just resolved, so
// the project is addressable by ?project= token on a LATER connection.
//
// Learning a root is the only moment the daemon knows what project a client
// means, and how it learned it — client roots, --path, a stdio arg — does not
// change how authoritative that statement is. So all three register on the
// same terms.
//
// The ordering is the whole point. Before this, registerProject had exactly
// one caller, `mache init`, so a daemon could serve a project for days with
// ~/.mache/projects.json absent and every token lookup necessarily missing.
// The clients that most NEED token resolution — plain request/response HTTP,
// with no channel for roots/list — are precisely the ones that can never
// populate the registry themselves. Someone else has to do it for them, and
// the daemon is the only party that ever finds out.
//
// `mache init` keeps its own job: writing the token into the client's config,
// which a server cannot do for it.
//
// Failure is deliberately silent. A read-only HOME or a corrupt registry must
// not take down a session that is otherwise resolving fine — serving the graph
// is the job; registration is an optimization for the next client.
func rememberResolvedRoot(rootPath, how string) {
	if ensureProjectRegistered(rootPath) {
		log.Printf("registered project %s (discovered via %s)", rootPath, how)
	}
}

// discoverSessionRoot asks the client for its first workspace root. The short
// timeout keeps unsupported or non-responsive clients from blocking a tool
// call indefinitely.
func discoverSessionRoot(ctx context.Context, session server.ClientSession) (string, error) {
	rootsSession, ok := session.(server.SessionWithRoots)
	if !ok {
		return "", fmt.Errorf("client does not support MCP roots")
	}

	rootsCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	result, err := rootsSession.ListRoots(rootsCtx, mcp.ListRootsRequest{})
	if err != nil {
		return "", err
	}
	if len(result.Roots) == 0 {
		return "", fmt.Errorf("client returned no workspace roots")
	}
	rootPath := rootURIToPath(result.Roots[0].URI)
	if rootPath == "" {
		return "", fmt.Errorf("client returned an invalid workspace root")
	}
	return rootPath, nil
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

	if after, ok := strings.CutPrefix(head, "ref: "); ok {
		ref := after
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
	args         []string
	basePath     string // optional; defaults to "." (CWD) when empty
	schemaPreset string // optional; if set, skips auto-detection (e.g., "go", "python")
	once         sync.Once
	embedOnce    sync.Once // triggers embedding on first successful get()
	inner        graph.Graph
	schema       *api.Topology // retained after init for schema-aware tools
	cleanup      func()
	err          error
	// sheafInv is the SheafInvalidator wired into the file watcher's
	// onChange path (when this lazyGraph backs a directory source).
	// Nil for control mode, .db sources, single-file sources, and
	// composite mounts — see buildServeGraph / buildMaybeMultiGraph
	// for the construction contract. The MCP get_communities handler
	// calls SetCommunityResult + SetSheaf on this to engage the
	// cross-region cascade; until then the watcher's invalidate calls
	// fall back to single-node Graph.Invalidate.
	sheafInv *graph.SheafInvalidator

	// sheafRouter is the back-reference to the graphRegistry's
	// process-wide router. Set at lazyGraph construction time so
	// init() can register sheafInv with the router (and cleanup can
	// unregister it). Optional — when nil, the lazyGraph stands alone
	// and sheaf events are only consumed via the watcher's own
	// invalidator. The router is the seam for daemon-pushed events,
	// not a hard requirement for serving the graph.
	sheafRouter *sheafEventRouter

	// resolveMounts is the side CompositeGraph resolve_ref (mache-be0b9f)
	// dynamically mounts resolved sub-graphs into, exposed to callers under
	// "resolve/<hash>" IDs. Kept separate from `inner` (rather than
	// mounting into inner directly) because inner is only a
	// *graph.CompositeGraph when the caller used --mount; the common
	// single-source serve has no composite to mount into, and wrapping it
	// in one would change the root/path identity every existing tool
	// already depends on.
	//
	// Two-level nesting: resolveMounts has exactly one static mount,
	// "resolve" -> resolveHashMounts, and each resolved target is mounted
	// into resolveHashMounts under its bare hash (no slash — CompositeGraph
	// mount prefixes are single path segments; Mount("resolve/<hash>", g)
	// directly would silently never route, since resolve() only cuts the
	// first segment). Nesting gets correct two-level ID reprefixing for
	// free from CompositeGraph's own (already-tested) GetNode/ListChildren/
	// etc, instead of duplicating that logic here.
	//
	// Both nil until the first successful resolve_ref mount; created
	// together, lazily, via ensureResolveMounts.
	resolveMounts     *graph.CompositeGraph
	resolveHashMounts *graph.CompositeGraph
	resolveMountsOnce sync.Once

	// resolveRegistry is the scheme -> Resolver registry (ADR-0016) used by
	// resolve_ref, anchored to this lazyGraph's own served root. Shared
	// across every session serving the same root, same as inner.
	resolveRegistry     *resolve.Registry
	resolveRegistryOnce sync.Once

	// resolveMountPrefixes memoizes cacheKey -> "resolve/<hash>" so a
	// repeated resolve_ref call for the same target returns the existing
	// mount instead of erroring on CompositeGraph.Mount's duplicate-prefix
	// check. resolveMountSF coalesces concurrent first-time mounts of the
	// same cacheKey (build+mount is real subprocess/IO cost).
	resolveMountPrefixes sync.Map // cacheKey string -> prefix string
	resolveMountSF       singleflight.Group
}

// ensureResolveMounts lazily creates the two-level side CompositeGraph
// resolve_ref mounts into (see the resolveMounts field doc for why it's
// nested). Safe to call from multiple goroutines/sessions.
func (lg *lazyGraph) ensureResolveMounts() *graph.CompositeGraph {
	lg.resolveMountsOnce.Do(func() {
		inner := graph.NewCompositeGraph()
		outer := graph.NewCompositeGraph()
		_ = outer.Mount(strings.TrimSuffix(resolveMountIDPrefix, "/"), inner) // fresh composite; Mount cannot fail here
		lg.resolveHashMounts = inner
		lg.resolveMounts = outer
	})
	return lg.resolveMounts
}

// resolverRegistry lazily builds the mod/gomod resolver registry, anchored
// to this lazyGraph's own served root — both LocalPathResolver (relative-
// locator escape checks) and GoModResolver (`go list`'s working directory)
// need an anchor, and the session's served root is the only one resolve_ref
// has to offer.
//
// Implements the resolveMounter interface (cmd/serve_resolve_ref.go) so the
// resolve_ref handler can reach this without every other Graph
// implementation needing to grow these methods.
func (lg *lazyGraph) resolverRegistry() *resolve.Registry {
	lg.resolveRegistryOnce.Do(func() {
		root := lg.resolvedBasePath()
		reg := resolve.NewRegistry()
		reg.Register("mod", &resolve.LocalPathResolver{Anchor: root})
		reg.Register("gomod", &resolve.GoModResolver{WorkDir: root})
		lg.resolveRegistry = reg
	})
	return lg.resolveRegistry
}

// mountResolved mounts the graph build() produces under a
// "resolve/<hash-of-cacheKey>" prefix and returns that prefix, or returns
// the existing prefix if cacheKey was already mounted. Idempotent, and
// coalesces concurrent callers for the same cacheKey via singleflight so
// two racing resolve_ref calls for the same target build and mount it once.
func (lg *lazyGraph) mountResolved(cacheKey string, build func() (graph.Graph, error)) (string, error) {
	if cached, ok := lg.resolveMountPrefixes.Load(cacheKey); ok {
		return cached.(string), nil
	}
	v, err, _ := lg.resolveMountSF.Do(cacheKey, func() (any, error) {
		if cached, ok := lg.resolveMountPrefixes.Load(cacheKey); ok {
			return cached.(string), nil
		}
		g, buildErr := build()
		if buildErr != nil {
			return nil, buildErr
		}
		lg.ensureResolveMounts() // populates resolveHashMounts as a side effect
		hash := shortHash(cacheKey)
		if mountErr := lg.resolveHashMounts.Mount(hash, g); mountErr != nil {
			// Lost a mount race for the same hash — the winner's result
			// is just as good, use it instead of failing this caller.
			if cached, ok := lg.resolveMountPrefixes.Load(cacheKey); ok {
				return cached.(string), nil
			}
			return nil, mountErr
		}
		prefix := resolveMountIDPrefix + hash
		lg.resolveMountPrefixes.Store(cacheKey, prefix)
		return prefix, nil
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// isResolveMountPath reports whether id falls under the resolve_ref mount
// namespace and so should route to resolveMounts instead of the base graph.
func isResolveMountPath(id string) bool {
	return strings.HasPrefix(strings.TrimPrefix(id, "/"), resolveMountIDPrefix)
}

func newErrorLazyGraph(err error) *lazyGraph {
	lg := &lazyGraph{err: err}
	// Mark initialization complete so a handler cannot replace the diagnostic
	// by auto-detecting a schema from the process working directory.
	lg.once.Do(func() {})
	return lg
}

// SheafInvalidator exposes the cascade-invalidator the file watcher
// holds for this lazyGraph. Returns nil for graphs that don't have a
// watcher (control mode, .db sources, composite mounts) — callers
// must nil-check before use.
func (lg *lazyGraph) SheafInvalidator() *graph.SheafInvalidator {
	lg.init()
	return lg.sheafInv
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

		// --control mode: skip all source detection, read from arena.
		if serveControl != "" {
			g, si, cleanup, err := buildServeGraph("", &api.Topology{Version: api.SchemaVersion})
			if err != nil {
				lg.err = err
				return
			}
			lg.inner = g
			lg.sheafInv = si                                            // expected nil in control mode; stored for uniformity // coverage:ignore — control-mode wiring; reduction tracked in mache-89b5dd.
			lg.cleanup = lg.wrapCleanupWithSheafUnregister(cleanup, si) // coverage:ignore — control-mode wiring; reduction tracked in mache-89b5dd.
			lg.schema = &api.Topology{Version: api.SchemaVersion}
			lg.registerSheafInvalidator() // coverage:ignore — control-mode wiring; reduction tracked in mache-89b5dd.
			log.Println("graph ready (arena control mode)")
			return
		}

		if len(lg.args) == 0 {
			// If a schema preset was provided (e.g., from ?schema= query param),
			// use it directly — skip config loading and auto-detection.
			if lg.schemaPreset != "" {
				resolved, err := resolveSchema(lg.schemaPreset, base)
				if err != nil {
					lg.err = fmt.Errorf("resolve schema preset %q: %w", lg.schemaPreset, err)
					return
				}
				schema = resolved
				dataSource = base
				log.Printf("using schema preset %q (from query param)", lg.schemaPreset)
			} else if cfg, err := loadProjectConfig(base); err != nil {
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

		g, si, cleanup, err := buildMaybeMultiGraph(dataSource, schema)
		if err != nil {
			lg.err = err
			return
		}
		lg.inner = g
		lg.sheafInv = si
		lg.schema = schema
		lg.cleanup = lg.wrapCleanupWithSheafUnregister(cleanup, si)
		lg.registerSheafInvalidator()
		log.Println("graph ready")
	})
}

// registerSheafInvalidator hooks lg.sheafInv into the registry's
// process-wide router so daemon-pushed sheaf.invalidate events can
// route to this graph. No-op when either the router or the invalidator
// is nil — the watcher cascade still works in the local-only path
// even without the subscriber wired.
func (lg *lazyGraph) registerSheafInvalidator() {
	if lg.sheafRouter == nil || lg.sheafInv == nil {
		return
	}
	lg.sheafRouter.register(lg.sheafInv) // coverage:ignore — router wiring on lazyGraph init; reduction tracked in mache-89b5dd.
}

// wrapCleanupWithSheafUnregister produces a cleanup func that
// unregisters this graph's SheafInvalidator from the router BEFORE
// running the inner cleanup. Ordering: stop receiving events first,
// then tear down the graph state those events would touch. Returns
// inner unchanged when the router or invalidator is nil — no need
// to wrap a no-op.
func (lg *lazyGraph) wrapCleanupWithSheafUnregister(inner func(), si *graph.SheafInvalidator) func() {
	if lg.sheafRouter == nil || si == nil {
		return inner
	}
	router := lg.sheafRouter // coverage:ignore — router cleanup wiring; reduction tracked in mache-89b5dd.
	return func() {          // coverage:ignore — router cleanup wiring; reduction tracked in mache-89b5dd.
		router.unregister(si) // coverage:ignore — router cleanup wiring; reduction tracked in mache-89b5dd.
		if inner != nil {     // coverage:ignore — router cleanup wiring; reduction tracked in mache-89b5dd.
			inner() // coverage:ignore — router cleanup wiring; reduction tracked in mache-89b5dd.
		} // coverage:ignore — router cleanup wiring; reduction tracked in mache-89b5dd.
	} // coverage:ignore — router cleanup wiring; reduction tracked in mache-89b5dd.
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
	if isResolveMountPath(id) {
		return lg.ensureResolveMounts().GetNode(id)
	}
	g, err := lg.get()
	if err != nil {
		return nil, err
	}
	return g.GetNode(id)
}

func (lg *lazyGraph) ListChildren(id string) ([]string, error) {
	if isResolveMountPath(id) {
		return lg.ensureResolveMounts().ListChildren(id)
	}
	g, err := lg.get()
	if err != nil {
		return nil, err
	}
	return g.ListChildren(id)
}

func (lg *lazyGraph) ListChildStats(id string) ([]graph.NodeStat, error) {
	if isResolveMountPath(id) {
		return lg.ensureResolveMounts().ListChildStats(id)
	}
	g, err := lg.get()
	if err != nil {
		return nil, err
	}
	return g.ListChildStats(id)
}

func (lg *lazyGraph) ReadContent(id string, buf []byte, offset int64) (int, error) {
	if isResolveMountPath(id) {
		return lg.ensureResolveMounts().ReadContent(id, buf, offset)
	}
	g, err := lg.get()
	if err != nil {
		return 0, err
	}
	return g.ReadContent(id, buf, offset)
}

// GetCallers federates across the base graph and any resolve_ref mounts —
// unlike GetNode/ListChildren, a caller token isn't namespaced by mount
// prefix, so a token defined in either graph should surface its callers
// from both. resolveMounts is only queried once it has an actual mount
// (nil until then), so this is a no-op federation for every session that
// never calls resolve_ref.
func (lg *lazyGraph) GetCallers(token string) ([]*graph.Node, error) {
	g, err := lg.get()
	if err != nil {
		return nil, err
	}
	callers, err := g.GetCallers(token)
	if err != nil {
		return nil, err
	}
	if lg.resolveMounts != nil {
		if extra, extraErr := lg.resolveMounts.GetCallers(token); extraErr == nil {
			callers = append(callers, extra...)
		}
	}
	return callers, nil
}

func (lg *lazyGraph) GetCallees(id string) ([]*graph.Node, error) {
	if isResolveMountPath(id) {
		return lg.ensureResolveMounts().GetCallees(id)
	}
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

// lazyGraph also implements graph.RefsQuerier, graph.RefsMapper, and graph.DefsMapper
// by delegating to the inner graph if it supports those interfaces.

func (lg *lazyGraph) QueryRefs(query string, args ...any) (*sql.Rows, error) {
	g, err := lg.get()
	if err != nil {
		return nil, err
	}
	if qg, ok := g.(graph.RefsQuerier); ok {
		return qg.QueryRefs(query, args...)
	}
	return nil, fmt.Errorf("backend does not support QueryRefs")
}

func (lg *lazyGraph) RefsMap() map[string][]string {
	g, err := lg.get()
	if err != nil || g == nil {
		return nil
	}
	if rp, ok := g.(graph.RefsMapper); ok {
		return rp.RefsMap()
	}
	return nil
}

// DBPath implements the graph.DBPathProvider opt-in by delegating to the inner
// graph, the same way QueryRefs and RefsMap do.
//
// Without this forwarder the `qg.(graph.DBPathProvider)` assertion fails for every
// serve-mode query — handlers hold a *lazyGraph, not the SQLiteGraph or
// WritableGraph underneath it — so the sibling `.bindings.capnp` event log is
// never consulted and both of its consumers silently degrade:
//
//   - queryLSPRefs falls through to queryLSPRefsLegacy, the `_lsp_refs` SQL
//     path that mache-6bd4d8 retired as the consumer-side contract. On a .db
//     built after LLO T8.2 (which emits the capnp log and no longer populates
//     those columns) find_callers loses its lsp_refs supplement entirely.
//   - ensureSmellQueryContext skips LoadCapnpBindings, so the
//     `_capnp_binding_refs` TEMP table stays empty and the v_refs UNION arm
//     over it contributes nothing. MCP find_smells then sees strictly fewer
//     refs than the find-smells CLI, whose dbQuerier does implement DBPath —
//     same rules, same .db, different answers.
//
// Returning "" for a backend that has no path is the documented "no source"
// sentinel: readLSPRefsFromCapnp and LoadCapnpBindings both no-op on it, so a
// MemoryStore-backed graph degrades exactly as it did before.
func (lg *lazyGraph) DBPath() string {
	g, err := lg.get()
	if err != nil || g == nil {
		return ""
	}
	if dp, ok := g.(graph.DBPathProvider); ok {
		return dp.DBPath()
	}
	return ""
}

func (lg *lazyGraph) DefsMap() map[string][]string {
	out := map[string][]string{}
	if lg.resolveMounts != nil {
		for token, ids := range lg.resolveMounts.DefsMap() {
			out[token] = append(out[token], ids...)
		}
	}
	g, err := lg.get()
	if err != nil || g == nil {
		return nilIfEmpty(out)
	}
	if dp, ok := g.(graph.DefsMapper); ok {
		for token, ids := range dp.DefsMap() {
			out[token] = append(out[token], ids...)
		}
	}
	return nilIfEmpty(out)
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

// MountPrefixOf forwards to the inner CompositeGraph so MCP handlers'
// annotateMounts() can find a mount-prefix resolver even though the
// registry hands them a lazyGraph wrapper. Returns "" when the inner
// isn't a composite (single-source serves) — same semantics agents
// already see for non-composite shapes.
func (lg *lazyGraph) MountPrefixOf(id string) string {
	g, err := lg.get()
	if err != nil || g == nil {
		return ""
	}
	if cg, ok := g.(*graph.CompositeGraph); ok {
		return cg.MountPrefixOf(id)
	}
	return ""
}

// LookupDef forwards the optional graph.DefsLookuper interface so
// find_definition's anchored-exact path can use the O(1) lookup
// instead of falling through to the O(N) DefsMap snapshot. Without
// this passthrough, every find_definition call in production paid
// the snapshot cost even when the inner backend (MemoryStore,
// SQLiteGraph, CompositeGraph) had a fast lookup available.
//
// Returns nil when the inner doesn't implement graph.DefsLookuper —
// callers fall through to DefsMap as before.
// LookupDef federates the base graph and any resolve_ref mounts — a token
// may be defined in either, and dir IDs from resolveMounts already carry
// their "resolve/<hash>/..." prefix (CompositeGraph.LookupDef applies it),
// so results from both sides are directly usable by GetNode/ReadContent
// without further qualification.
func (lg *lazyGraph) LookupDef(token string) []string {
	var out []string
	if lg.resolveMounts != nil {
		out = append(out, lg.resolveMounts.LookupDef(token)...)
	}
	g, err := lg.get()
	if err != nil || g == nil {
		return out
	}
	if dl, ok := g.(graph.DefsLookuper); ok {
		out = append(out, dl.LookupDef(token)...)
	}
	return out
}

// SearchDefs forwards the optional graph.DefsSearcher interface so
// `search role=definition` can use a SQL-pushdown when the inner
// backend supports it. Mirrors LookupDef's passthrough shape.
//
// Returns nil when the inner doesn't implement graph.DefsSearcher —
// the search handler falls through to graph.DefsMapper iteration.
func (lg *lazyGraph) SearchDefs(pattern string, limit int) map[string][]string {
	out := map[string][]string{}
	if lg.resolveMounts != nil {
		for token, ids := range lg.resolveMounts.SearchDefs(pattern, limit) {
			out[token] = append(out[token], ids...)
		}
	}
	g, err := lg.get()
	if err != nil || g == nil {
		return nilIfEmpty(out)
	}
	if ds, ok := g.(interface {
		SearchDefs(string, int) map[string][]string
	}); ok {
		for token, ids := range ds.SearchDefs(pattern, limit) {
			out[token] = append(out[token], ids...)
		}
	}
	return nilIfEmpty(out)
}

// nilIfEmpty normalizes an empty federation result back to nil so callers
// that branch on "backend doesn't support this" (nil) vs. "supports it but
// found nothing" (empty map) keep seeing the same nil they did before
// resolveMounts federation was added.
func nilIfEmpty(m map[string][]string) map[string][]string {
	if len(m) == 0 {
		return nil
	}
	return m
}

// ---------------------------------------------------------------------------
// Interface types for optional graph backend capabilities
// ---------------------------------------------------------------------------

// graph.RefsQuerier is the subset of Graph backends that support SQL queries.

// graph.RefsMapper is the subset of Graph backends that expose their refs map
// for community detection (Louvain).

// graph.DefsMapper is the subset of Graph backends that expose their defs map
// for find_definition (symbol → where it's defined).

// sheafInvalidatorProvider is the subset of Graph backends that own a
// SheafInvalidator wired into their file-watcher onChange path. The
// get_communities handler type-asserts this to install the freshly-
// detected CommunityResult + dialed SheafClient, which is the trigger
// that moves the watcher's cascade calls from single-node fallback
// into actual cross-region propagation. Backends that don't have a
// watcher (control-mode lazyGraph, composite mounts) don't implement
// this interface; the handler degrades silently in that case.
type sheafInvalidatorProvider interface {
	SheafInvalidator() *graph.SheafInvalidator
}

// graph.DefsLookuper is the cheaper alternative to graph.DefsMapper for the
// common case of looking up exactly one symbol. Backends that
// implement it avoid the O(N) snapshot copy of the full defs map
// when the caller only needs one token's dir IDs.

// graph.DefsSearcher supports pattern-based search across the defs index
// without snapshotting the whole map. The pattern uses SQL LIKE
// syntax ('%' = any chars, '_' = single char). SQL-backed graphs
// push the filter down to the database; in-memory graphs may
// implement it as a linear scan with sqlLikeMatch. Returns up to
// `limit` token→nodeIDs entries.
//
// search role=definition uses this when available — fixes the
// bug where SQLiteGraph's empty in-memory defs map made the
// search handler return [] for every pattern (bead mache-9cba08).

// writeBacker is the subset of Graph backends that support surgical write-back
// (validate → format → splice → update node).
type writeBacker interface {
	UpdateNodeContent(id string, data []byte, origin *graph.SourceOrigin, modTime time.Time) error
	ShiftOrigins(filePath string, afterByte uint32, delta int32)
}

// Every opt-in interface above is reached by a `g.(X)` assertion in some
// handler, and in serve mode `g` is ALWAYS a *lazyGraph — never the
// SQLiteGraph or WritableGraph underneath it. So a capability lazyGraph
// forgets to forward is not a compile error and not a test failure: the
// assertion just returns ok=false and the caller takes its "backend doesn't
// support this" path. The feature silently disappears for every consumer
// reaching mache through `mache serve`, which is every MCP client.
//
// That is not hypothetical — DBPath was missing, which cost find_callers its
// lsp_refs supplement and left MCP find_smells reading a strictly smaller ref
// set than the find-smells CLI over the identical .db.
//
// These assertions turn the whole class into a build failure. Adding an opt-in
// interface without a lazyGraph forwarder now fails to compile instead of
// silently degrading at runtime.
var (
	_ graph.RefsQuerier        = (*lazyGraph)(nil)
	_ graph.RefsMapper         = (*lazyGraph)(nil)
	_ graph.DefsMapper         = (*lazyGraph)(nil)
	_ graph.DefsLookuper       = (*lazyGraph)(nil)
	_ graph.DefsSearcher       = (*lazyGraph)(nil)
	_ graph.DBPathProvider     = (*lazyGraph)(nil)
	_ graph.MountPrefixer      = (*lazyGraph)(nil)
	_ schemaProvider           = (*lazyGraph)(nil)
	_ sheafInvalidatorProvider = (*lazyGraph)(nil)
	_ writeBacker              = (*lazyGraph)(nil)
	_ graph.Graph              = (*lazyGraph)(nil)
)
