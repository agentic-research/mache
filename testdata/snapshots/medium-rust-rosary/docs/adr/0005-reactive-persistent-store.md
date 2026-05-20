# ADR-005: Reactive Persistent Store for Agent IPC

## Status

Proposed

## Context

Rosary's current data layer is Dolt (SQL + Git). MCP tools write bead state to Dolt, but nothing reacts — the reconciler polls for status changes, field updates don't propagate to backends, and agents can't subscribe to each other's state. The Elixir conductor polls for pipeline transitions.

This creates three problems:

1. **Latency**: Changes are only visible after the next poll cycle.
2. **Coupling**: Every new "reaction" (sync to Linear, advance pipeline, notify agent) requires adding code to the reconciler's polling loop.
3. **Scale ceiling**: Dolt serializes all writes at ~300/sec. With 50-200 concurrent agents, this becomes a bottleneck.

The desired model is "local Firebase" — a reactive, persistent database where writes trigger events that propagate to subscribers. Agents communicate through the database, not through direct IPC.

## Use Case Definition

| Requirement | Detail |
|---|---|
| Concurrent writers | 50-200 agents, strong consistency |
| Reactive events | Write triggers immediate notification to subscribers |
| Subscribers | Elixir conductor, Linear/GitHub sync (via IssueTracker), other agents |
| Persistence | Crash-safe, survives restarts |
| Local-first | Single machine, no cloud dependency |
| Federation | Multi-instance sync via git remotes (.github profile discovery) |
| Audit | Full history of every state change |
| Embeddable | Rust-native, ideally in-process |
| Pipeline IPC | Phase transitions are DB writes that trigger next dispatch |

## Decision Drivers

| Requirement | Weight | Notes |
|---|---|---|
| Strong consistency | Must | Agent B must see Agent A's bead close immediately for dependency resolution |
| Reactive events | Must | Write -> immediate notification (no polling) |
| Persistence | Must | Survives restarts, crash-safe |
| Local-first | Must | Single machine, no cloud dependency |
| Embeddable in Rust | Should | In-process preferred over separate server |
| Federation | Should | Multi-instance sync via git remotes or P2P |
| Git-style branching | Nice | Branch-per-agent, cell-level merge. Or time-travel may suffice |
| Standard SQL | Nice | Migration path from Dolt. SurrealQL acceptable if gains justify |
| Audit trail | Must | Full history of every state change |
| 50-200 concurrent writers | Must | Without serialization bottleneck |

## Candidates Evaluated

### Option A: SurrealDB 3.0 (Rust, reactive-first)

**Strengths:**
- `LIVE SELECT` — core reactive primitive, sub-ms in embedded mode
- `DEFINE EVENT` — triggers on write, can fire HTTP webhooks
- Change Feeds — CDC pull for batch sync
- Rust-native, embeddable as library
- ACID with serializable isolation
- Record-level access control per agent

**Weaknesses (dealbreakers in bold):**
- **No federation** — zero sync between instances, no push/pull/remotes
- **Durability defaults dangerous** — no fsync unless SURREAL_SYNC_DATA=true
- **3.0 stability** — build hangs (#6954), WHERE regression 1ms->2000ms (#6800), large SELECT hangs (#7037)
- **No branching** — time-travel is read-only, no branch/merge/diff
- LIVE SELECT is single-node only (#5070)
- SurrealQL, not standard SQL

### Option B: Turso/libSQL (Rust, SQLite-compatible)

**Strengths:**
- Full SQLite compatibility
- Copy-on-Write branches, MVCC
- Rust rewrite in progress, concurrent writes (4x SQLite, tech preview)
- `update_hook` for in-process reactivity

**Weaknesses:**
- Reactivity is in-process only (no cross-process subscriptions)
- CoW branches, not full git-style merge
- Beta (Rust rewrite)

### Option C: Dolt + Event Sidecar (NATS/Redis)

**Strengths:**
- Keep everything that works today
- Dolt handles versioning, branching, federation

**Weaknesses:**
- Two systems to operate
- Still limited to ~300 writes/sec serialized

### Option D: Hybrid SurrealDB (hot) + Dolt (cold)

SurrealDB for real-time IPC, Dolt for versioned snapshots and federation.
Complexity of two databases + sync layer.

### Option E: Extend Ley-Line (preferred direction)

**This is the discovery that changes the calculus.** Ley-line already has:

| Capability | Status | Detail |
|---|---|---|
| SQLite (SQL) | Production | rusqlite 0.34, bundled, serialize feature |
| jj integration | Production | Sidecar pattern: every write auto-snapshots to jj (debounced 500ms) |
| Zero-copy arena | Production | sqlite3_deserialize, double-buffered mmap, atomic generation swaps |
| Generation-based change detection | Production | AtomicU64 counter, readers detect swaps, stale pool eviction |
| sqlite-vec (embeddings) | Production | AllMiniLM-L6-v2 via fastembed, KNN search |
| Content-addressing | Production | SHA-256 manifests, Ed25519 signing, ML-DSA-44 post-quantum |
| Lock-free reader pool | Production | crossbeam::ArrayQueue, 2-8 readers, ~16MB target |
| C FFI for mache | Production | cbindgen header, UDS control socket |
| Tree-sitter AST projection | Production | Go, Python, Rust, HTML, JSON, YAML, etc. |

**What's missing to make ley-line the reactive agent IPC store:**

1. **Write path for beads** — currently read-heavy (mache pushes). Need a write API for bead state mutations from agents.
2. **Subscription/notification** — generation counter detects changes but no pub/sub for "bead X changed." Add SQLite `update_hook` or channel-based notification.
3. **Multi-writer support** — single `Mutex<SqliteGraph>` writer. For 50+ agents, need concurrent writes or write-through-Elixir serialization.
4. **Dolt bridge** — beads currently live in Dolt. Either ley-line reads/writes Dolt, or beads migrate into ley-line's SQLite arena with Dolt as federation export.

**Architecture with ley-line:**
```
Agent -> rsry MCP tool -> ley-line SQLite write (in-process, zero-copy)
  -> jj auto-snapshot (existing, debounced 500ms)
  -> generation counter bump (existing)
  -> update_hook / channel -> Elixir conductor (new: subscription)
  -> update_hook / channel -> IssueTracker sync (new: backend push)

Federation:
  ley-line SQLite arena -> jj snapshot -> git remote push (existing VCS path)
  Content-addressed manifests -> signet identity -> GitHub refs (existing crypto)
```

## Federation Model

Inspired by octo-sts: trust policies at well-known paths in profile repos.

```
github.com/{user}/.github/
  rosary.toml              # repo list, federation config
  rosary/
    {name}.trust.yaml      # octo-sts-style identity -> permission mapping

refs/rosary/beads          # bead database snapshots (custom git ref)
  OR
refs/dolt/data             # if keeping Dolt for federation layer
```

**Key insight from Dolt v1.81.10 (Feb 2026):** Dolt databases can use Git repositories as remotes via `refs/dolt/data` — invisible to normal git, fully accessible to Dolt tools. A repo serves double duty: source code (normal refs) + bead database (dolt ref).

**Discovery:** To federate with user Alice, read `github.com/alice/.github/rosary.toml`.
**Sync:** Push/pull bead snapshots via custom git refs or Dolt remotes.
**Identity:** Signet keys for verification, ley-line content-addressing for integrity.

## Rosary as a Linear Agent

Rosary registers as a **Linear Agent** via the Agent API (Developer Preview):

- OAuth app with `actor=app` — dedicated identity in Linear workspace, no seat consumed
- Scopes: `read,write,app:assignable,app:mentionable`
- **Agent Sessions**: created on issue assignment or @mention
  - Must respond within 10 seconds (webhook -> reactive store -> dispatch)
  - Progress updates via `agentActivityCreate` mutation (status, tool calls, reasoning)
  - Plans via `agentSessionUpdate` mutation (plan items: pending/inProgress/completed)
  - PR links via `externalUrls` field -> unlocks Linear's PR integration UI

**Reactive store enables this:** Linear webhook fires -> reactive store writes event -> ley-line generation bumps -> Elixir conductor sees new session -> dispatches agent -> agent writes progress to reactive store -> Linear sync layer pushes `agentActivityCreate` -> Linear UI updates in real-time.

Without a reactive store, the 10-second response requirement forces polling or complex webhook-to-dispatch wiring. With it, the data flow is natural: webhook writes, conductor subscribes, agent dispatches.

**Linear as PM layer projection (not source of truth):**
- Beads in ley-line/Dolt are the system of record
- Linear shows the human view: kanban, triage, assignment
- Rosary appears as an agent peer in Linear — assignable, mentionable, shows progress
- Bidirectional: Linear webhook -> bead state, bead state change -> Linear update

## Semantic Merge via Mache AST Projection

A key capability enabled by this architecture: **structural merges via AST projection.**

Mache projects code as AST structures (symbols, call graphs, type hierarchies) into the same SQLite `nodes` table that ley-line hosts. If bead state includes structural code references (via blast radius derivation), the reactive store can detect structural conflicts:

- Agent A modifies function `foo()` signature
- Agent B calls `foo()` with old signature
- Line-based merge succeeds (different files), but the code is broken

With mache's AST in the same store as bead state, a merge validator queries the call graph to detect this. Merging becomes a graph query, not a text diff.

## Deployment Options

| Mode | Store | Federation | Linear |
|---|---|---|---|
| Local dev | ley-line embedded in rsry | jj -> git remote push | Webhook via tunnel |
| Sprites instance | ley-line server on persistent volume | GitHub <-> Sprites bidirectional | Direct webhook endpoint |
| Team | ley-line + Elixir conductor | Shared instance, profile repo discovery | Linear Agent (OAuth app) |

The Sprites model: a persistent Sprites instance IS the "firebase" — always running, agents connect to it, it pushes snapshots to GitHub for backup/federation, Linear agent sessions respond in real-time.

## Migration Path

1. `rsry_bead_update` lands with current Dolt backend (immediate value, unblocks backfill)
2. Add `IssueTracker::update_fields()` to trait (backend-agnostic field sync)
3. Prototype ley-line write path for bead state (new `nodes` schema or separate `beads` table in arena)
4. Add subscription mechanism (SQLite `update_hook` -> channel -> Elixir)
5. Dual-write: MCP tools write Dolt + ley-line during transition
6. Validate reactive pipeline: webhook -> ley-line write -> conductor dispatch -> agent -> progress -> Linear
7. Register rosary as Linear Agent (OAuth app, webhook handler)
8. If proven: ley-line becomes primary, Dolt becomes federation export

## Consequences

**Positive:**
- Build on existing infrastructure (ley-line, mache, jj) rather than introducing new systems
- Agents communicate through data, not direct IPC
- Every write is an event — no separate event system
- Linear integration becomes real-time via Agent API
- Semantic merge via shared AST + bead store

**Negative:**
- ley-line needs significant new capabilities (write path, subscriptions, multi-writer)
- Migration period with dual-write complexity
- jj snapshot every 500ms may be too frequent for high-write agent IPC

**Risks:**
- Single-writer Mutex in ley-line may not scale to 50+ concurrent agents without architectural change
- ley-line is currently optimized for read-heavy mache workloads, not write-heavy agent IPC
- Linear Agent API is Developer Preview — APIs may change before GA
