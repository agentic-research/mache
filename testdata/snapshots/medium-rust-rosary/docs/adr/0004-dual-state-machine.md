# ADR-004: Dual State Machine — Bead Flow vs Agent Flow

## Status
Proposed

## Context

Rosary has evolved into two distinct runtime layers:

1. **rsry (Rust)** — system of record for beads, MCP server, Linear sync, scanning, triage
2. **conductor (Elixir/OTP)** — agent lifecycle supervision, ACP protocol, pipeline progression

Both layers track overlapping state. The Rust reconciler (`reconcile.rs`) has an in-memory `BeadTracker` with pipeline phase, retries, and backoff. The Elixir conductor has `Pipeline` structs in GenServer state with the same information. Neither persists across restarts. Both are ephemeral.

Meanwhile, the user interacts with **beads** (via Linear, MCP tools, or CLI), not with agent processes. The user creates a bead, sets priority, maybe assigns an owner. The orchestrator picks it up. Agents are invisible infrastructure — the user sees bead status change from `open` → `dispatched` → `done`.

This creates two distinct state machines that are currently conflated:

### Bead flow (user-facing)
```
user creates bead → backend stores it → orchestrator picks it up → bead transitions states → user sees result
```
The bead state machine is: `backlog → open → queued → dispatched → verifying → done/rejected/blocked`

This is **persistent, cross-repo, visible to humans**. It lives in per-repo Dolt and syncs to Linear.

### Agent flow (infrastructure)
```
orchestrator selects bead → spawns agent → agent works → agent exits → verify → advance pipeline or retry
```
The agent flow is: `spawn → running → exited(success|failure|timeout) → verify → advance|retry|deadletter`

This is **ephemeral within a session, invisible to users**. It's supervision tree internals — which agent is at which pipeline phase, how many retries, what the backoff timer is.

### The problem

These two flows are tangled:
- `BeadTracker` in `reconcile.rs` mixes bead state (generation, status) with agent flow (retries, pipeline phase, backoff)
- The conductor's `Pipeline` struct tracks agent flow but also mutates bead state via rsry MCP calls
- Neither persists agent flow state — restart = lost
- The `external_ref` field in Dolt is overloaded for both Linear linkage AND cross-repo bead references
- Pipeline phase (which agent perspective) is not a property of the bead — it's a property of the current execution attempt

### What we built so far

Phase 1 (`store.rs`, `store_dolt.rs`) introduced three backend traits and a Dolt implementation:
- `HierarchyStore` — decades, threads, bead-to-thread membership (bead flow)
- `DispatchStore` — pipeline state, dispatch history (agent flow)
- `LinkageStore` — cross-repo deps, Linear links (bead flow)

The schema is at `~/.rsry/dolt/rosary/` — separate from per-repo `.beads/` databases.

## Decisions

### 1. Bead flow and agent flow are separate state machines

**Bead state** is owned by per-repo Dolt (`.beads/`). It's the user-facing artifact. Transitions are: status changes, priority changes, assignment, comments. Linear syncs against this.

**Agent flow** is owned by the rosary backend (`~/.rsry/dolt/rosary/`). It's infrastructure. Transitions are: pipeline phase advances, dispatch records, retry counts, backoff timers. The user never sees this directly.

The two connect at defined touch points:
- Bead transitions to `dispatched` when agent flow starts
- Bead transitions to `done` when agent flow completes the pipeline
- Bead transitions to `blocked` when agent flow deadletters
- Bead transitions to `rejected` when agent flow fails verification

### 2. rsry is the persistence layer for both flows

rsry already owns bead CRUD (per-repo Dolt). The backend store (Phase 1) persists agent flow. Both are exposed via MCP tools:

| MCP tool | Flow | Consumer |
|----------|------|----------|
| `rsry_bead_create/close/comment` | bead | user, Linear, conductor |
| `rsry_list_beads` | bead | conductor (triage), user |
| `rsry_pipeline_upsert` (new) | agent | conductor |
| `rsry_pipeline_query` (new) | agent | conductor, debugging |
| `rsry_dispatch_record` (new) | agent | conductor |
| `rsry_dispatch_history` (new) | agent | debugging, analytics |

The conductor writes agent flow state to rsry's backend via MCP. On restart, it reads active pipelines back and resumes.

### 3. The conductor owns execution; rsry owns data

```
                        ┌──────────────────────────────┐
                        │         User / Linear         │
                        └──────────┬───────────────────┘
                                   │ creates/updates beads
                                   ▼
                        ┌──────────────────────────────┐
                        │      rsry (Rust) — data       │
                        │  • bead CRUD (per-repo Dolt)  │
                        │  • backend store (~/.rsry/)   │
                        │  • MCP server                 │
                        │  • Linear sync                │
                        │  • scan + triage scoring      │
                        └──────────┬───────────────────┘
                                   │ MCP tools
                                   ▼
                        ┌──────────────────────────────┐
                        │  conductor (Elixir) — exec    │
                        │  • agent lifecycle (OTP)      │
                        │  • pipeline progression       │
                        │  • ACP/CLI dispatch           │
                        │  • retry/backoff/deadletter   │
                        │  • validation loops           │
                        └──────────────────────────────┘
```

The conductor is a **consumer** of rsry's MCP API. It reads beads to decide what to dispatch, and writes pipeline state + dispatch records back for persistence.

### 4. reconcile.rs becomes scan + triage only

The Rust reconciler's execution code (dispatch, verify, session tracking, BeadTracker) is being replaced by the conductor. What remains:

- **Scan**: discover beads across repos (already works, stays)
- **Triage**: score beads for dispatch priority (stays, exposed via MCP)
- **Status mirroring**: `persist_status()` updates Dolt + Linear (stays)

The dispatch/verify/session/backoff code in `reconcile.rs` becomes dead code as the conductor takes over execution. It can remain as a fallback path (`rsry run` without conductor) or be removed.

### 5. Backend store serves both Rust and Elixir consumers

The `DispatchStore` trait persists agent flow state that the conductor writes via MCP:

```
conductor                    rsry                      backend Dolt
    │                          │                           │
    │──rsry_pipeline_upsert──▶│──DispatchStore.upsert──▶  │
    │                          │                           │
    │◀─rsry_pipeline_query────│◀─DispatchStore.get_all──  │
    │                          │                           │
```

On conductor restart:
1. Read `list_active_pipelines()` from backend
2. For each, check if the agent process is still alive (PID check)
3. If alive, re-attach supervision
4. If dead, decide: resume (next phase) or retry (same phase)

## Consequences

### Positive
- Clean separation: users think in beads, infrastructure thinks in agent flows
- Persistence: both flows survive restarts (Dolt is durable)
- The conductor doesn't need its own database — rsry is the single store
- Linear sync stays simple — it only sees bead state, not agent internals
- Pipeline state becomes queryable (MCP tools) for debugging and analytics

### Negative
- Network hop: conductor → rsry HTTP → Dolt for every state write
- Two processes to run (rsry serve + conductor) instead of one monolith
- reconcile.rs has significant dead code until cleaned up
- Agent flow state is only as fresh as the last MCP write-through

### Risks
- Conductor and rsry could disagree on bead state if MCP calls fail mid-transition
- Two databases (per-repo Dolt + backend Dolt) with no distributed transaction

### Conflict resolution (split by ownership)
- **Bead flow**: backend (per-repo Dolt) is authoritative. Multiple writers (user, Linear, PM agent, conductor). On conflict, re-read from rsry.
- **Agent flow**: conductor is authoritative for in-memory state (sole writer). On divergence, conductor overwrites backend. On restart (no in-memory state), backend is authoritative by default.

### Write ordering at dispatch (linearization point)
The bead status update to `dispatched` is the commit point. The sequence MUST be:
1. Update bead status to `dispatched` in per-repo Dolt (linearization point)
2. Write PipelineState to backend Dolt
3. Spawn agent

If step 1 fails, abort entirely. If step 2 fails after step 1, the bead is dispatched with no pipeline — recovery handles this. If step 3 fails, mark pipeline as `failed`.

### Recovery protocol
On conductor startup:
1. Read `list_active_pipelines()` from backend
2. For each pipeline, read the bead's actual status from rsry
3. Cross-reference:
   - `phase_status = executing` + dead process → retry same phase
   - `phase_status = completed` + no next phase → bead is done, clear pipeline
   - `phase_status = completed` + next phase exists → advance
   - `phase_status = failed` → check retry count, retry or deadletter
   - `phase_status = pending` → dispatch the phase
   - Bead is `done` but pipeline exists → stale pipeline, clear it
   - Bead is `dispatched` with no pipeline → orphaned dispatch, reset to `open`

### Diagnostic comments for stuck beads
When a bead has been `dispatched` for longer than 2x the median dispatch duration, the conductor auto-posts a bead comment:
```
[conductor] Pipeline status for rsry-001:
  Phase: 1/4 (staging-agent)
  Attempt: 3 of 5
  Status: backoff until 14:30 UTC (last failure: test tier)
```
This surfaces agent flow state through the bead interface without breaking the abstraction.

## Resolved Questions

1. **Write-through vs batch?** Write-through for status transitions (dispatched, verifying, done, blocked, rejected). These are fencing points that prevent double-dispatch and enable recovery. Comments can be batched or dropped on failure.

2. **Keep `rsry run`?** Yes, as a single-process fallback for local dev. Both paths share bead state validation — `persist_status()` enforces `valid_transitions()` regardless of caller.

3. **Conductor triage?** Conductor should consume rsry's triage scores via `rsry_list_dispatchable` (Phase 3), not reimplement.

4. **Verification ownership?** Conductor owns verification (it has the work directory and exit status). Verification results are persisted via rsry (bead comments, status transitions).

## Open Questions

1. **Fencing tokens**: Should the conductor write an instance ID to pipeline_state to prevent zombie instances from overwriting state?

2. **RsryClient bottleneck**: The GenServer serializes all MCP calls. At 5+ concurrent agents this becomes a bottleneck. Direct `Req` from each AgentWorker, or connection pool?

## Actors

| Actor | Level | Touches | Creates hierarchy? |
|-------|-------|---------|-------------------|
| **User** | Strategic | Beads (review), ADRs (write), Linear | No |
| **PM agent** | Decade/thread | ADRs → beads via BDR decompose (MCP) | **Yes** |
| **Orchestrator** | Queue | Selects beads, dispatches to execution agents | No |
| **Execution agents** (dev/staging/prod/feature) | Bead | Works one bead, commits, closes | No |

Bead creation paths:
- user → bead (CLI/MCP) — no hierarchy
- user → Linear → bead (webhook sync) — no hierarchy
- user ↔ PM agent → `rsry_decompose` (MCP) → beads + hierarchy — only path that writes to both per-repo Dolt and backend
- orchestrator → bead mutation (status only, not creation)

## Implementation Phases

### Phase 2: Wire pipeline state (next)
- New MCP tools: `rsry_pipeline_upsert`, `rsry_pipeline_query`, `rsry_dispatch_record`, `rsry_dispatch_history`
- Conductor writes pipeline state on every phase transition
- Conductor reads active pipelines on startup

### Phase 3: Triage as MCP service
- `rsry_triage_score` MCP tool — conductor calls this instead of reimplementing
- `rsry_list_dispatchable` — pre-filtered, pre-scored list of beads ready for dispatch

### Phase 4: Hierarchy persistence
- Decades/threads from BDR decompose written to backend
- Linear milestone mapping via HierarchyStore

### Phase 5: Linkage cleanup
- `CrossRepoDep` replaces mirror beads
- `LinearLink` replaces overloaded `external_ref`
- Webhook handler uses `find_by_linear_id()` from backend

### Phase 6: reconcile.rs simplification
- Remove dispatch/verify/session/backoff code from Rust
- Keep scan + triage + status mirroring
- `rsry run` becomes `rsry scan --continuous` or similar
