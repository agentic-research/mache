# ADR-0008: Agent Hierarchy Dispatch Model

**Status:** Proposed
**Date:** 2026-03-20
**Depends on:** ADR-0004 (Dual State Machine)
**Relates to:** ADR-0006 (Declarative Tool Registry), ADR-0007 (BDR Enrichment Pipeline)

## Context

Rosary currently treats all agents as flat peers. `agent_pipeline()` in `dispatch.rs:561`
maps issue types to a sequence of agent perspectives (dev → staging → prod), but every agent
in the pipeline operates at the same privilege level: full write access, direct merge to main,
bead close on agent completion. This creates three problems:

1. **No PR review gate.** `merge_or_pr()` in `workspace.rs:578` fast-forward merges
   task/bug branches directly to main (`git merge --ff-only`). Only `feature` and `epic`
   types push a branch for PR. Agent-authored code bypasses human review for most work.

1. **No composition layer.** A thread (ordered group of related beads) has no agent that
   owns the thread's lifecycle. Each bead is dispatched independently. Nobody verifies that
   the combined output of 5 dev-agent beads in a thread is coherent, compiles together,
   or has consistent APIs.

1. **Flat permissions.** `PermissionProfile` in `dispatch.rs:26` has three levels
   (`ReadOnly`, `Plan`, `Implement`) but these are per-agent-perspective, not per-tier.
   A dev-agent working on a 3-line rename has the same permissions as one refactoring
   a module boundary.

The agent hierarchy formalizes what scope and reign each tier has, and introduces a
composition layer (feature agent) between individual bead work and human review.

## Decisions

### 1. Three-tier agent hierarchy

```
Tier 3: Orchestrator
  ├── All repos, all beads
  ├── Triage, prioritize, dispatch feature agents
  └── Cannot edit code

Tier 2: Feature agents
  ├── Thread-scoped (ordered group of related beads)
  ├── Dispatch dev agents for individual beads
  ├── Compose outputs, open PRs
  └── Cannot merge to main

Tier 1: Dev agents
  ├── Single bead, file-scoped
  ├── Edit, test, commit
  └── Cannot open PRs or merge

Tier 0: Human
  ├── Merge PRs, approve releases
  ├── Create ADRs, set priorities
  └── Override any agent decision
```

**Scope broadens up the hierarchy; detail decreases.** A dev agent knows every line of its
scoped files. A feature agent knows the thread's beads and their relationships but not
individual lines. The orchestrator knows all repos and priorities but not file contents.

This maps to the existing scope/reign model from `project_scope_reign_hierarchy.md`:

- **Scope** = context detail (what the agent sees)
- **Reign** = tool access (what the agent can do)
- Scope and reign vary inversely across tiers

### 2. workspace_merge always pushes branch + opens PR

**Current behavior** (`workspace.rs:631`):

```rust
let needs_pr = matches!(issue_type, "feature" | "epic");
```

Tasks, bugs, and chores fast-forward merge to main without PR review.

**New behavior:** `workspace_merge` always pushes the branch and opens a PR. No agent
ff-merges to main. The human (Tier 0) merges PRs.

**Rationale:** Agent-authored code must be reviewable. Fast-forward merging removes the
review gate. Even for a 3-line task, the PR provides an audit trail and catch point.

**Implementation:** Remove the `needs_pr` branch in `merge_or_pr()`. Always push + open PR.
The function becomes `push_and_pr()`.

### 3. Dev agents checkpoint, feature agents merge

| Operation                   | Tier 1 (Dev)                    | Tier 2 (Feature)                  |
| --------------------------- | ------------------------------- | --------------------------------- |
| `rsry_workspace_checkpoint` | Yes — snapshot work in progress | No                                |
| `rsry_workspace_merge`      | No — cannot push/PR             | Yes — pushes branch, opens PR     |
| `rsry_workspace_create`     | No — feature agent creates it   | Yes — creates worktree for thread |
| `rsry_workspace_cleanup`    | No                              | Yes — after PR merge              |

Dev agents work in a worktree created by the feature agent. They checkpoint their progress
(commit to the branch). The feature agent reviews all checkpoints, runs integration tests
across the thread's beads, then calls `workspace_merge` to push and open a PR.

**Enforcement:** `PermissionProfile` gains a tier dimension. Dev agents get `Implement`
but without workspace_merge approval. Feature agents get workspace_merge but not direct
bead creation.

### 4. Bead closes on PR merge, not agent completion

**Current behavior:** Dev agents call `rsry_bead_close` when they finish work. The bead
is done before any human reviews the code.

**New behavior:** Beads transition through:

```
open → dispatched → checkpointed → pr_open → done
```

- `dispatched`: orchestrator assigned an agent
- `checkpointed`: dev agent committed work (not closed)
- `pr_open`: feature agent pushed branch and opened PR
- `done`: PR merged (human action or auto-merge after CI)

The bead closes when the PR merges, not when the agent finishes. This is detected by:

- GitHub webhook (PR merged event) → update bead status
- Or polling: `rsry scan` checks if branch was merged

**Why:** Agent completion ≠ work completion. An agent can produce code that compiles
and passes tests but is architecturally wrong. The PR review gate catches this.

### 5. Orchestrator dispatches feature agents, not dev agents

**Current behavior** (`reconcile.rs`): The reconciler dispatches dev agents directly
to individual beads.

**New behavior:** The orchestrator (Tier 3) dispatches feature agents to threads.
Feature agents dispatch dev agents to individual beads within the thread. The orchestrator
never dispatches dev agents directly.

```
Orchestrator
  │
  ├── dispatch feature-agent → Thread A (beads 1, 2, 3)
  │     ├── dispatch dev-agent → Bead 1
  │     ├── dispatch dev-agent → Bead 2
  │     └── dispatch dev-agent → Bead 3
  │
  └── dispatch feature-agent → Thread B (beads 4, 5)
        ├── dispatch dev-agent → Bead 4
        └── dispatch dev-agent → Bead 5
```

**Exception:** Standalone beads (not in any thread) get a synthetic single-bead thread.
The feature agent still owns the PR lifecycle, even for one bead.

### 6. Permission profiles by tier

Current `PermissionProfile` (`dispatch.rs:26`):

```rust
pub enum PermissionProfile {
    ReadOnly,
    Plan,
    Implement,
}
```

Extended with tier-aware permissions:

| Permission        | Tier 1 (Dev) | Tier 2 (Feature)  | Tier 3 (Orchestrator) |
| ----------------- | ------------ | ----------------- | --------------------- |
| Read files        | Yes          | Yes               | Yes                   |
| Edit files        | Yes          | No                | No                    |
| Run tests         | Yes          | Yes (integration) | No                    |
| Commit            | Yes          | No                | No                    |
| Checkpoint        | Yes          | No                | No                    |
| Merge/PR          | No           | Yes               | No                    |
| Create beads      | No           | No                | Yes                   |
| Dispatch agents   | No           | Yes (dev only)    | Yes (feature only)    |
| Close beads       | No           | No                | No (PR merge closes)  |
| MCP tools (rsry)  | Read         | Read + dispatch   | All                   |
| MCP tools (mache) | All          | Read              | Read                  |

## Consequences

### Positive

- Every agent change goes through PR review — human stays in the loop
- Feature agents catch cross-bead incoherence before PR
- Clear permission boundaries prevent privilege escalation
- Bead lifecycle tracks actual work completion (PR merge), not agent exit
- Thread-scoped dispatch enables parallel dev agents within a thread

### Negative

- More latency: dev agent → feature agent review → PR → human merge
- Feature agent is new code to write (thread management, PR lifecycle)
- Single-bead tasks still need the feature agent wrapper (overhead)
- Bead state machine gains two new states (`checkpointed`, `pr_open`)

### Risks

- Feature agent becomes a bottleneck if it serializes dev agent dispatch
  (mitigation: feature agent dispatches dev agents in parallel per wave model)
- PR review fatigue if agents produce many small PRs
  (mitigation: feature agent groups related beads into single PR per thread)

## Open Questions

1. Should feature agents run integration tests, or delegate to staging-agent?
1. How does the pipeline (`agent_pipeline()`) change? Is it per-tier or per-thread?
1. Should standalone beads skip the feature agent layer for speed?
1. How do cross-repo threads work when beads span multiple repos?

## References

- `src/dispatch.rs:561` — `agent_pipeline()` current pipeline mapping
- `src/dispatch.rs:26` — `PermissionProfile` enum
- `src/workspace.rs:578` — `merge_or_pr()` current merge logic
- `src/acp.rs:124` — `should_approve()` permission enforcement
- `src/reconcile.rs` — current flat dispatch model
- ADR-0004 — Dual state machine (bead flow vs agent flow)
- Bead `rosary-1284c7` — implementation epic for this ADR
