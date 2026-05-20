# Structured Handoff Pipeline: Fresh Context + Adversarial Review

## Summary

The dispatch pipeline today runs multiple agents sequentially but passes no structured context between them. Each phase gets the original bead description and whatever's on disk — no "here's what I did, here's what to watch for." This design adds structured handoffs, cross-model adversarial review, and PR-as-terminal-step.

## Architecture

```
Bead dispatch
  │
  ▼
┌─────────────────────────────────────────────────────┐
│ Phase 0: dev-agent (Claude)                         │
│  - Reads bead description + codebase via mache      │
│  - Writes code, runs tests                          │
│  - Orchestrator writes .rsry-handoff.json           │
│  - Orchestrator writes .rsry-dispatch.json (SBOM)   │
│  - Orchestrator captures .rsry-stream.jsonl (log)   │
└──────────────┬──────────────────────────────────────┘
               │ handoff
               ▼
┌─────────────────────────────────────────────────────┐
│ Phase 1: staging-agent (Gemini — adversarial)       │
│  - Reads .rsry-handoff.json for context             │
│  - Uses mache to structurally review dev's changes  │
│  - Can read .rsry-stream.jsonl to see dev's process │
│  - Runs tests adversarially (edge cases, mocks)     │
│  - Writes review verdict to handoff                 │
└──────────────┬──────────────────────────────────────┘
               │ handoff
               ▼
┌─────────────────────────────────────────────────────┐
│ Phase 2: prod-agent (Claude — module-level review)  │
│  - Reads both prior handoffs                        │
│  - Checks architecture, blast radius, dependencies  │
│  - Final quality gate                               │
└──────────────┬──────────────────────────────────────┘
               │ pass
               ▼
┌─────────────────────────────────────────────────────┐
│ Terminal: PR creation                                │
│  - Orchestrator squash-merges worktree → PR branch  │
│  - gh pr create with handoff chain as PR body       │
│  - Human reviews in GitHub/Linear                   │
│  - Merge closes the bead                            │
└─────────────────────────────────────────────────────┘
```

## Handoff Artifact

Written by the orchestrator (not the agent) after each phase completes. Lives in the workspace.

```json
{
  "schema_version": "1",
  "phase": 0,
  "from_agent": "dev-agent",
  "to_agent": "staging-agent",
  "bead_id": "rosary-abc",
  "provider": "claude",

  "summary": "Fixed timeout handling in reconcile.rs:420. Edge case: timeout=0 was treated as infinity.",
  "files_changed": ["src/reconcile.rs", "src/reconcile_test.rs"],
  "lines_changed": { "added": 47, "removed": 12 },

  "review_hints": [
    "Check timeout=0 edge case — was a silent infinite loop before",
    "New test at reconcile_test.rs:180 covers this but only for single-agent"
  ],

  "artifacts": {
    "manifest": ".rsry-dispatch.json",
    "log": ".rsry-stream.jsonl",
    "previous_handoff": null
  },

  "verdict": null,
  "timestamp": "2026-03-15T23:45:00Z"
}
```

### Handoff chain

Each phase's handoff references the previous via `artifacts.previous_handoff`. The staging agent's handoff adds a `verdict` field:

```json
{
  "verdict": {
    "decision": "approve",
    "concerns": ["No integration test for concurrent timeout scenario"],
    "suggestions": ["Consider adding a test with max_concurrent=3 and staggered timeouts"]
  }
}
```

## Components

### 1. Capture agent stdout (prerequisite)

Currently `dispatch.rs` sends stdout to `Stdio::null()`. Redirect to `{workspace}/.rsry-stream.jsonl`. This gives us:
- Session ID (for resume)
- Cost/token data (for SBOM)
- Tool call history (for review agents to see what dev did)
- Stop reason (for orchestrator decision-making)

### 2. Handoff writer (orchestrator-side)

After each phase passes, the orchestrator:
1. Runs `Workspace::checkpoint()` (git add + commit)
2. Extracts `Work` from git diff (files, lines, diff_stat)
3. Parses `.rsry-stream.jsonl` last line for cost/session data
4. Generates `summary` from commit messages or agent's final text output
5. Writes `.rsry-handoff-{phase}.json` to workspace
6. This handoff is included in the NEXT phase's agent prompt

### 3. Handoff-aware agent prompt

The agent prompt changes from:

```
Fix this issue. [bead description]
```

To (for phase 1+):

```
Review the previous agent's work on this issue.

Bead: [bead description]

Previous phase handoff: (read .rsry-handoff-0.json in your working directory)
Previous agent log: (optionally read .rsry-stream-0.jsonl for full tool call history)

Use mache MCP tools to structurally understand the changes:
- mcp__mache__find_callers to check blast radius
- mcp__mache__get_diagnostics for type errors
- mcp__mache__search for related code

Your role: [staging-agent perspective from agents/staging-agent.md]
```

### 4. Cross-model adversarial review

The pipeline template specifies provider per phase:

```elixir
%Step{agent: "dev-agent",     provider: :claude,  mode: :implement}
%Step{agent: "staging-agent", provider: :gemini,  mode: :read_only}
%Step{agent: "prod-agent",    provider: :claude,  mode: :read_only}
```

Different model = different biases = adversarial by construction. Claude reviews itself poorly. Gemini reviewing Claude's work catches different classes of bugs.

### 5. PR as terminal step

When the final phase passes:
1. Orchestrator creates a git branch from the worktree: `fix/{bead-id}`
2. Pushes to origin
3. Creates PR via `gh pr create` with:
   - Title from bead
   - Body assembled from handoff chain (each phase's summary + verdict)
   - Labels: `agent-generated`, `perspective:dev`, `perspective:staging`
4. Links PR URL to bead (bead.pr_url field)
5. Bead transitions to `verifying` (not `closed` — human reviews first)
6. On PR merge → bead closes (via GHA webhook or rsry sync)

### 6. Human review gate

Between final agent phase and PR creation, an optional pause:
- Orchestrator sets bead status to `in_review`
- Linear shows the bead in "In Review" column
- Human can:
  - Approve → orchestrator creates PR
  - Request changes → orchestrator reopens bead with feedback, redispatches
  - Reject → orchestrator deadletters bead

For overnight mode: skip the human gate, create PR directly. Human reviews async via GitHub PR review.

## Dependencies

- **rosary-44fa25**: Capture stdout (prerequisite for handoff content + cost)
- **rosary-e69404**: Dispatch SBOM (handoff references the manifest)
- **manifest.rs**: Already built — handoff consumes it

## Backend Agnostic

The handoff artifact is a JSON file in the workspace. It doesn't depend on:
- Linear (works without it — handoff is workspace-local)
- Dolt (handoff is file-based, not database)
- Any specific provider (Claude, Gemini, ACP all produce handoffs)
- Any specific execution backend (local, sprites — handoff is in the workspace)

The orchestrator (Rust reconciler or Elixir conductor) writes the handoff. The backend stores the workspace. The handoff travels with the workspace.

## Implementation Order

1. **Capture stdout** — redirect to .rsry-stream.jsonl (dispatch.rs, 1 line change + file creation)
2. **Handoff struct** — Rust struct in manifest.rs or new handoff.rs, JSON serializable
3. **Handoff writer** — orchestrator writes after checkpoint, before next phase dispatch
4. **Handoff-aware prompt** — build_prompt reads handoff and injects into agent prompt
5. **Provider per step** — Pipeline.Step gets a provider field, AgentWorker uses it
6. **PR creation** — terminal step in pipeline, gh pr create from worktree
7. **Human gate** — optional pause between final phase and PR
8. **Verdict schema** — staging/prod agents write structured review verdicts
