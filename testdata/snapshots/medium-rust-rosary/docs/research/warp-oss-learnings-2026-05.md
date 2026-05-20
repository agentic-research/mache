# Warp OSS — Learnings for Rosary

**Date**: 2026-05-08 (initial pass), revised 2026-05-08 after a deeper sweep.
**Source**: [github.com/warpdotdev/warp](https://github.com/warpdotdev/warp) — pinned at commit `ef00af00bf033a4d355aca5b145841e107fd2b7c`. ~698 MB, **63 crates + a 945k-line `app/` tree (1.3M LOC total)**, 159 spec dirs at the snapshot time.
**Status**: Snapshot research. The original draft of this doc skimmed ~1-2k of 343k crate-LOC. The corrections below are from a second pass.

## Corrections to the original draft

The first version of this doc made three claims I later found wrong or shallow:

1. **`isolation_platform` is NOT sandboxing.** It's workload identity (OIDC-style tokens for cloud workload, `WorkloadToken { token, expires_at }`, with platform detection for Fly/AWS/GCP). Different concept. There's no equivalent in rosary because rosary isn't multi-tenant in that way. Removed from the "isolation" comparison; what rosary actually competes with is `crates/computer_use` (Anthropic's computer-use spec, real desktop automation) — which has no rosary analogue.
2. **The "63-crate" framing missed the real bulk.** Crates is 343k LOC. The actual **app/** directory is **945k LOC** as a single large tree. Total Warp client ≈ 1.3M Rust LOC. So "they split aggressively" overstates it — they split the *libraries*, the app itself is monolithic.
3. **`harness_type` is concrete, not abstract.** It's an enum: `{ Oz, ClaudeCode, OpenCode, Gemini, Codex }` (see `crates/ai/src/agent/orchestration_config.rs:184-214`). It's literally rosary's `provider` field with two more harnesses (Codex + OpenCode) and a self-harness (Oz). 1:1 alignment.

## The headline I missed: the skills crate

`crates/ai/src/skills/` is **1,482 LOC across 10 files** and is the most directly portable thing in the entire Warp codebase. It's a structured, parsed, multi-provider skill system:

```rust
pub struct ParsedSkill {
    pub path: PathBuf,
    pub name: String,
    pub description: String,
    pub content: String,                  // full file contents incl. front matter
    pub line_range: Option<Range<usize>>, // markdown body, 1-indexed
    pub provider: SkillProvider,
    pub scope: SkillScope,
}

pub enum SkillProvider { Claude, Codex, Gemini, Droid, OpenCode, Warp }
pub enum SkillScope    { Bundled, Home, Project }   // bundled = ships with Warp
```

Rosary's equivalent is `agents/*.md` — a flat directory loaded at runtime by `load_agent_prompt()` in `src/dispatch/prompt.rs` (`std::fs::read_to_string` per file). What's missing isn't runtime loading; it's the *structure*: no parsed metadata, no provider tagging, no project-vs-home-vs-bundled scope, no front-matter/body separation.

Code reuse is constrained by Warp's licensing — the skills crate is AGPL-3.0 (only `warpui_core` / `warpui` carry the MIT carve-out). Lifting the *pattern* into a fresh rosary implementation is fine; copying source verbatim would pull rosary into AGPL contagion if we aren't already AGPL there. Either way, the design is portable. This would let us:

- Mix Claude+Codex+Gemini agent definitions in a single registry without per-provider branching
- Distinguish "ships with rosary" agents (dev/staging/prod) from project-overrides at `<repo>/.rsry/agents/`
- Reuse Warp's parser (front-matter + content + line-range tracking) instead of inventing one

This is more impactful than any of the six items I originally listed.

## TL;DR

Warp open-sourced its terminal client under AGPL-3.0 + a small MIT carve-out for the UI framework (`warpui_core`, `warpui`). OpenAI is the founding sponsor. The paid product is **Oz** — the agentic management bot that triages issues, writes specs, opens PRs, and reviews PRs in this very repo. The repo *is* the demo: contributors watch Oz work alongside humans on `build.warp.dev`.

Rosary is doing the same shape of thing — agent orchestration over multi-repo work — and is structurally **deeper** in some places (provenance, cross-repo deps, Dolt-versioned beads) but **simpler than it should be** in two specific places that Warp gets right:

1. **Spec PR before code PR** for feature work, with a clean two-doc gate (`PRODUCT.md` + `TECH.md`)
2. **Explicit user-approval gate** on agent runs (`OrchestrationConfigStatus: None | Approved | Disapproved`) before auto-launch

## Architecture map

### Crate layout (the relevant subset)

| Warp crate | Rough rosary analogue | Note |
|---|---|---|
| `ai/agent/orchestration_config.rs` | `src/dispatch/`, `src/config.rs` | Warp models `RunAgentsRequest` with `harness_type` (enum: Oz/ClaudeCode/OpenCode/Gemini/Codex) + `execution_mode: Local | Remote{environment_id, worker_host}` per-run, not per-config |
| `ai/skills/` (parsed_skill, skill_provider, parser, scope) | `agents/*.md` (flat) | **The big find** — see "headline" section above. 1,482 LOC of structured skill parsing across 6 providers and 3 scope levels. |
| `computer_use` | (no analogue) | Anthropic's computer-use desktop automation — Action / Key / MouseButton / Screenshot. They implement the actor side. Distinct from rosary's Bash/Edit/Write tool model. |
| `isolation_platform` | (no analogue) | **NOT sandboxing.** Workload identity (OIDC tokens for Fly/AWS/GCP). Multi-tenant auth glue rosary doesn't need yet. |
| `ipc`, `jsonrpc` | `src/serve/mod.rs` | Wire-protocol layer |
| `persistence` | `src/store_*.rs` | Their state store (didn't read deeply yet) |
| `command-signatures-v2` | (none) | Just a `rust-embed` static asset bundle (7 LOC of `lib.rs`, the rest is `build.rs` + assets). Excluded from CI because it's compiled embedded data, not behaviors. |
| `firebase` | (no analogue) | Google sign-in backend (`GetAccountInfo`, `FetchAccessToken`). Hosted-product auth glue. |
| `app/` (945k LOC, single tree) | `src/` (~50k LOC) | Where the actual app lives. They split *libraries*; the *app* is monolithic. |

**Real scale**: 63 crates + 945k-LOC `app/` = ~**1.3M Rust LOC** total. The crate count is right-sized for their library surface; the app itself is one big tree.

### Spec PR flow

```
specs/APP-NNNN/
  PRODUCT.md   — user-facing problem, behavior, non-goals, mocks
  TECH.md      — file paths + line numbers + diff sketch + interactions
```

98 of the 159 spec dirs follow `APP-NNNN` or `GH-NNNN` numbering. The others are personal scratch dirs (`Advait-M/`, `alokedesai/`, `andy/`).

The TECH.md spec I read (`APP-1915`) is **strikingly precise** — concrete file paths, line ranges (`view.rs (14712-14803)`), exact callers to update, code snippets of the proposed change. This is what rosary's `tech_spec` bead field aspires to but rarely contains.

### Readiness labels

| Label | Means |
|---|---|
| `ready-to-spec` | Problem understood, design open. **Reserved for feature requests.** Next step: open a spec PR. |
| `ready-to-implement` | Design settled, OR triaged bug. Next step: open a code PR. |
| `needs-mocks` | Wait for design mocks. |

Three flat labels replace what rosary models as a dual state machine (ADR-0004). Simpler. Possibly too simple — you can't express "blocked on dep" or "deadlettered" — but for a 50k-star repo it's been enough. **Question for rosary**: are the additional states earning their complexity?

### Review pipeline (Oz)

```
File issue → Warp team triages → readiness label → contributor opens PR
   → Oz auto-reviews → Oz approves → SME (human) auto-assigned → CI → merge
```

Oz is the auto-assignee on every PR. Contributors push fixups, comment `/oz-review` (max 3x per PR) to re-trigger. This is **exactly** the pattern PR #173/#176 did this session with Copilot — except Oz is the product, not a generic bot.

### `/feedback` command

Warp users can file an issue from inside the running terminal — the command attaches logs + env automatically. Rosary's `rsry bead create` is text-only; users have to paste context themselves.

## What Warp does that rosary doesn't (and could)

### 1. Spec PR is a hard gate

Rosary has bead descriptions. A long bead description ≠ a structured PRODUCT/TECH split, and the description doesn't get reviewed independently of the code.

Concrete proposal: a new `bead.spec_status` field with values `none | needs-spec | spec-in-review | ready-to-implement`, and a `rsry spec` command that opens a PR adding `specs/<bead-id>/PRODUCT.md + TECH.md` against the target repo. Bead can't dispatch to dev-agent until `spec_status = ready-to-implement`. This formalizes Golden Rule 12 (5-whys) into a reviewable artifact.

Already-existing bead: `rosary-d59626` ("Scoping agent — LLM pre-dispatch validation, 5 whys as pipeline phase -1") covers half of this. Spec PR is the *output* of that scoping phase.

### 2. Explicit approval gate before auto-launch

```rust
pub enum OrchestrationConfigStatus { None, Approved, Disapproved }
```

The user has to approve the (model_id, harness_type, execution_mode) triple. After approval, future `run_agents` calls with matching fields auto-launch; mismatched calls show a confirmation card.

Rosary today: bead enters queue → triage scores → reconciler dispatches if score ≥ threshold. There's no human approval primitive. Adding `bead.dispatch_approval: None | Approved | Rejected` (default None for new beads, auto-Approved for self-managed repos) would let users gate dispatch on a per-(repo, agent) basis without changing the queue.

### 3. Per-run execution_mode

```rust
RunAgentsExecutionMode::Local
RunAgentsExecutionMode::Remote { environment_id, worker_host }
```

Rosary's compute provider is swapped at config time (sprites vs local vs Fly). Warp lets a single conversation pin where each agent run executes. **Lesson**: lift compute_provider into `RunSpec` / `DispatchRecord`, default from config.

### 4. Specs-in-tree numbering

`specs/APP-NNNN/` mirrors GitHub issue numbers. Easy to grep. Easy to land a spec without code. Easy for the bot to reference.

Rosary stores specs in bead descriptions (live in Dolt, not in the tree). Tradeoff: Dolt is versioned + cross-repo queryable, but specs aren't grep-able from `find docs/`. Putting a synced copy at `docs/specs/<bead-id>/` could give both.

### 5. `script/presubmit` over `task lint`

Warp uses a single `./script/presubmit` that runs fmt + clippy + tests as one command. Rosary uses Taskfile with `task lint` (clippy) + `task test` (cargo nextest) + pre-commit hooks. Roughly equivalent — Warp's is simpler to remember; rosary's is more granular.

## What rosary does that Warp doesn't

### 1. Cross-repo work tracking

Warp is a single repo. Rosary's `[[repo]]` config + cross-repo deps (ADR-0009 stratified acyclicity + modal evidence) is genuinely novel — Warp has nothing like it. If rosary opens up its multi-repo orchestration as a primitive, that's a positioning advantage Warp doesn't compete on.

### 2. DSSE/in-toto handoff signatures (APAS L2)

Warp has **no** signed attestation on agent output. Their PR review is "Oz says LGTM" + "human says LGTM" + CI. Rosary's APAS L2 (DSSE-Ed25519-signed handoffs, in-toto Statement payload) is a more rigorous substrate. If/when supply-chain auditability matters, rosary already has it.

### 3. Versioned bead store (Dolt)

Warp's `persistence` crate is conventional state. Rosary's per-repo `.beads/` is a versioned SQL DB. Reverting bad bead writes, branching state per agent run, federating bead changes across repos — all enabled by Dolt, none of which Warp can do.

### 4. BDR lattice (Decade → Thread → Bead)

Warp has issues. That's it. No higher-level grouping primitive. Rosary's BDR (decompose ADR markdown into atoms → beads grouped into threads under decades) is structurally richer for org-wide planning across many small units of work.

## Specific things to copy (revised priority order)

| Item | Effort | Where | Status |
|---|---|---|---|
| **Lift the skills-crate pattern**: `ParsedSkill { path, name, description, content, line_range, provider, scope }` + `SkillProvider` enum + `SkillScope { Bundled, Home, Project }` | medium | new `src/skills.rs` or `crates/skills/`; replace static `agents/*.md` loading | — |
| Add `DispatchApproval { None, Approved, Rejected }` field on `RepoConfig.approval`, gated by `[dispatch].require_approval` | small | `src/config.rs` | ✅ shipped in PR #180 |
| Add `specs/<bead-id>/{PRODUCT,TECH}.md` convention to rosary self-managed repo | small | `docs/specs/` directory + `bead.spec_path` field | — |
| Add per-run `execution_mode` override to `DispatchRecord` | small | `src/store.rs` `pub struct DispatchRecord` already exists; add field | — |
| Extract a `presubmit` task from Taskfile that wraps `task fmt && task lint && task test` | trivial | `Taskfile.yml` | — |
| `rsry feedback` command that auto-attaches `~/.rsry/rsry-serve.log` tail to a new bead | small | `src/main.rs` + `src/cli.rs` | — |
| Add `ready-to-spec` / `ready-to-implement` Linear-label-equivalent to bead status | medium | `src/bead.rs` BeadState — possibly via a new `phase` enum separate from the lifecycle state | — |

## Specific things to NOT copy

- **63-crate split** is right for their library surface but their **app is a 945k-line single tree**. Don't aspire to either extreme — keep rosary's current structure.
- Their slack-only contributor channel. Rosary should keep bead comments + GitHub PRs as the canonical record; Slack is a private side-channel.
- Their dependency on a hosted Oz product. Rosary's "all roles in one binary, all open source" positioning is already a clearer story.
- `crates/firebase` (Google sign-in) and `crates/isolation_platform` (cloud workload identity) — both are hosted-product infra rosary doesn't need.

## Still un-surveyed (for a future pass)

I haven't read these in any depth — flagging so the next sweep can pick them up:

- `app/src/ai_assistant/` — where the agent UI actually lives
- `app/src/billing/` — they actively bill in-app
- `crates/persistence` — their state model (didn't read schema)
- `crates/onboarding` — first-run flow worth studying for `rsry enable` UX
- `crates/managed_secrets` + `_wasm` — secret management with WASM-targeted variant
- `crates/jsonrpc` — their MCP/JSON-RPC layer specifics
- The `script/deploy_remote_server` operations side

## Open questions

- **Build.warp.dev dashboard**: a public live view of agent activity. Could rosary do a static-site equivalent fed by `~/.rsry/backend.db` exports? Would dovetail with the iOS control-room app.
- **specs/ vs beads/**: should rosary spec docs live in-tree (greppable, PR-reviewable) or in Dolt (versioned, cross-repo)? Sync model?
- **Approval gate**: per-bead, per-(repo, agent), or per-run? Warp does per-OrchestrationConfig (model+harness+mode triple). Rosary's grain isn't obvious.

## References

All paths below are pinned to commit `ef00af00bf033a4d355aca5b145841e107fd2b7c` so they remain stable as upstream evolves.

- [github.com/warpdotdev/warp](https://github.com/warpdotdev/warp) — main repo
- [warp.dev/blog/warp-is-now-open-source](https://www.warp.dev/blog/warp-is-now-open-source) — announcement
- [CONTRIBUTING.md](https://github.com/warpdotdev/warp/blob/ef00af00bf033a4d355aca5b145841e107fd2b7c/CONTRIBUTING.md) — contribution flow
- [specs/APP-1915/PRODUCT.md](https://github.com/warpdotdev/warp/blob/ef00af00bf033a4d355aca5b145841e107fd2b7c/specs/APP-1915/PRODUCT.md) and [TECH.md](https://github.com/warpdotdev/warp/blob/ef00af00bf033a4d355aca5b145841e107fd2b7c/specs/APP-1915/TECH.md) — sample spec pair
- [crates/ai/src/agent/orchestration_config.rs](https://github.com/warpdotdev/warp/blob/ef00af00bf033a4d355aca5b145841e107fd2b7c/crates/ai/src/agent/orchestration_config.rs) — approval state machine
