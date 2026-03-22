# 10. Hosted Mache Architecture

Date: 2026-03-21
Status: Proposed
Depends-On: ADR-0002 (Declarative Topology), ADR-0009 (AST-Aware Write Pipeline)

## Context

Mache currently runs locally: the user installs the binary, points it at a data source, and gets a projected graph via FUSE/NFS mount or MCP server. We want to host mache at `mcp.rosary.bot` so users connect their AI tools (Claude, Cursor, Copilot) to a remote MCP endpoint without installing anything.

The existing `mache serve --http :7532` already provides Streamable HTTP MCP with stateful sessions. The `graphRegistry` in `cmd/serve.go` routes sessions to per-workspace graphs keyed by `rootPath@gitHEAD`. What is missing is the complete server lifecycle: cloning repositories on demand, persisting indexed graphs to remote storage, re-indexing on change, and managing the lifecycle of ephemeral vs. persistent workspaces across a fleet of workers.

### What We Already Have

| Component                     | Location                           | Role in Hosted                                             |
| ----------------------------- | ---------------------------------- | ---------------------------------------------------------- |
| `graphRegistry`               | `cmd/serve.go`                     | Session-to-graph routing, git HEAD cache keying            |
| `lazyGraph`                   | `cmd/serve.go`                     | Lazy init with `sync.Once`, cleanup on eviction            |
| `HotSwapGraph`                | `internal/graph/hotswap.go`        | Atomic graph replacement behind `RWMutex`                  |
| `Engine.ReIngestFile`         | `internal/ingest/engine.go`        | Incremental re-ingest of single changed files              |
| `MemoryStore.DeleteFileNodes` | `internal/graph/graph.go`          | Surgical node removal by source path (roaring bitmap)      |
| `SQLiteWriter`                | `internal/ingest/sqlite_writer.go` | Serializes graph to SQLite (nodes, refs, defs, file_index) |
| `SQLiteGraph`                 | `internal/graph/sqlite_graph.go`   | Reads serialized .db directly as lazy graph                |
| `SheafClient`                 | `internal/leyline/sheaf.go`        | Topology-aware cache invalidation via ley-line daemon      |
| `TriggerEmbedding`            | `internal/leyline/trigger.go`      | Push graph content to ley-line for semantic search         |

### The Design Problem

1. **Ephemeral mode** (free tier): clone repo, project graph, serve MCP, cleanup on session end.
1. **Persistent mode** (paid tier): clone, index, persist graph to remote storage, re-index on webhook/change, serve from cached graph.
1. **Incremental re-index**: webhook or fsnotify triggers re-ingestion of only changed files, surgical graph update, debounced persistence (not every change).
1. **Pluggable storage**: the persistence layer must be abstract -- R2 today, GCS/S3/SQLite-on-disk tomorrow.
1. **Multi-tenant**: multiple users connect to different repos simultaneously on shared infrastructure.

## Decision

### Architecture Overview

```
                         mcp.rosary.bot
                    ┌──────────────────────────┐
                    │       Edge / Router       │
                    │  (TLS termination, auth)  │
                    └─────────┬────────────────┘
                              │ Streamable HTTP MCP
                    ┌─────────▼────────────────┐
                    │     Session Router        │
                    │  sessionID → WorkspaceID  │
                    │  (extended graphRegistry)  │
                    └─────────┬────────────────┘
                              │
              ┌───────────────┼───────────────┐
              │               │               │
     ┌────────▼──────┐ ┌─────▼───────┐ ┌─────▼───────┐
     │  Workspace A  │ │ Workspace B │ │ Workspace C │
     │  (repo X)     │ │ (repo Y)    │ │ (repo X)    │
     │               │ │             │ │             │
     │ ┌───────────┐ │ │  ...        │ │  (shares    │
     │ │ HotSwap   │ │ │             │ │   graph     │
     │ │ Graph     │ │ │             │ │   with A)   │
     │ └─────┬─────┘ │ │             │ │             │
     │       │ swap   │ │             │ │             │
     │ ┌─────▼─────┐ │ │             │ │             │
     │ │MemoryStore│ │ │             │ │             │
     │ │ (gen N)   │ │ │             │ │             │
     │ └───────────┘ │ │             │ │             │
     └───────┬───────┘ └─────────────┘ └─────────────┘
             │ debounced persist
     ┌───────▼──────────────────────────────────────┐
     │            Storage Backend (R2/GCS/S3)        │
     │   key: repo_url_hash/branch/graph.db          │
     └──────────────────────────────────────────────┘
```

Multiple sessions connected to the same repository and branch share a single `Workspace` instance. Each workspace owns a `HotSwapGraph` that wraps the live `MemoryStore`. Graph updates (from re-indexing) produce a new `MemoryStore`, which is atomically swapped in. The old store is not closed until all in-flight method calls complete (guaranteed by the RWMutex read lock in HotSwapGraph).

### 1. Workspace Lifecycle

A `Workspace` is the unit of isolation. It encapsulates: a git working tree, an ingestion engine, a live graph, and a persistence handle.

```go
// Workspace manages the full lifecycle of a single projected repository.
type Workspace struct {
    ID         string          // hash of (repoURL, branch)
    RepoURL    string
    Branch     string
    Mode       WorkspaceMode   // Ephemeral or Persistent
    WorkDir    string          // local clone path (tmpdir or persistent)

    graph      *graph.HotSwapGraph
    engine     *ingest.Engine
    schema     *api.Topology
    generation uint64          // monotonic, advances on each re-index

    // Persistence
    store      StorageBackend
    persistMu  sync.Mutex      // serializes persist operations
    dirty      atomic.Bool     // set by re-index, cleared by persist

    // Change detection
    watcher    ChangeWatcher   // webhook receiver or fsnotify
    debouncer  *Debouncer      // coalesces rapid changes

    // Session tracking
    sessions   sync.Map        // sessionID -> *SessionState
    lastAccess atomic.Int64    // unix timestamp, for idle eviction

    mu         sync.RWMutex
}

type WorkspaceMode int

const (
    ModeEphemeral  WorkspaceMode = iota // cleanup on last session disconnect
    ModePersistent                       // persist graph, survive restarts
)
```

**Lifecycle states:**

```
                   ┌──────────┐
         clone     │          │  persist
    ───────────►   │  ACTIVE  │ ──────────►  storage
                   │          │
                   └────┬─────┘
                        │ idle timeout OR
                        │ last session disconnects (ephemeral)
                   ┌────▼─────┐
                   │  DRAINING│  (finish in-flight, persist if dirty)
                   └────┬─────┘
                        │
                   ┌────▼─────┐
                   │  CLOSED  │  (cleanup working tree)
                   └──────────┘
```

Ephemeral workspaces are cleaned up when the last session disconnects. Persistent workspaces survive disconnection but are evicted from memory after an idle timeout (default 15 minutes), with their graph state persisted to remote storage before eviction.

### 2. Pluggable Storage Backend

The key design insight is that `SQLiteWriter` already serializes the full graph state (nodes, refs, defs, file_index) into a portable SQLite database, and `SQLiteGraph` reads it back natively. The storage backend moves these SQLite files to/from remote object storage.

```go
// StorageBackend persists and retrieves serialized graph databases.
// Implementations are responsible for their own connection pooling and retry logic.
type StorageBackend interface {
    // Put uploads a serialized graph database to remote storage.
    // key is the workspace-derived storage path (e.g., "abc123/main/graph.db").
    // The source path is a local file. Put must be atomic: partial uploads
    // must not be visible to Get.
    Put(ctx context.Context, key string, sourcePath string) error

    // Get downloads a serialized graph database from remote storage.
    // Returns os.ErrNotExist if no graph has been persisted for this key.
    // The destPath is where the file should be written locally.
    Get(ctx context.Context, key string, destPath string) error

    // Exists checks whether a persisted graph exists for the given key
    // without downloading it. Used for cold-start optimization.
    Exists(ctx context.Context, key string) (bool, error)

    // Delete removes a persisted graph. Called during workspace cleanup
    // for ephemeral workspaces that were force-persisted during drain.
    Delete(ctx context.Context, key string) error

    // PutMeta stores small metadata (JSON) alongside the graph.
    // Used for: generation number, last commit hash, schema version,
    // index timestamp.
    PutMeta(ctx context.Context, key string, meta []byte) error

    // GetMeta retrieves metadata. Returns os.ErrNotExist if absent.
    GetMeta(ctx context.Context, key string) ([]byte, error)
}
```

**Storage key structure:**

```
<repo_hash>/<branch>/graph.db       -- serialized graph
<repo_hash>/<branch>/meta.json      -- generation, commit, timestamp
<repo_hash>/<branch>/embed.db       -- ley-line embedding sidecar (optional)
```

The `repo_hash` is SHA-256 of the normalized repository URL (lowercase, trailing `.git` stripped). This prevents path traversal and keeps keys opaque.

**Why SQLite as the wire format (not protobuf, not custom binary):**

| Alternative                            | Rejected Because                                                                                                        |
| -------------------------------------- | ----------------------------------------------------------------------------------------------------------------------- |
| Custom binary (ECS/mmap from ADR-0004) | Platform-specific (endianness, page size), requires custom deserializer                                                 |
| Protobuf/FlatBuffers                   | Adds a serialization layer that the graph cannot read directly -- would need to deserialize into MemoryStore            |
| JSON dump                              | Prohibitively large for repos with 100K+ nodes                                                                          |
| SQLite                                 | Already produced by SQLiteWriter, already consumed by SQLiteGraph, portable, self-describing, compresses well with zstd |

The storage backend uploads zstd-compressed SQLite files. On download, decompress to a temp file and open with `SQLiteGraph.OpenSQLiteGraph`. Compression ratios for graph databases are typically 8-12x because node content is highly repetitive (template-rendered source code).

### 3. Cold Start Path

The cold start sequence when a user connects to a repository for the first time on this worker:

```
Request arrives for repo X, branch main
     │
     ▼
┌─────────────────────────┐
│ Check in-memory registry │──── found ──► return existing Workspace
└─────────┬───────────────┘
          │ miss
          ▼
┌─────────────────────────┐
│ Check remote storage     │──── found ──► download graph.db
│ (storage.Exists)         │              │
└─────────┬───────────────┘              ▼
          │ miss                 ┌────────────────────┐
          │                      │ Open as SQLiteGraph │
          │                      │ (fast: ~200ms)      │
          │                      └────────┬───────────┘
          │                               │
          │                      ┌────────▼───────────┐
          │                      │ Start background    │
          │                      │ git clone + full    │
          │                      │ re-index (async)    │
          │                      └────────────────────┘
          │                      Serve stale graph while
          │                      re-index builds fresh one
          │
          ▼
┌─────────────────────────┐
│ Full cold start:         │
│ 1. git clone --depth 1   │
│ 2. Detect schema         │
│ 3. Engine.Ingest (full)  │
│ 4. Persist to storage    │
│ 5. Serve                 │
└─────────────────────────┘
```

**Latency budget:**

| Step                             | Target  | Notes                                                                       |
| -------------------------------- | ------- | --------------------------------------------------------------------------- |
| Storage check                    | \<100ms | HEAD request to R2/S3                                                       |
| Graph download (10MB compressed) | \<500ms | Decompresses to ~80MB                                                       |
| SQLiteGraph open + eager scan    | \<2s    | Existing benchmark: 12s for 323K NVD records; typical codebase: 5-20K nodes |
| Full cold clone + index          | 10-60s  | Repo size dependent. User sees "indexing..." progress via MCP notifications |

When a cached graph exists in remote storage, the user gets responses within 2-3 seconds of first connection. A background goroutine performs a fresh clone and re-index; when complete, the new graph is swapped in via HotSwapGraph.

### 4. Incremental Re-Index

File changes are detected via two mechanisms:

**Primary: Webhooks (persistent mode)**

```
GitHub/GitLab push webhook
     │
     ▼
┌─────────────────────────────┐
│ Webhook Handler              │
│ Parse push event:            │
│   - commit range (before/after) │
│   - modified file list       │
│   - branch                   │
└─────────┬───────────────────┘
          │
          ▼
┌─────────────────────────────┐
│ git fetch + checkout         │
│ (in workspace working tree)  │
└─────────┬───────────────────┘
          │
          ▼
┌─────────────────────────────┐
│ For each changed file:       │
│   1. DeleteFileNodes(path)   │  ◄── roaring bitmap O(k)
│   2. ReIngestFile(path)      │  ◄── tree-sitter parse + schema walk
│   3. Mark workspace dirty    │
└─────────┬───────────────────┘
          │
          ▼
┌─────────────────────────────┐
│ Debouncer (30s quiet period) │
│   - Coalesces rapid pushes   │
│   - On fire: serialize +     │
│     upload to storage        │
└─────────────────────────────┘
```

**Fallback: TTL Polling (ephemeral mode or webhook-less repos)**

```go
// PollInterval is the TTL between git fetch checks for repos without webhooks.
// Aggressive enough to catch branch switches, conservative enough to avoid
// GitHub rate limits. Scaled by tier: free = 5min, paid = 30s.
const (
    PollIntervalFree = 5 * time.Minute
    PollIntervalPaid = 30 * time.Second
)
```

The poller runs `git fetch` and compares the local HEAD with the remote. If they diverge, it performs the same incremental re-index sequence as the webhook path.

### 5. Debounced Persistence

Persistence to remote storage is the most cost-sensitive operation. R2/GCS charges per PUT request and per GB stored. A naive "persist after every re-index" would be expensive for repositories with active development (multiple pushes per minute).

```go
// Debouncer coalesces rapid changes into a single persistence operation.
// It uses a quiet-period model: the timer resets on each new change.
// After the quiet period expires, it fires exactly once.
type Debouncer struct {
    quietPeriod time.Duration   // default 30s
    maxDelay    time.Duration   // default 5min (upper bound even under continuous change)
    timer       *time.Timer
    firstChange time.Time       // tracks maxDelay
    mu          sync.Mutex
    fire        func()          // the persist callback
}

// Touch signals a change occurred. Resets the quiet-period timer.
// If maxDelay has been exceeded since the first change, fires immediately.
func (d *Debouncer) Touch() {
    d.mu.Lock()
    defer d.mu.Unlock()

    now := time.Now()
    if d.firstChange.IsZero() {
        d.firstChange = now
    }

    // Hard ceiling: if changes have been accumulating for too long, flush now.
    if now.Sub(d.firstChange) >= d.maxDelay {
        d.timer.Stop()
        d.firstChange = time.Time{}
        go d.fire()
        return
    }

    // Reset quiet period
    if d.timer != nil {
        d.timer.Stop()
    }
    d.timer = time.AfterFunc(d.quietPeriod, func() {
        d.mu.Lock()
        d.firstChange = time.Time{}
        d.mu.Unlock()
        d.fire()
    })
}
```

The `fire` callback:

1. Acquires `workspace.persistMu` (serializes concurrent persist attempts).
1. Checks `workspace.dirty` -- if false, no-op (another persist already ran).
1. Serializes the current MemoryStore to a temp .db file via SQLiteWriter.
1. Compresses with zstd.
1. Uploads to storage backend.
1. Writes metadata (generation, commit hash, timestamp).
1. Clears `workspace.dirty`.

**Critical invariant:** Serialization reads from the live HotSwapGraph. Because HotSwapGraph's methods hold a read lock, and serialization walks the entire graph (ListChildren recursively + ReadContent for each leaf), we must ensure that no graph swap occurs during serialization. Two options:

- **Option A: Snapshot isolation.** Clone the MemoryStore into a new instance before serializing. Memory-expensive (doubles RAM briefly) but allows the live graph to continue receiving updates.
- **Option B: Block swaps during persist.** Hold HotSwapGraph's write lock during serialization. Simple but blocks re-index until persist completes.

**Decision: Option A (snapshot isolation).** The persist path creates a shallow copy of the MemoryStore's node map and serializes from the copy. The live graph continues serving reads and can even be swapped during serialization. The snapshot cost is bounded: we copy map entries (pointers), not node content.

### 6. HotSwapGraph: Generation-Aware Swap

The existing HotSwapGraph has a subtle safety issue: `Swap()` closes the old graph immediately, but MCP tool handlers may hold `*Node` pointers from the old graph across multiple calls within a single session interaction. For example, a `list_directory` response returns node IDs that the client then uses in subsequent `read_file` calls -- if a swap occurs between those calls, the node IDs still resolve correctly (IDs are path-based, not memory-addressed), but any `ContentRef` pointers from the old graph would fail to resolve if the old SQLite connection was closed.

For the hosted server, we extend HotSwapGraph with generation-aware draining:

```go
// GenerationalGraph extends HotSwapGraph with epoch-based draining.
// Old graph generations are kept alive until their reference count drops to zero.
type GenerationalGraph struct {
    mu         sync.RWMutex
    current    *generation
    retired    []*generation  // generations with active readers, awaiting drain
    cleanupMu  sync.Mutex
}

type generation struct {
    id       uint64
    graph    Graph
    refs     atomic.Int64     // active reader count
    closeOnce sync.Once
}

// Acquire returns the current generation and increments its reference count.
// Callers MUST call Release when done.
func (gg *GenerationalGraph) Acquire() *generation {
    gg.mu.RLock()
    gen := gg.current
    gen.refs.Add(1)
    gg.mu.RUnlock()
    return gen
}

// Release decrements the reference count. If this generation is retired
// and the count reaches zero, it is closed and freed.
func (gg *GenerationalGraph) Release(gen *generation) {
    if gen.refs.Add(-1) <= 0 {
        gg.tryCleanup()
    }
}

// Swap installs a new graph generation. The old generation is moved to
// the retired list and will be closed when its reference count drops to zero.
func (gg *GenerationalGraph) Swap(newGraph Graph) {
    gg.mu.Lock()
    old := gg.current
    gg.current = &generation{
        id:    old.id + 1,
        graph: newGraph,
    }
    gg.retired = append(gg.retired, old)
    gg.mu.Unlock()

    gg.tryCleanup()
}
```

This is a simplified epoch-based reclamation (cf. ADR-0004) without the mmap complexity. The `generation` struct uses atomic reference counting rather than goroutine-local epoch tracking because MCP sessions are long-lived and may hold references across multiple HTTP requests.

### 7. Change Detection Interface

```go
// ChangeWatcher detects modifications to a workspace's source data
// and delivers change events to the workspace's re-index pipeline.
type ChangeWatcher interface {
    // Start begins watching. Events are delivered to the callback.
    // The watcher must coalesce rapid events internally (e.g., fsnotify
    // may fire multiple events for a single save operation).
    Start(ctx context.Context, onChange func([]ChangedFile)) error

    // Stop halts watching and releases resources.
    Stop() error
}

// ChangedFile describes a single file modification.
type ChangedFile struct {
    Path   string     // relative to workspace root
    Action FileAction // Added, Modified, Deleted
}

type FileAction int

const (
    FileAdded    FileAction = iota
    FileModified
    FileDeleted
)
```

Two implementations:

| Implementation   | Trigger                                         | Use Case                                 |
| ---------------- | ----------------------------------------------- | ---------------------------------------- |
| `WebhookWatcher` | GitHub/GitLab push events                       | Persistent repos with webhook configured |
| `PollWatcher`    | Periodic `git fetch` + `git diff --name-status` | Ephemeral repos, repos without webhooks  |

A third implementation, `FSNotifyWatcher`, is deferred. It would be useful for local persistent workspaces but adds complexity (recursive watches, rename handling, `.git/` exclusion) that is not needed for the initial hosted deployment where all changes arrive via git operations.

### 8. Workspace Manager

The `WorkspaceManager` is the top-level orchestrator that replaces `graphRegistry` for the hosted server. It maps repository coordinates to workspace instances and manages their lifecycle.

```go
// WorkspaceManager owns all active workspaces and handles lifecycle.
type WorkspaceManager struct {
    workspaces sync.Map   // workspaceID (string) → *Workspace
    sessions   sync.Map   // sessionID (string) → workspaceID (string)
    storage    StorageBackend
    config     HostedConfig
}

type HostedConfig struct {
    MaxWorkspaces       int           // total across all users
    MaxWorkspacesPerUser int          // per authenticated user
    IdleTimeout         time.Duration // evict persistent workspaces after this
    EphemeralTTL        time.Duration // hard TTL for ephemeral workspaces
    CloneDepth          int           // default git clone depth (1 for ephemeral, 0 for persistent)
    PersistQuietPeriod  time.Duration // debounce quiet period
    PersistMaxDelay     time.Duration // debounce hard ceiling
    StoragePrefix       string        // prefix for storage keys
}

// WorkspaceID derives a deterministic ID from repository coordinates.
// Uses SHA-256(normalized_url + "/" + branch)[:16] for collision resistance
// while keeping keys human-debuggable in logs.
func WorkspaceID(repoURL, branch string) string {
    normalized := strings.ToLower(strings.TrimSuffix(repoURL, ".git"))
    h := sha256.Sum256([]byte(normalized + "/" + branch))
    return hex.EncodeToString(h[:8])
}
```

### 9. Session-to-Workspace Routing

When a new MCP session connects, the router must determine which workspace to assign:

```
New MCP session (Streamable HTTP)
     │
     ▼
┌──────────────────────────────────┐
│ Extract repo coordinates from:    │
│   1. MCP ListRoots response      │  ◄── client workspace root
│   2. Query parameter (?repo=...) │  ◄── direct URL
│   3. Auth token → user's repos   │  ◄── dashboard binding
└─────────┬────────────────────────┘
          │
          ▼
┌──────────────────────────────────┐
│ WorkspaceManager.GetOrCreate     │
│   - Check in-memory registry     │
│   - Check remote storage         │
│   - Clone + index if needed      │
└─────────┬────────────────────────┘
          │
          ▼
┌──────────────────────────────────┐
│ Bind session to workspace        │
│ sessions[sessionID] = workspaceID │
└──────────────────────────────────┘
```

The critical path from session connect to first tool response:

| Scenario             | Latency | Path                                                      |
| -------------------- | ------- | --------------------------------------------------------- |
| Workspace in memory  | \<1ms   | Direct map lookup                                         |
| Workspace in storage | 2-5s    | Download + open SQLiteGraph                               |
| Full cold start      | 10-60s  | Clone + index. MCP notification: "Indexing repository..." |

During cold start, the MCP server responds to `tools/list` and `initialize` immediately (these do not require a graph). The first tool call that needs graph data triggers the lazy initialization, which blocks until the graph is ready. An MCP progress notification is sent to keep the client informed.

### 10. What Gets Persisted (and What Does Not)

| Data                                | Persisted? | Location                    | Rationale                                 |
| ----------------------------------- | ---------- | --------------------------- | ----------------------------------------- |
| Graph nodes, refs, defs, file_index | Yes        | `graph.db` in storage       | Core graph state, expensive to rebuild    |
| Schema (Topology)                   | Yes        | `meta.json` alongside graph | Needed to open SQLiteGraph                |
| Git HEAD commit hash                | Yes        | `meta.json`                 | Staleness detection                       |
| Generation number                   | Yes        | `meta.json`                 | Monotonic ordering                        |
| ley-line embeddings                 | Separate   | `embed.db` in storage       | Optional, large, independently cacheable  |
| Git working tree                    | No         | Ephemeral tmpdir            | Re-cloneable, large, changes constantly   |
| FIFO content cache                  | No         | In-memory only              | Warm cache is nice-to-have, not essential |
| Session state                       | No         | In-memory only              | Sessions are transient                    |

**Meta.json schema:**

```json
{
    "version": 1,
    "repo_url": "https://github.com/org/repo",
    "branch": "main",
    "commit": "a1b2c3d4e5f6",
    "generation": 42,
    "indexed_at": "2026-03-21T14:30:00Z",
    "schema_hash": "sha256:...",
    "node_count": 12847,
    "engine_version": "0.9.0"
}
```

The `engine_version` field enables cache invalidation when mache's ingestion logic changes in ways that produce structurally different graphs. On version mismatch, the cached graph is discarded and rebuilt from source.

### 11. Concurrent Access to Shared Workspaces

Multiple sessions may connect to the same repository simultaneously. The workspace graph is shared, not cloned per session. This is safe because:

1. **Read path:** All Graph interface methods on MemoryStore use `RLock`. Multiple readers execute concurrently.
1. **Re-index path:** Builds a new MemoryStore in a separate goroutine, then atomically swaps via GenerationalGraph. Active readers continue on the old generation.
1. **Write-back path (future):** Serialized through the write pipeline (ADR-0009). Hosted mode initially launches read-only; write-back requires source-level authentication that is out of scope here.

**Session isolation concern:** MCP sessions are stateful (the session ID maps to a graph). But sessions sharing a workspace see the same graph state. If user A triggers a re-index (via webhook) while user B is mid-interaction, user B's subsequent calls see the new graph. This is the correct behavior -- both users should see the latest indexed state.

The one exception is if user B holds node IDs from a previous generation that no longer exist (e.g., a file was deleted in the push that triggered re-index). The tool handler returns `ErrNotFound` and the client retries with updated paths. This is a natural consequence of shared mutable state and is consistent with how filesystem-based tools handle concurrent modifications.

### 12. Security Boundaries

Hosted mache introduces a trust boundary that local mache does not have:

```
Untrusted                          Trusted
─────────                          ───────
User repos (cloned)                Mache server process
  - may contain malicious          - must not execute repo code
    tree-sitter grammars             (tree-sitter parse only, no eval)
  - may contain adversarial        - storage backend credentials
    file paths (traversal)         - webhook secrets
  - may be very large              - session encryption keys
```

**Mitigations:**

- **No code execution:** Mache's ingestion uses tree-sitter for parsing (AST only, no evaluation) and Go templates for rendering (sandboxed, no function calls beyond the registered `json`/`first`/`slice` helpers). No user-supplied code is ever executed.
- **Path sanitization:** All node IDs and file paths are sanitized through `filepath.Clean` and must not contain `..` components. The existing `normalizeID` in MemoryStore strips leading slashes.
- **Resource limits:** Per-workspace memory cap (configurable, default 512MB for graph state), per-workspace node count cap (default 500K), clone timeout (default 120s), index timeout (default 300s).
- **Isolation:** Each workspace has its own tmpdir for git clone, its own MemoryStore instance, and its own SQLite connections. No shared mutable state between workspaces except the WorkspaceManager's routing maps.

### 13. Deployment Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                        Fly.io / Railway                         │
│                                                                 │
│   ┌─────────────────┐    ┌─────────────────┐                   │
│   │  Worker 1        │    │  Worker 2        │                  │
│   │  mache serve     │    │  mache serve     │                  │
│   │  --hosted        │    │  --hosted        │                  │
│   │  --storage r2    │    │  --storage r2    │                  │
│   │                  │    │                  │                   │
│   │  Workspaces:     │    │  Workspaces:     │                  │
│   │   repo-A (2 sess)│    │   repo-C (1 sess)│                  │
│   │   repo-B (1 sess)│    │   repo-D (3 sess)│                  │
│   └────────┬─────────┘    └────────┬─────────┘                  │
│            │                       │                            │
│            └───────────┬───────────┘                            │
│                        │                                        │
│              ┌─────────▼──────────┐                             │
│              │  Cloudflare R2     │                              │
│              │  (graph storage)   │                              │
│              └────────────────────┘                              │
│                                                                 │
│   ┌──────────────────────────────────┐                          │
│   │  Webhook Relay                    │                         │
│   │  (GitHub App receives push events,│                         │
│   │   routes to correct worker)       │                         │
│   └──────────────────────────────────┘                          │
└─────────────────────────────────────────────────────────────────┘
```

Workers are stateless (all persistent state is in R2). A worker can pick up any workspace by downloading the cached graph from storage. Session affinity (sticky sessions via session ID cookie) keeps a user on the same worker for the duration of their interaction, but failover to another worker is possible with a brief cold-start penalty.

**Scaling triggers:**

- Horizontal: spawn new worker when existing workers exceed memory/CPU threshold.
- Vertical: increase worker memory for users indexing very large repositories (monorepos).

## Consequences

### Positive

- **Zero-install experience:** Users add `mcp.rosary.bot` as an MCP endpoint and get structural code intelligence immediately.
- **Reuses existing components:** SQLiteWriter/SQLiteGraph as serialization format, HotSwapGraph for atomic updates, Engine.ReIngestFile for incremental re-indexing. No new serialization format to maintain.
- **Cost-efficient persistence:** Debounced writes to R2 avoid per-change storage costs. Zstd compression reduces storage and transfer costs 8-12x.
- **Graceful degradation:** If storage is unavailable, the server falls back to full cold start. If webhooks fail, the poller catches up. If a worker dies, another worker restores from storage.
- **Shared graphs:** Multiple sessions on the same repo share one MemoryStore, reducing per-user memory cost.

### Negative

- **Memory pressure:** Each active workspace holds a full MemoryStore in RAM. A large repository (100K nodes) consumes ~200-400MB. Workers need sufficient memory for their workspace count.
- **Cold start latency:** First connection to an un-cached repository requires clone + full index (10-60s). This is inherent to the problem -- no way to project a graph without reading the source.
- **HotSwapGraph complexity:** Generation-aware draining adds reference counting and a retired-generations list. A leaked Acquire/Release pair causes a memory leak (old generation never freed). Mitigated by a 5-minute timeout on retired generations.
- **Webhook reliability:** GitHub webhooks are best-effort. Missed webhooks cause staleness until the TTL poller catches up. For paid tier with 30s polling, maximum staleness is 30s.
- **Single-writer serialization:** SQLiteWriter is single-threaded. Persist operations for large graphs (100K+ nodes) may take 2-5 seconds, during which no other persist can run for that workspace. This is acceptable because persists are debounced to every 30s minimum.

### Risks and Mitigations

| Risk                                | Impact                                       | Mitigation                                                                                                    |
| ----------------------------------- | -------------------------------------------- | ------------------------------------------------------------------------------------------------------------- |
| Malicious repo with huge file count | OOM on worker                                | Per-workspace node cap (500K), per-workspace memory cap (512MB), clone depth limit                            |
| Storage backend outage              | Cannot persist, cannot cold-start from cache | In-memory graph continues serving. New connections fall back to full clone+index. Alert on persist failures.  |
| Concurrent Swap + Persist race      | Persist serializes stale graph               | Snapshot isolation: persist clones the node map before serializing, so swaps do not affect in-flight persists |
| Webhook replay / duplicate delivery | Redundant re-index                           | Idempotent: re-indexing a file that has not changed is a no-op (mtime check in Engine)                        |
| Worker crash during persist         | Partial upload visible to other workers      | Storage Put is atomic (write to temp key, then rename/copy). Partial uploads are invisible.                   |

## Phased Approach

### Phase 0: Single-worker, ephemeral only

- `mache serve --hosted` flag with ephemeral workspace lifecycle
- No storage backend, no persistence, no webhooks
- WorkspaceManager with in-memory-only graph registry
- Session routing via MCP ListRoots
- **Gate:** Deploy to a single Fly.io instance, validate end-to-end MCP connection from Claude Desktop

### Phase 1: Persistent storage (R2)

- Implement R2 StorageBackend
- Debounced persistence after index completion
- Cold-start from cached graph
- **Gate:** Measure cold-start latency. Target: \<5s for cached repos, \<60s for uncached.

### Phase 2: Webhooks + incremental re-index

- GitHub App for push event delivery
- WebhookWatcher implementation
- Incremental re-index pipeline (DeleteFileNodes + ReIngestFile per changed file)
- **Gate:** Measure re-index latency for typical push (5-20 changed files). Target: \<2s.

### Phase 3: Multi-worker + auth

- Session affinity via cookie/header
- Authentication (GitHub OAuth, API keys)
- Rate limiting and usage tracking per user
- Horizontal scaling with shared R2 storage
- **Gate:** Load test with 50 concurrent sessions across 20 repos on 3 workers.

### Phase 4: Semantic search + embeddings

- Persist ley-line embedding sidecars alongside graph databases
- Incremental embedding updates (re-embed only changed files)
- Expose semantic search via MCP tool
- **Gate:** Embedding persistence round-trip validated. KNN search returns correct results from restored embeddings.

## References

- ADR-0002: Declarative Topology Schema (defines what gets projected)
- ADR-0004: MVCC Memory Ledger (epoch-based reclamation, generation model)
- ADR-0009: AST-Aware Write Pipeline (incremental file operations)
- Fraser, K. (2004). _Practical lock-freedom._ Epoch-based reclamation.
- MCP Specification: Streamable HTTP transport, session management
- Cloudflare R2: S3-compatible object storage with zero egress fees
