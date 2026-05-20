# Glossary

Terms used across rosary, the conductor, agents, and ADRs.

## Work hierarchy (BDR lattice)

| Term | What | Example |
|------|------|---------|
| **Decade** | One ADR decomposed — the top-level organizing primitive. Contains threads. Named after a rosary decade (10 beads in a group). | `ADR-003` "Linear hierarchy mapping" |
| **Thread** | A semantic grouping of related beads within a decade: context, implementation, validation, etc. | `ADR-003/implementation` |
| **Bead** | Atomic work item. Lives in a repo's `.beads/` Dolt database. The unit an agent receives, works, and closes. | `rsry-d93546` "Add webhook HMAC verification" |
| **Channel** | BDR visibility tier. Decade (internal) → Thread (team) → Bead (external). Maps atoms to the right granularity. | `BdrChannel::Bead` |
| **Atom** | A single extractable concept from a document — friction point, decision, phase, validation point, etc. Decomposed into beads. | `AtomKind::Phase` "Phase 1: Scaffold" |

## Orchestrator

| Term | What |
|------|------|
| **Reconciler** | The core loop: scan → triage → dispatch → verify → report → sleep. Kubernetes-controller-style desired-state reconciliation. |
| **Triage** | Scoring open beads to decide dispatch priority. Composite: 40% priority, 30% dependency readiness, 20% age, 10% retry penalty. |
| **Dispatch** | Spawning an agent in an isolated workspace to work a bead. Assigns the bead to an agent + provider + compute backend. |
| **Pipeline** | The sequence of agent perspectives a bead passes through: dev → staging → prod → feature. Each phase is a different agent with a different lens. |
| **Generation** | Content hash of a bead (id + title + description + priority). When it changes, the bead is re-triaged. Prevents redundant work on unchanged beads. |
| **Backoff** | Exponential delay after dispatch failure: `min(30s × 2^retries, 30min)`. After 5 retries, the bead is deadlettered. |
| **Deadletter** | A bead that has exhausted retries or hit 3 consecutive regressions. Blocked for human attention — agents won't touch it. |

## Execution

| Term | What |
|------|------|
| **AgentProvider** | Which model runs: Claude, Gemini, ACP. Returns an `AgentSession`. |
| **ComputeProvider** | Where the agent runs: `local` (host subprocess) or `sprites` (remote container via sprites.dev). |
| **Workspace** | Isolated VCS environment for an agent to work in. jj workspace (preferred) or git worktree (fallback). Destroyed after verification. |
| **Conductor** | Elixir/OTP application that manages agent lifecycles via supervision trees. Talks to rsry over HTTP/MCP. Handles the WHICH/WHERE/HOW of execution. |
| **ACP** | Agent Client Protocol — standardized interface for AI model invocation. The conductor uses ACP adapters to talk to Claude, Gemini, etc. |

## Verification

| Term | What |
|------|------|
| **Tier** | One check in the verification pipeline. Tiers run in order; first failure short-circuits. |
| **Tier 0** | Commit exists? |
| **Tier 1** | Does it compile? |
| **Tier 2** | Do tests pass? |
| **Tier 3** | Does the linter approve? |
| **Tier 4** | Diff sanity — ≤10 files, ≤500 lines changed. |

## Storage

| Term | What |
|------|------|
| **Dolt** | Version-controlled SQL database. Each repo has a `.beads/` directory with its own Dolt server. Beads live here. |
| **Backend** | Rosary's own persistent state store (`~/.rsry/dolt/rosary/`). Stores cross-repo relationships: pipeline state, dispatch history, decades/threads, Linear links. Separate from per-repo bead Dolt databases. |
| **Linear** | External issue tracker used as a human-facing UI. Bidirectional sync — beads are source of truth, Linear is a projection. |
| **LinearLink** | Mapping between a bead and its Linear representation (issue, sub-issue, or milestone). Replaces the overloaded `external_ref` field. |
| **Mirror bead** | (Legacy) A cross-repo reference created by copying a bead into another repo's `.beads/`. Being replaced by `CrossRepoDep` in the backend. |

## Agent perspectives

| Agent | Lens | Scope |
|-------|------|-------|
| **dev-agent** | Implementation quality | Function-level |
| **staging-agent** | Test validity (adversarial) | Test files |
| **prod-agent** | Production quality | Module-level |
| **feature-agent** | Cross-file coherence | Feature branch |
| **pm-agent** | Strategic perspective | Cross-repo |

## Config & state

| Term | What |
|------|------|
| **`~/.rsry/config.toml`** | Global config: repos, Linear settings, compute, HTTP/tunnel, backend. |
| **`rosary.toml`** | Per-project config (overrides global for that repo). |
| **`rosary-self.toml`** | Self-management config — rosary watches its own repo (dogfooding). |
| **Session registry** | `~/.rsry/sessions.json` — tracks active agent PIDs. Pruned of dead processes on load. |

## Plugins

| Term | What |
|------|------|
| **Plugin** | Out-of-process executable that extends rosary along a `kind` axis. Discovered from `~/.rsry/plugins/` and `<repo>/.rosary/plugins/`. |
| **`kind = "hook"`** | Pipeline hook (default). Runs at `pipeline.triage` / `pipeline.verify` / `pipeline.close`. |
| **`kind = "mcp"`** | Outbound MCP server — rosary connects to it as a client (planned). |
| **`kind = "dispatch"`** | Alternative `AgentProvider` (e.g. local model runner, sprites). |
| **`kind = "state_sink"`** | Mirrors bead state to an external system (planned). |
| **Coverage gate** | A `pipeline.verify` plugin can return `coverage: 0..1`. When a bead's `doc_coverage_min` is set, rosary fails verify if coverage is below the threshold. |
| **`assay.scan`** | Scan-time hook. `rsry scan --assay` calls these plugins per repo, files P3 chore beads for each reported stale ref. |

## Provenance & capture

| Term | What |
|------|------|
| **`ProvenanceRef`** | Tagged enum tracking where a bead's atom came from. Variants: `Doc { path }`, `Session { transcript_path, summary }`, `Code { repo, path, symbol }`, `Meeting`, `SlackThread`. Stored in `Bead.derived_from`. |
| **`rsry capture --from-session`** | Reads a transcript file, extracts BDR atoms via LLM, produces BeadSpecs with `Session` provenance. |
| **`rsry capture --from-code`** | Reads a source file (optionally scoped to a symbol), extracts atoms via LLM, produces BeadSpecs with `Code` provenance. |
| **`rsry decompose --stub-output`** | Emits Rust skeletons (`.rsry-stubs/<decade>.rs`) from `TechnicalSpec` and `Constraint` atoms — review the stub PR before implementing to validate the design. |
| **Verifiable test command** | `rsry bead close` requires impl beads (bug/feature/task/chore) to have a recognised test command (`cargo test`, `pytest`, `npm test`, etc.) in their description. Override with `--force`. |

## Encrypted notes

| Term | What |
|------|------|
| **Scope** | Subdirectory of `notes/` (e.g. `notes/work/`, `notes/public/`) holding age-encrypted notes for one access tier. |
| **Recipients file** | `notes/<scope>/.recipients` — newline-separated age recipients allowed to decrypt that scope. |
| **`rsry notes rotate`** | Re-encrypts every `*.age` file in a scope after applying `--add-recipient` / `--remove-recipient` edits. Refuses to rotate to an empty list. |
