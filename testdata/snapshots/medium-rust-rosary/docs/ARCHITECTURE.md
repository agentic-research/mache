# Rosary Architecture

Rosary is a cross-repo agent orchestrator that strings beads (per-repo work items), threads (ordered progressions), and decades (ADR-level groupings) into coordinated autonomous development work.

## System Overview

```mermaid
graph TB
    subgraph "Rosary (Rust)"
        CLI[rsry CLI]
        MCP[MCP Server<br/>stdio + HTTP]
        RC[Reconciler]
        SC[Scanner]
        TR[Triage / Queue]
        DI[Dispatcher]
        VE[Verifier]
        WS[Workspace<br/>jj / git worktree]
        HS[HierarchyStore<br/>decades / threads]
    end

    subgraph "Conductor (Elixir/OTP)"
        ORCH[Orchestrator<br/>GenServer]
        SUP[DynamicSupervisor]
        AW[AgentWorker<br/>per-bead GenServer]
    end

    subgraph "Storage"
        D1[Dolt: repo-A/.beads/]
        D2[Dolt: repo-B/.beads/]
        DB[DoltBackend<br/>~/.rsry/dolt/rosary<br/>decades, threads, pipelines]
    end

    subgraph "External"
        LN[Linear API + Webhooks]
        CC[Claude / Gemini / Qwen]
        GH[GitHub]
    end

    CLI --> RC
    MCP --> RC
    MCP --> HS
    ORCH -->|MCP HTTP| MCP
    ORCH --> SUP --> AW
    AW --> CC
    RC --> SC --> D1 & D2
    RC --> TR --> DI --> WS
    RC --> VE
    RC --> HS --> DB
    RC -.-> LN
    AW -.-> GH
```

## Dual Orchestrator

Rosary has two orchestration paths:

```mermaid
graph LR
    subgraph "Rust Reconciler"
        RT[Triage + Scoring]
        RV[Verification Pipeline]
        RS[Dolt Persistence]
        RH[Hierarchy Store]
    end

    subgraph "Elixir Conductor"
        EL[Agent Lifecycle<br/>OTP supervision]
        EP[Pipeline Phases<br/>dev → staging → prod]
        EH[Handoff Writing]
        EM[merge_or_pr]
    end

    RT -->|"MCP: rsry_scan, rsry_status"| EL
    EL -->|"MCP: rsry_bead_close"| RS
    EP --> EH --> EM
```

The Rust reconciler handles triage, verification, and persistence. The Elixir conductor handles agent lifecycle via OTP supervision — instant crash detection (`:DOWN` messages), automatic restart, and pipeline phase advancement as synchronous GenServer state.

## BDR Harmony Lattice

Work is organized in a 3-tier lattice matching OpenAI's Harmony channel model:

```mermaid
graph TB
    DEC[Decade<br/>ADR-level rationale<br/>channel: analysis]
    THR1[Thread: pipeline-quality<br/>channel: commentary]
    THR2[Thread: auth-redesign<br/>channel: commentary]
    B1[Bead: staging review gate<br/>channel: final]
    B2[Bead: test safety linter<br/>channel: final]
    B3[Bead: signet OIDC flow<br/>channel: final]
    B4[Bead: auth middleware<br/>channel: final]

    DEC --> THR1
    DEC --> THR2
    THR1 --> B1
    THR1 --> B2
    THR2 --> B3
    THR2 --> B4
```

| Tier   | BDR Channel  | Visibility              | Dolt Table                   |
| ------ | ------------ | ----------------------- | ---------------------------- |
| Decade | `analysis`   | Internal (architect)    | `decades`                    |
| Thread | `commentary` | Team (developers)       | `threads` + `thread_members` |
| Bead   | `final`      | External (stakeholders) | per-repo `issues`            |

Thread-aware triage: same-thread beads are **sequenced** (dispatched in order), not **suppressed** (false dedup). The reconciler pre-computes a bead→thread map before triage to avoid async borrows.

## Reconciliation Loop

```mermaid
flowchart LR
    SCAN["1. SCAN<br/>Discover beads"]
    VCS["1.5 VCS<br/>jj commit refs"]
    XREF["1.75 XREF<br/>Cross-repo sync"]
    THREAD["2.5 THREAD<br/>Auto-cluster<br/>+ build map"]
    TRIAGE["3. TRIAGE<br/>Score + filter"]
    DISPATCH["4. DISPATCH<br/>Spawn in worktree"]
    VERIFY["5. VERIFY<br/>Tiered checks"]
    ADVANCE["5.5 ADVANCE<br/>Phase + handoff"]

    SCAN --> VCS --> XREF --> THREAD --> TRIAGE --> DISPATCH --> VERIFY --> ADVANCE
```

### Triage Filters (Constraint Stack)

1. State check (must be Open)
1. Severity floor (configurable min priority)
1. Skip epics (planning beads, not actionable)
1. Dependency check (blocked beads deferred)
1. Per-repo busy check (one agent per repo)
1. **Thread sequencing** (same-thread beads wait for thread-mate)
1. Semantic dedup (`epic::is_dominated_by`)
1. **File/directory overlap** (`epic::has_file_overlap` — prefix matching for directory scopes)

## Bead State Machine

```mermaid
stateDiagram-v2
    [*] --> open

    open --> queued : triage selects
    queued --> dispatched : semaphore acquired
    dispatched --> verifying : agent exits
    verifying --> done : all tiers pass
    verifying --> rejected : tier fails
    verifying --> blocked : needs human

    rejected --> open : retry after backoff
    blocked --> open : human /resume

    done --> [*]
```

## Pipeline Phase Advancement

```mermaid
flowchart LR
    DEV[dev-agent<br/>Phase 0]
    STAGE[staging-agent<br/>Phase 1]
    PROD[prod-agent<br/>Phase 2]

    DEV -->|handoff| STAGE -->|handoff| PROD -->|merge_or_pr| DONE[Done]
```

Pipeline per issue type:

| Type            | Pipeline             |
| --------------- | -------------------- |
| bug             | dev → staging        |
| feature         | dev → staging → prod |
| task/chore      | dev                  |
| review          | staging              |
| design/research | architect            |
| epic            | pm                   |

Handoff files (`.rsry-handoff-N.json`) carry summary, files_changed, review_hints, verdict, and thread_id between phases. The workspace is **reused** across phases — each agent sees the previous agent's commits and handoff chain.

## Module Layout

```mermaid
graph LR
    subgraph "Core Loop"
        reconcile["reconcile.rs<br/>loop + triage"]
        scanner["scanner.rs<br/>multi-repo scan"]
        queue["queue.rs<br/>priority queue"]
        dispatch["dispatch.rs<br/>AgentProvider"]
        verify["verify.rs<br/>tiered checks"]
    end

    subgraph "Execution"
        workspace["workspace.rs<br/>jj/git isolation"]
        backend["backend.rs<br/>ComputeProvider"]
        handoff["handoff.rs<br/>phase context"]
        manifest["manifest.rs<br/>dispatch SBOM"]
    end

    subgraph "Data"
        bead["bead.rs<br/>data model"]
        dolt["dolt.rs<br/>MySQL client"]
        pool["pool.rs<br/>RepoPool"]
        epic["epic.rs<br/>clustering + overlap"]
        store["store.rs<br/>HierarchyStore trait"]
        store_dolt["store_dolt.rs<br/>DoltBackend"]
    end

    subgraph "Integration"
        linear["linear.rs<br/>Linear sync"]
        linear_tracker["linear_tracker.rs<br/>IssueTracker"]
        github_mirror["github_mirror.rs<br/>bead → PR comment"]
        sync["sync.rs<br/>bidi engine"]
        xref["xref.rs<br/>cross-repo refs"]
    end

    subgraph "Interface"
        main["main.rs<br/>CLI"]
        serve["serve/<br/>MCP 27 tools + webhooks"]
        config["config.rs<br/>TOML config"]
        plugin["plugin.rs<br/>kind=hook|mcp|dispatch|state_sink"]
    end

    subgraph "Capture & BDR I/O"
        capture["capture.rs<br/>session/code → BeadSpecs"]
        bdr_enrich["bdr_enrich.rs<br/>LLM atom extraction"]
        decompose["decompose.rs<br/>atoms → Rust stubs"]
        scan_assay["scan_assay.rs<br/>stale-ref → chore beads"]
        notes["notes.rs<br/>age recipient rotation"]
    end

    subgraph "BDR (crate)"
        bdr_parse["parse.rs<br/>markdown → atoms"]
        bdr_decompose["decompose.rs<br/>atoms → beads"]
        bdr_provenance["provenance.rs<br/>Doc/Session/Code refs"]
        bdr_harmony["harmony.rs<br/>Harmony tokens"]
        bdr_accrete["accrete.rs<br/>completion → status"]
    end

    reconcile --> scanner --> dolt
    reconcile --> queue --> bead
    reconcile --> dispatch --> workspace --> backend
    reconcile --> verify
    reconcile --> store --> store_dolt
    serve --> pool --> dolt
    linear --> sync --> linear_tracker
    main --> reconcile & serve
```

## Verification Pipeline

Five tiers, first failure short-circuits:

```mermaid
flowchart LR
    T0["Tier 0<br/>Commit exists?"]
    T1["Tier 1<br/>Compile"]
    T2["Tier 2<br/>Test"]
    T3["Tier 3<br/>Lint"]
    T4["Tier 4<br/>Diff Sanity<br/>≤10 files, ≤500 lines"]

    T0 -->|pass| T1 -->|pass| T2 -->|pass| T3 -->|pass| T4 -->|pass| DONE["done"]
    T0 -->|fail| REJECT["rejected"]
    T1 -->|fail| REJECT
    T2 -->|fail| RETRY["retry"]
    T3 -->|fail| RETRY
    T4 -->|fail| BLOCK["blocked"]
```

Language-aware: Rust gets `cargo check/test/clippy`, Go gets `go vet/test/golangci-lint`.

## Dolt Connection Model

Two tiers of Dolt databases:

```mermaid
graph TB
    subgraph "Per-Repo (beads)"
        R1["rosary/.beads/dolt/rosary"]
        R2["mache/.beads/dolt/mache"]
        RN[".../.beads/dolt/..."]
    end

    subgraph "Orchestrator (hierarchy)"
        BE["~/.rsry/dolt/rosary<br/>decades, threads,<br/>thread_members,<br/>pipelines, dispatches,<br/>linear_links"]
    end

    POOL["RepoPool"] --> R1 & R2 & RN
    HS["HierarchyStore"] --> BE
```

Connection safety: `dolt_transaction_commit=1` (auto-commit per statement), `max_connections=1` (session variable consistency), bail on known dead port (no silent empty DB).

## MCP Tools (27)

| Category   | Tools                                                                                                                                              |
| ---------- | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| Beads      | `rsry_bead_create`, `rsry_bead_update`, `rsry_bead_search`, `rsry_bead_comment`, `rsry_bead_close`, `rsry_bead_link`, `rsry_bead_import`           |
| Status     | `rsry_status`, `rsry_list_beads`, `rsry_scan`, `rsry_active`                                                                                       |
| Dispatch   | `rsry_dispatch`, `rsry_run_once`, `rsry_decompose`, `rsry_pipeline_upsert`, `rsry_pipeline_query`, `rsry_dispatch_record`, `rsry_dispatch_history` |
| Workspaces | `rsry_workspace_create`, `rsry_workspace_checkpoint`, `rsry_workspace_cleanup`, `rsry_workspace_merge`                                             |
| Hierarchy  | `rsry_decade_list`, `rsry_thread_list`, `rsry_thread_assign`                                                                                       |
| Repos      | `rsry_repo_register`, `rsry_repo_list`                                                                                                             |

## Linear Integration

Bidirectional sync with sub-issue projection:

| BDR Tier | Linear Entity | `linear_type` |
| -------- | ------------- | ------------- |
| Decade   | Project       | —             |
| Thread   | Parent Issue  | `issue`       |
| Bead     | Sub-Issue     | `sub_issue`   |

Beads with thread assignments sync as sub-issues of the thread's parent issue. Beads without threads create flat issues (backwards compatible).

## File/Directory Scoping

All bead types require scopes for parallel dispatch:

- **Files**: `src/reconcile.rs` (exact path)
- **Directories**: `crates/bdr/` (trailing slash = prefix match)
- **Repo-wide**: `./` (blocks all dispatch in that repo)

`has_file_overlap()` uses prefix matching — `crates/bdr/` overlaps `crates/bdr/src/harmony.rs`. This enables design beads to scope to subtrees while implementation beads scope to exact files.

## Stopping Conditions

| Condition            | Default | Scope                      |
| -------------------- | ------- | -------------------------- |
| Max retries per bead | 5       | Per-bead, then deadletter  |
| Consecutive reverts  | 3       | Per-bead, then deadletter  |
| Agent timeout        | 10 min  | Per-dispatch, kill process |

## Design Influences

- **Kubernetes controllers**: desired state reconciliation, generation tracking
- **driftlessaf** (Chainguard): workqueue with priority, NotBefore scheduling, exponential backoff
- **beads** (steveyegge): AI-native issue tracking, Dolt-backed
- **OpenAI Symphony**: OTP supervision patterns, Elixir conductor
- **OpenAI Harmony**: 3-channel progressive disclosure (analysis/commentary/final → decade/thread/bead)
