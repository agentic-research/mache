---
status: Proposed
author: jamestexas
repo: rosary
---

# Tool Constellation + Plugin Substrate

Five tools, each a different lens on the same underlying graph of files, symbols, and repos. Rosary is the orchestrator; the others are composable inputs.

| Tool | Lens |
|------|------|
| mache | code → symbols → references (what exists) |
| rosary | intent → beads → file scopes (work to do, deps, dispatch) |
| assay | docs → code refs (coverage + staleness) |
| claude-guard | sandboxed agent chains, cost-optimized prompt cache |
| ley-line | semantic embeddings for fuzzy doc/code match |

Shared substrate: assay's tree-sitter queries derive from mache's schema. Rosary's BDR can use mache for code traversal. Claude-guard's chain YAML is the same primitive rosary's pipeline is reaching for.

## Goals

- Rosary aggregates all five tools as composable plugins rather than hard-coded integrations.
- Agents dispatched into workspaces see only the tools scoped to their pipeline phase.
- Work derived from docs, sessions, code, and external sources flows through a unified BDR provenance chain.
- Every bead has a verifiable test command in its success criteria before it can be closed.

## Plugin Substrate

The shipped `[[plugins]]` system handles lifecycle hooks (subprocess/HTTP, four hook points). It does NOT aggregate child MCP servers, filter per-session tool catalogs, or provide a dispatch-backend axis beyond subprocess.

### Add Plugin Kind Axis

The `PluginConfig` TOML struct needs a `kind` field distinguishing three roles:

- `kind = "hook"` — existing behavior (default when field absent; backward-compatible)
- `kind = "mcp"` — context/tool provider; rosary connects as outbound MCP client
- `kind = "dispatch"` — execution backend (sandboxed runner, chain-YAML executor)
- `kind = "state-sink"` — outbound state mirror (issue trackers, dashboards, webhooks)

Files to change: `src/config.rs` (add `PluginKind` enum, `kind: Option<PluginKind>` on `PluginConfig`), `src/plugin.rs` (route on kind).

Success when: `cargo test -p rosary plugin::config` passes, including backward-compat test where missing `kind` defaults to `hook`.

### MCP Client Integration

Rosary must be able to connect outbound to child MCP servers declared as `kind = "mcp"` plugins. Use an async MCP client (rmcp or equivalent) to connect at serve startup and maintain connections per registered MCP plugin.

Each connected child exposes a tool catalog. Rosary re-exposes those tools under a namespace: `<plugin_name>__<tool_name>`. This gives dispatched agents a single MCP surface.

Success when: a `kind = "mcp"` plugin registered in config connects at startup; its tools appear in rosary's tool list prefixed with the plugin name; `cargo test -p rosary serve::mcp_client` passes.

### Tool Catalog Aggregation

Aggregate tool catalogs from all connected MCP plugins into a unified registry. Namespacing: `<plugin>__<tool>`. Tool descriptions include which plugin provides them.

The aggregated catalog must be deterministic so that two sessions with the same config produce identical tool lists. De-duplicate tools with identical names across plugins by preferring the first-registered plugin.

Success when: `cargo test -p rosary serve::tool_catalog` passes; catalog includes tools from two mock MCP servers with correct namespacing.

### Per-Session Phase-Scoped Tool Catalog

Each dispatched agent session should receive only the tools scoped to its pipeline phase. Phase config specifies an allowlist of plugin kinds and tool names. This requires per-session capabilities filtering on the MCP server side.

This is conditional on rmcp supporting per-session capabilities. Spike (30 min): check rmcp API. If supported: implement. If not: file a blocked bead citing the dependency.

Success when: `cargo test -p rosary serve::session_filter` passes, or a blocked bead exists citing rmcp limitation.

### Dispatch-Backend Axis

Current dispatch spawns subprocess CLIs (claude, gemini, ACP). The plugin `kind = "dispatch"` axis makes execution backends pluggable: containerized chains, remote runners, chain-YAML executors.

A dispatch plugin receives the same JSON payload as a hook plugin. It is responsible for spawning the agent and returning structured output. The rosary dispatch loop treats it identically to a built-in provider.

Success when: a sample `kind = "dispatch"` plugin config loads and routes correctly; `cargo test -p rosary dispatch::backend_plugin` passes.

### Plugin Discovery from Directories

Plugins should register by dropping a TOML file — same pattern as skills and agents. Discovery locations (in priority order):

1. `<repo>/.rosary/plugins/*.toml` — project-local
2. `~/.rsry/plugins/*.toml` — user-global

A plugin file has the same shape as a `[[plugins]]` entry in config. Rosary merges discovered plugins with config-declared ones at startup; project-local plugins override user-global plugins with the same name.

Success when: a TOML file in `.rosary/plugins/` is picked up at startup; `cargo test -p rosary config::plugin_discovery` passes.

## BDR Enhancements

BDR's ProvenanceRef now includes `Session` and `Code` variants (added in this branch). The following capabilities use them.

### Session Capture Command

Add `capture --from-session <path>` subcommand to the CLI. It reads a transcript file (path arg or stdin), runs `bdr_enrich::extract_atoms_with_llm()` over it, and proposes `BeadSpec`s to stdout (dry-run by default; `--commit` writes to `.beads/`).

ProvenanceRef for the output beads: `Session { transcript_path: <path>, summary: <LLM-extracted one-liner> }`.

This closes the gap where chat logs evaporate. Sessions are already a model (see interactions.jsonl). The capture path is the missing piece.

Success when: `rsry capture --from-session /path/to/transcript.md` produces JSON BeadSpecs to stdout; `cargo test -p rosary capture::session` passes with a fixture transcript.

### Code Provenance Round-Trip

Add a `capture --from-code <repo> <path> [--symbol <sym>]` subcommand. It calls mache's `find_definition` + `find_callers` to extract context, then runs BDR atom extraction to derive a "current design" from code.

ProvenanceRef: `Code { repo, path, symbol }`.

Pairs with assay's signature-drift check (roadmap level 3) to close the bidirectional design loop: markdown → atoms → code (forward), code → atoms → drift signal (backward).

Success when: `rsry capture --from-code rosary src/bead.rs --symbol BeadSpec` produces at least one BeadSpec; `cargo test -p rosary capture::code` passes.

### Stub-the-Design Output Mode

Given a `Doc` or `Session` ProvenanceRef plus decomposed atoms (especially `TechnicalSpec` and `Constraint` kinds), emit code stubs into the target repo. The stub is a minimal Rust/Go file with the signatures derived from the spec but no logic.

The bead is the stub commit. Reviewing the stub PR is reviewing the design before any logic is written. This mitigates AI anchoring on premature implementations.

Output flag: `rsry decompose --stub-output <target_repo_path>`.

Success when: `rsry decompose --stub-output /path/to/repo docs/design/spec.md` creates stubs and opens a draft PR; `cargo test -p rosary decompose::stub_output` passes.

### Per-Phase Model Selection

Pipeline config should allow a `model` field per phase (as chain YAML already does). Light prompts (triage, verify) should default to haiku; heavy prompts (review, architect) should default to sonnet or opus.

Add `model: Option<String>` to the per-phase config struct. Dispatch picks up this value and passes it to the agent provider.

Success when: a pipeline config with `model = "haiku"` on the triage phase dispatches with haiku; `cargo test -p rosary dispatch::phase_model` passes.

## GitHub Integration

Linear webhook is wired. The symmetric GitHub side is missing.

### GitHub Merge Webhook → Advance Dependents

When a PR is merged on GitHub, rosary should:
1. Find any bead linked to that PR (via bead metadata or PR body tag).
2. Advance the bead to `Done`.
3. Unblock any beads that depended on it (transition them from `Blocked` to `Open`).

The rosary-stringer GitHub App already exists (App ID + Installation ID in config). Wire a `/webhook/github` endpoint in `src/serve.rs` alongside the existing Linear webhook endpoint.

Success when: a sample `pull_request.closed` GitHub webhook payload (merged=true) delivered to `/webhook/github` advances a bead and unblocks a dependent; `cargo test -p rosary serve::github_webhook` passes with a fixture payload.

### GitHub-Native Bead Presentation

Non-power-users won't open a new tool. Mirror bead context outward as a structured comment block on the linked PR or issue: decade / thread / deps / success criteria.

Symmetric to existing Linear sync but in the opposite direction. Triggered on bead state transitions (e.g., `InProgress` → post comment on PR; `Done` → close issue reference).

Success when: `rsry sync --github` posts a bead context comment on a test PR; `cargo test -p rosary linear_tracker::github_mirror` passes with a mock GitHub API.

## Assay Pipeline Gates

Assay is already shipped. These beads wire it into rosary's pipeline.

### Assay as Verify-Tier Plugin

Drop-in via the shipped `[[plugins]]` system:

```toml
[[plugins]]
name = "assay-coverage"
kind = "hook"
hook = "pipeline.verify"
command = ["assay", "verify", "--threshold", "0.8", "--format", "json"]
```

Parse assay's JSON output, map `covered` → `pass`, `uncovered` → `fail`, `stale` → `request-changes`. Doc coverage becomes a pipeline gate.

Success when: plugin config loads; `cargo test -p rosary plugin::assay_verdict` passes; a test run with mock assay output produces the correct verdicts.

### Per-Bead Doc-Coverage Delta

Run assay before and after a bead's PR lands. Fail the verify gate if:
- Coverage drops below baseline for any entity in the bead's file_scopes.
- New public entities in the bead's diff are uncovered by docs.

Add `doc_coverage_min: Option<f32>` to `BeadSpec.success_criteria`. The assay plugin reads this from the bead's handoff JSON.

Success when: a bead with `doc_coverage_min = 0.9` fails verify when assay returns 0.85; `cargo test -p rosary plugin::assay_delta` passes.

### Stale Refs → Chore Beads

Assay's `stale` output lists markdown references to code that no longer exists. Auto-file these as `chore` beads via `rsry scan --assay`.

No LLM required. The stale ref IS the bead title. The markdown file is the file_scope. Priority: P3 by default.

Success when: `rsry scan --assay` on a repo with known stale refs creates the expected chore beads; `cargo test -p rosary scan::assay_stale` passes.

## Scoped Notes

Pattern for host-bound knowledge: `notes/` directory with subdirectories encrypted with `age`. Recipients are SSH keys; GH-registered keys are recipients-by-URL.

```
notes/
  personal/     .recipients = [my-laptop-key]
  work/         .recipients = [my-laptop-key, work-yubikey]
  work-shared/  .recipients = [my-laptop-key, https://github.com/<user>.keys, ...]
  public/       (no encryption)
```

### Context-MCP Plugin for Scoped Notes

A tiny `context-mcp` server reads the subset of `notes/` decryptable on the current host and exposes notes as MCP resources. Register it as a `kind = "mcp"` plugin in rosary.

Phase config decides which note scopes a dispatch can see (e.g., triage phase sees `public/` only; review phase sees `work/`). Different machines see different scopes naturally because they have different unwrap keys.

Success when: `context-mcp` plugin loads; notes decryptable by the current host appear as MCP resources; notes not decryptable are absent; `cargo test -p context_mcp server::scope_filter` passes.

### Age Encryption + Key Rotation

Encrypt notes with `age`; recipients are SSH keys. Rotation = re-encrypt affected directory with updated recipient list (one-liner). Host-binding starts with macOS Keychain and escalates to hardware-backed keys (age-plugin-yubikey, age-plugin-tpm) once scope layout stabilizes.

Document the rotation procedure and add a `rsry notes rotate --scope <scope>` subcommand that re-encrypts in place.

Success when: `rsry notes rotate --scope work --add-recipient <key>` re-encrypts all files in `notes/work/`; `cargo test -p rosary notes::rotation` passes.

## Open Questions

- Does rmcp support per-session capabilities filtering? Needs a 30-min spike before committing to per-session scoped catalogs.
- Should `capture --from-session` require an explicit `--model` flag, or always use haiku?
- Is `context-mcp` a new binary in this repo or a separate repo? (Lean: separate binary, registered as a plugin.)
- Should stale-ref chore beads be filed in the repo that owns the markdown, or in rosary?
