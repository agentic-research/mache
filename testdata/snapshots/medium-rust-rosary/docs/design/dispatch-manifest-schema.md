# Dispatch Manifest Schema (.rsry-dispatch.json)

A structured record of what an agent did during a dispatch. Written to the
workspace directory by the orchestrator after agent completion, before
checkpoint and cleanup.

## Motivation

Today, understanding what happened during a dispatch requires forensic
reconstruction: parsing git log, diffing file trees, grepping stderr.
The existing `DispatchRecord` in Dolt tracks the relational minimum
(who, when, outcome) but not the substance (what changed, at what cost,
with what quality).

The manifest bridges this gap. It is the dispatch SBOM — a single JSON
file that answers every question about a completed dispatch without
touching git, Dolt, or the orchestrator's memory.

## Location

```
{workspace_dir}/.rsry-dispatch.json
```

Workspace directories follow the existing convention:
- Git worktree: `{repo_parent}/.rsry-workspaces/{bead_id}/`
- jj workspace: `{repo_parent}/.rsry-workspaces/{bead_id}/`
- In-place: `{repo_path}/` (single-concurrency fallback)

The manifest is written **after** the agent exits but **before** checkpoint
(git commit / jj commit). This means the manifest itself is included in
the checkpoint commit, creating a self-describing work product.

## Schema Version

```
"schema_version": "1"
```

Integer string. Bump on breaking changes. Consumers must check this field
before parsing. Additive fields do not require a version bump.

---

## Concrete Schema

```json
{
  "schema_version": "1",

  "identity": {
    "dispatch_id": "d-550e8400-e29b-41d4-a716-446655440000",
    "bead_id": "rsry-8c31a5",
    "repo": "rosary",
    "agent": "dev-agent",
    "provider": "claude",
    "model": "claude-sonnet-4-20250514",
    "pipeline_phase": 0,
    "issue_type": "bug",
    "permission_profile": "implement"
  },

  "session": {
    "session_id": "abc123-def456-...",
    "workspace_path": "/Users/jg/remotes/art/.rsry-workspaces/rsry-8c31a5",
    "work_dir": "/Users/jg/remotes/art/.rsry-workspaces/rsry-8c31a5",
    "repo_path": "/Users/jg/remotes/art/rosary",
    "vcs_kind": "git",
    "started_at": "2026-03-15T02:30:00Z",
    "completed_at": "2026-03-15T02:34:17Z",
    "duration_ms": 257000,
    "pid": 48291
  },

  "work": {
    "commits": [
      {
        "sha": "a1b2c3d",
        "message": "fix(rsry-8c31a5): correct timeout handling in reconcile loop",
        "author": "Claude <noreply@anthropic.com>"
      }
    ],
    "files_changed": [
      "src/reconcile.rs",
      "src/reconcile_test.rs"
    ],
    "lines_added": 47,
    "lines_removed": 12,
    "diff_stat": "2 files changed, 47 insertions(+), 12 deletions(-)"
  },

  "quality": {
    "verification_passed": true,
    "highest_passing_tier": 6,
    "tiers": [
      { "name": "commit", "result": "pass" },
      { "name": "bead_ref", "result": "pass" },
      { "name": "compile", "result": "pass" },
      { "name": "test", "result": "pass" },
      { "name": "lint", "result": "pass" },
      { "name": "diff-sanity", "result": "pass" },
      { "name": "review", "result": "pass" }
    ]
  },

  "cost": {
    "total_cost_usd": 0.042,
    "input_tokens": 18500,
    "output_tokens": 3200,
    "cache_read_tokens": 12000,
    "cache_write_tokens": 5000,
    "num_turns": 7
  },

  "vcs": {
    "jj_change_id": "kpqvtsomyxwz",
    "git_branch": "fix/rsry-8c31a5",
    "bookmark": "fix/rsry-8c31a5",
    "base_commit": "858f18a",
    "head_commit": "a1b2c3d"
  },

  "outcome": {
    "success": true,
    "bead_closed": true,
    "stop_reason": "end_turn",
    "agent_closed_via_mcp": true,
    "error": null,
    "retries": 0,
    "deadlettered": false
  }
}
```

---

## Field-by-Field Population Guide

### identity

| Field | Source | When Available | Notes |
|-------|--------|----------------|-------|
| `dispatch_id` | Orchestrator generates UUID v4 | At spawn | Same as `DispatchRecord.id` in store.rs |
| `bead_id` | `Bead.id` from Dolt scan | At spawn | The work item being addressed |
| `repo` | `Bead.repo` | At spawn | Repo name from rosary.toml |
| `agent` | `Bead.owner` or `dispatch::default_agent()` | At spawn | Agent perspective (dev-agent, staging-agent, etc.) |
| `provider` | `AgentProvider::name()` | At spawn | "claude", "gemini", or "acp" |
| `model` | Stream-json `init` event: `model` field | After init event parsed | Not available at spawn; backfill from stream-json |
| `pipeline_phase` | `PipelineState.pipeline_phase` | At spawn | 0=dev, 1=staging, 2=prod |
| `issue_type` | `Bead.issue_type` | At spawn | bug, feature, task, chore, review, etc. |
| `permission_profile` | Derived from issue_type in `dispatch::spawn()` | At spawn | "read_only", "implement", or "plan" |

**Population timing:** All identity fields except `model` are known at dispatch
time. The orchestrator pre-populates identity in memory when spawning the agent.
`model` is extracted from the stream-json `init` event (or left null for
providers that do not emit stream-json).

### session

| Field | Source | When Available | Notes |
|-------|--------|----------------|-------|
| `session_id` | Stream-json `init` event: `session_id` or ACP `session/new` response | After agent starts | Enables `claude --resume` for interrupted dispatches |
| `workspace_path` | `Workspace.work_dir` | At spawn | Absolute path to the isolated worktree |
| `work_dir` | `AgentHandle.work_dir` | At spawn | Same as workspace_path (kept for DispatchRecord compat) |
| `repo_path` | `Workspace.repo_path` | At spawn | Original repo root (not the worktree) |
| `vcs_kind` | `Workspace.vcs` (Jj/Git/None) | At spawn | Lowercase string: "jj", "git", "none" |
| `started_at` | `AgentHandle.started_at` | At spawn | ISO 8601 UTC |
| `completed_at` | `chrono::Utc::now()` when exit detected | At completion | ISO 8601 UTC |
| `duration_ms` | Stream-json `result` event, or `completed_at - started_at` | At completion | Prefer stream-json (measures actual agent time, not wall clock) |
| `pid` | `AgentSession::pid()` | At spawn | OS process ID; null for non-subprocess providers |

**Population timing:** Most session fields are available at spawn. `session_id`
arrives with the first stream-json event or ACP session response. `completed_at`
and `duration_ms` are filled at exit.

### work

| Field | Source | When Available | Notes |
|-------|--------|----------------|-------|
| `commits` | `git log --format=...` in workspace | After checkpoint | Array of {sha, message, author}. May be empty (agent made no changes). The checkpoint commit is included. |
| `files_changed` | `git diff --name-only {base}..HEAD` | After checkpoint | Relative paths. Computed against `base_commit`. |
| `lines_added` | `git diff --numstat {base}..HEAD` | After checkpoint | Sum of additions across all files |
| `lines_removed` | `git diff --numstat {base}..HEAD` | After checkpoint | Sum of deletions across all files |
| `diff_stat` | `git diff --stat {base}..HEAD` (last line) | After checkpoint | Human-readable summary string |

**Population timing:** All work fields are populated *after* the orchestrator
runs `workspace.checkpoint()` (which does `git add -A && git commit` or
`jj commit`). The orchestrator runs git commands in the workspace to extract
these fields.

**Important:** `commits` captures the full chain of commits the agent made in
the worktree, not just the checkpoint commit. If the agent was instructed not
to commit (current behavior for Rust reconciler), the only commit is the
orchestrator's checkpoint. If the agent does commit (conductor mode), those
commits appear too.

### quality

| Field | Source | When Available | Notes |
|-------|--------|----------------|-------|
| `verification_passed` | `VerifySummary::passed()` | After verification | Boolean. True if all tiers passed OR if agent-closed via MCP (fast path). |
| `highest_passing_tier` | `VerifySummary.highest_passing_tier` | After verification | Index into the tiers array. null if nothing passed. |
| `tiers` | `VerifySummary.results` | After verification | Array of {name, result, detail?}. Only tiers that ran are included (verification short-circuits on first failure). |

**Tier result values:**
- `"pass"` -- VerifyResult::Pass
- `"fail"` -- VerifyResult::Fail(reason), detail field contains the reason
- `"partial"` -- VerifyResult::Partial(reason), detail field contains the reason

**Agent-closed fast path:** When the agent closes the bead via MCP
(`is_bead_agent_closed` returns true), verification is skipped entirely.
In this case:
- `verification_passed`: true
- `highest_passing_tier`: null
- `tiers`: `[{"name": "agent_self_closed", "result": "pass"}]`

This distinction is important for audit: "passed verification" and "skipped
verification because agent self-reported done" are fundamentally different
confidence levels.

### cost

| Field | Source | When Available | Notes |
|-------|--------|----------------|-------|
| `total_cost_usd` | Stream-json `result` event: `total_cost_usd` | At completion | null for providers that do not report cost |
| `input_tokens` | Stream-json `result` event: `usage.input_tokens` | At completion | Cumulative across all turns |
| `output_tokens` | Stream-json `result` event: `usage.output_tokens` | At completion | Cumulative across all turns |
| `cache_read_tokens` | Stream-json `result` event: `usage.cache_read_input_tokens` | At completion | Prompt caching; null if not reported |
| `cache_write_tokens` | Stream-json `result` event: `usage.cache_creation_input_tokens` | At completion | Prompt caching; null if not reported |
| `num_turns` | Stream-json `result` event: `num_turns` | At completion | Number of assistant turns (tool-use loops) |

**Population strategy:** The orchestrator must capture the agent's stdout
(stream-json output) to extract cost fields. Currently, `ClaudeProvider`
sends stdout to `Stdio::null()`. This is the primary change required:
redirect stdout to a file or pipe, parse the final `result` event.

For providers that do not emit stream-json (Gemini, ACP), cost fields are
null. ACP agents can report cost through the ACP protocol's
`prompt/complete` notification, which the conductor already handles.

### vcs

| Field | Source | When Available | Notes |
|-------|--------|----------------|-------|
| `jj_change_id` | `workspace.jj_change_id()` (reads `jj log -r @- -T change_id`) | After checkpoint | null for git-only repos or no-VCS mode |
| `git_branch` | Deterministic: `format!("fix/{}", bead_id)` | At spawn | The branch created by `create_git_worktree()` |
| `bookmark` | Deterministic: `format!("fix/{}", bead_id)` | After checkpoint | jj bookmark created by `workspace.jj_bookmark()` |
| `base_commit` | `git rev-parse HEAD` before agent runs, or `git merge-base` | At spawn | The commit the worktree branched from. Critical for accurate diff. |
| `head_commit` | `git rev-parse HEAD` after checkpoint | After checkpoint | The final commit SHA in the worktree |

**Population timing:** `git_branch` and `base_commit` are known at spawn.
`jj_change_id`, `bookmark`, and `head_commit` are populated after checkpoint.

### outcome

| Field | Source | When Available | Notes |
|-------|--------|----------------|-------|
| `success` | `AgentSession::wait()` return value + verification | At completion | True if agent exited 0 AND (verification passed OR agent-closed) |
| `bead_closed` | `is_bead_agent_closed()` or verification pass | At completion | Whether the bead transitioned to done/closed |
| `stop_reason` | Stream-json `result` event: `stop_reason`, or ACP `prompt/complete` | At completion | "end_turn", "max_tokens", "refusal", "cancelled", "tool_use", "timeout" |
| `agent_closed_via_mcp` | `is_bead_agent_closed()` check | At completion | True if the agent called `rsry_bead_close` during execution |
| `error` | Error message from agent stderr, verification failure detail, or null | At completion | null on success. On failure: first verification failure reason, or exit code description |
| `retries` | `BeadTracker.retries` or `Pipeline.retries_used()` | At completion | Number of retries consumed for this dispatch (not cumulative) |
| `deadlettered` | `WorkQueue.is_deadlettered()` check | At completion | Whether this dispatch exhausted retries and the bead was deadlettered |

---

## Population Flow

The manifest is assembled in three phases:

### Phase 1: At Spawn (dispatch::spawn or conductor::AgentWorker.init)

Pre-populate fields that are known before the agent starts:

```
identity: dispatch_id, bead_id, repo, agent, provider, pipeline_phase,
          issue_type, permission_profile
session:  workspace_path, work_dir, repo_path, vcs_kind, started_at, pid
vcs:      git_branch, base_commit
```

Store this partial manifest in memory (Rust: alongside the AgentHandle;
Elixir: in the GenServer state).

### Phase 2: During Execution (stream-json / ACP events)

Extract fields from the agent's output stream as they arrive:

```
identity: model (from init event)
session:  session_id (from init event or ACP session/new)
cost:     all fields (from result event, final line of stream-json)
outcome:  stop_reason (from result event or ACP prompt/complete)
```

This requires capturing the agent's stdout. The init event arrives first
and yields session_id + model. The result event arrives last and yields
cost + stop_reason.

### Phase 3: After Completion (checkpoint + verify)

Fill in remaining fields from git state and verification results:

```
session:  completed_at, duration_ms
work:     commits, files_changed, lines_added, lines_removed, diff_stat
quality:  verification_passed, highest_passing_tier, tiers
vcs:      jj_change_id, bookmark, head_commit
outcome:  success, bead_closed, agent_closed_via_mcp, error, retries,
          deadlettered
```

Then write `.rsry-dispatch.json` to the workspace directory.

---

## Relationship to Existing Types

```
DispatchRecord (store.rs)     .rsry-dispatch.json
================================  ==============================
id                            --> identity.dispatch_id
bead_ref.repo                 --> identity.repo
bead_ref.bead_id              --> identity.bead_id
agent                         --> identity.agent
provider                      --> identity.provider
started_at                    --> session.started_at
completed_at                  --> session.completed_at
outcome                       --> outcome.success (expanded)
work_dir                      --> session.work_dir
session_id                    --> session.session_id
workspace_path                --> session.workspace_path
                              NEW: identity.model
                              NEW: work.* (all git-derived fields)
                              NEW: quality.* (all verification fields)
                              NEW: cost.* (all token/cost fields)
                              NEW: vcs.* (change_id, branch, bookmark)
                              NEW: outcome.stop_reason, error, etc.
```

The manifest is a strict superset of DispatchRecord. The orchestrator can
populate DispatchRecord from the manifest, making the manifest the single
source of truth for "what happened during this dispatch."

---

## Open Questions for Implementation

1. **Stdout capture:** ClaudeProvider currently sends stdout to null.
   Switching to a file (e.g., `{workspace}/.rsry-stream.jsonl`) lets
   us parse cost/session without buffering the entire stream in memory.
   The stream file itself becomes a valuable debug artifact.

2. **Manifest before or after checkpoint commit?** If we write the manifest
   before checkpoint, it gets included in the commit (self-describing).
   If after, it is not in the commit but has the final SHA. Recommendation:
   write it before checkpoint so it is part of the committed work product.
   The `head_commit` field can be backfilled by re-reading after checkpoint,
   or the manifest can be amended.

3. **Conductor (Elixir) parity:** The conductor has richer runtime
   information (ACP messages, mid-execution validation results). Its
   manifest could include additional fields (tool_calls, validation_runs).
   These should be optional fields in the schema, not a separate schema.

4. **Retention:** Manifests in workspaces are ephemeral (cleaned up with
   teardown). For long-term storage, the orchestrator should copy the
   manifest into Dolt (a `dispatch_manifests` table) or append it to
   `{repo}/.beads/dispatches.jsonl` before cleanup.

5. **Provenance chain:** For multi-phase pipelines (dev -> staging -> prod),
   each phase produces its own manifest. The `pipeline_phase` field
   distinguishes them. A `previous_dispatch_id` field could chain them
   explicitly, but pipeline phase + bead_id is sufficient for querying.
