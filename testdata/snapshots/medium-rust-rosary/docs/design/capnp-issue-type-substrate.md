---
status: Proposed
author: jamestexas
repo: rosary
relates_to:
  - docs/design/structured-handoff-pipeline.md
  - docs/design/tool-constellation-substrate.md
  - docs/adr/0009-cross-repo-linkage.md
  - docs/adr/0010-observation-lattice.md
---

# Capnp Issue-Type Substrate + Self-Narrated Handoffs

## TL;DR

Make **issue type** the unit of extensibility. Express each type as a Cap'n Proto schema that declares its shape, guardrails, agent pipeline, sources, triggers, success criteria, and post-mortem subscriptions. Built-in types ship with rosary; user types live in `.rosary/types/*.capnp` per-repo. The agent pipeline runs the cost-tiered LLM ladder (Needle → Qwen → ds4 → Claude) gated by deterministic guardrails. Agents emit first-person **self-narrated handoffs** at pause/near-close/interrupt, providing the resume substrate, the trust signal, and the federation payload — one mechanism, three uses. Federation rides on ADR-0010's observation lattice; passkey-signed canonical capnp payloads are the portable trust unit. Inter-peer trust uses signet device-cert chains; OIDC is for outbound integration only.

## 1. Problem

Three problems show up as one:

1. **Multi-session continuity (the AFK problem).** Work happens across remote sessions, mobile, phone-side claude-code, local CLI. No session knows what the others did. Sitting back down means re-reading every PR description and bead.
2. **Cross-source drift.** Same work exists as a bead, a GitHub issue, a Linear ticket, a PR review thread, and a Slack message — with no canonical reconciliation. Hardcoded mappers per pair don't scale.
3. **No resume story when work pauses.** Interrupting an agent — by design (interrupt mechanism), by failure, or by end-of-context — discards everything the agent learned about the problem. The next session starts cold.

Today rosary handles bead lifecycle, dispatch, semantic dedup, cross-repo linkage, observation folding (ADR-0010), structured phase-to-phase handoffs ([structured-handoff-pipeline](structured-handoff-pipeline.md)), and the plugin kind axis ([tool-constellation-substrate](tool-constellation-substrate.md)). What's missing is a **declarative spec for what work is** that the runtime materializes — plus a **session-spanning self-narrated handoff** that turns interruption from data loss into a checkpoint.

## 2. Core abstraction: Cap'n Proto issue types

Each issue type is a Cap'n Proto struct declaring everything the runtime needs to materialize it. Built-in types (`bug`, `feature`, `task`, `epic`, `design`, `chore`) ship with rosary as capnp shipped with the binary. User types live in `.rosary/types/*.capnp` and inherit from built-ins.

```capnp
@0xabc123...;

struct IssueType {
  name        @0  :Text;
  inherits    @1  :Text;                # "task" | "bug" | ... | other user type
  shape       @2  :Schema;              # required + optional fields beyond bead defaults
  guardrails  @3  :List(Guardrail);     # deterministic: schema, dedup, file overlap, secrets, custom
  pipeline    @4  :Pipeline;            # agent stages + LLM tier per stage
  sources     @5  :List(SourceMap);     # gh-issues | linear | beads | pr-threads | slack
  triggers    @6  :List(Trigger);       # cron | webhook | state-signature | manual
  success     @7  :SuccessCriteria;
  handoff     @8  :HandoffSpec;         # see section 5
}
```

### 2.1 Why capnp

- **Already in stack** — `cloister.capnp`, `cluster.capnp`, `config.capnp`. Toolchain present.
- **Canonical binary form** — schema → bytes is deterministic. Same schema everywhere produces same content hash. This is what makes the federation story (section 4) and trust attestation work.
- **Schema evolution rules** — field numbering gives forward/backward compat between rosary instances on different versions.
- **Codegen for Rust** — `capnpc-rust` emits typed accessors; type-specific fields land as real types, not `serde_json::Value`.
- **RPC primitive** — capnp-rpc is available if inter-instance middleware needs it later.

### 2.2 Per-repo + free layering

Schemas live in `.rosary/types/*.capnp`. Capnp's existing import-path resolution gives layering as a free property — adding `~/.rosary/types/` to the search path produces a personal layer with no new design surface:

```capnp
@0xdef456...;
using import "/task.capnp".Task;                              # built-in
using import "~/.rosary/types/my-defaults.capnp".MyDefaults;  # optional personal layer

struct StalePrTriage extends Task { ... }
```

What's **out of scope** for MVP: org-shared schema registries, ley-line-published content-addressed schema bundles. Those become trivial later when needed; nothing in MVP design prevents them.

### 2.3 Kubernetes-style inheritance

Inheritance merges fields from the parent; user types can add new fields, override defaults, or explicitly negate (e.g., `disabled_guardrails: ["file-overlap"]` for a type that legitimately needs to write same files concurrently). Negation makes the schema admit intent rather than hide it.

## 3. Pipeline & guardrails

### 3.1 Cost-tiered LLM ladder

Each pipeline stage declares its model tier. Stages cascade from cheapest to most capable; later stages run only on items that survive earlier filters.

| Tier | Model | Use |
|------|-------|-----|
| `needle` | Needle 26M (cactus-compute) | Classify, route, "is this worth thinking about" |
| `qwen` | Qwen3-Coder local (vLLM/Ollama) | Synthesize, draft bead body, structured extraction |
| `ds4` | DeepSeek V4 Flash via antirez/ds4 | Complex synthesis; benefits from on-disk KV cache for stable system prompts |
| `claude` | Claude API or claude-code subprocess | Escalation only |

Local LLMs run as subprocesses (see section 9 — llama.cpp runtime + interrupt mechanism are open questions). ds4's on-disk KV-cache-keyed-by-SHA1-of-token-IDs lets stable system prompts amortize across runs — cheap repeated invocation.

### 3.2 Deterministic guardrails (post-validation)

Every LLM output passes through a guardrail chain before any side effect. Existing rosary primitives become first-class `Guardrail` variants:

- `schema-validate` — output conforms to type's `shape`
- `dedup` — wraps `epic::is_dominated_by` (multi-signal similarity)
- `file-overlap` — wraps `epic::has_file_overlap`
- `secret-scrub` — wraps `src/secrets.rs`
- `policy` — type-specific predicates (e.g., "test command present", "scope set")
- `verify-plugin` — invokes a `pipeline.verify` plugin (existing); honors `doc_coverage_min`

Guardrail failures feed back as structured violations the LLM can re-attempt against, up to K retries. After K, the agent demotes itself and emits a handoff (section 5).

### 3.3 Non-moving falsification gates

A bead's success criteria are falsification gates. Once declared, they do not move retroactively. If a gate is missed, the bead stays open — even if other gates passed, even if the implementation produced "real value", even if mid-flight discovery suggests the gate was wrong.

If the gate was wrong, that observation is itself a finding (categories `processSmell` or `scopeCreep` in §5). A new bead with a new gate is the correct response. Retroactively softening the original gate to "honest about partial result" or "shipped progress" is the failure mode this rule prevents — the rule exists because *every other discipline in this spec depends on knowing whether a gate was actually hit*. Trust ramp, dedup, post-mortem rollup, federation merge — all of them treat `closed` as ground truth. If `closed` can mean "we lowered the bar," none of them work.

Concrete shape for the agent's self-rating in §5.3: report each gate's target and actual side-by-side. Words like "honest", "partial", "best-effort", or "essentially passing" do not appear in gate status reports. They appear at most as adjacent observations, never as modifiers on the status.

### 3.4 Trust ramp tied to shape tightness, not review count

Trust starting tier is a function of how tight the shape is, not how many humans approved:

| Shape tightness | Example | Starting tier |
|---|---|---|
| Hard schema, enum-only outputs | Classify P0/P1/P2/P3 | Auto-execute |
| Structured object with validated fields | `BeadSpec` with title, scope, deps | Human-gated, ramps fast |
| Free-form natural language | "Draft a summary paragraph" | Shadow only, rarely promotes |

Promotion/demotion is driven by `trustSignal` findings from self-narrated handoffs (section 5.3) — gated by §3.3, since `closed` must mean "all gates hit" for trust signal to mean anything. Human review only triggers when guardrails are ambiguous or a new shape is needed — humans set goals and shape, the runtime enforces, the agents execute.

## 4. Federation substrate (ADR-0010 + reflog)

ADR-0010 already accepted: G-set log + per-field algebra fold + flat-lattice cross-source merge. This design adds the **operation log surface** that feeds it.

### 4.1 Reflog of operations

Every local bead operation (create, update, comment, close, handoff emission) appends a capnp-typed record to a per-instance reflog. The reflog is the source of truth fed into `src/observation/log_sqlite.rs`. Operations are content-addressed (hash of canonical capnp bytes).

### 4.2 Portable trust unit

```
canonical capnp bytes
        ↓
content hash (SHA-256)
        ↓
passkey-signed assertion over hash
        ↓
attached envelope: { schema_id, content_hash, signature, signer_pubkey }
```

The envelope is the unit shipped between peers. Receivers verify the signature chain (`signer_pubkey ∈ trusted_pubkey_set`), then fold the operation into their G-set via the algebra registered for the field. Peers with divergent workflows fold independently — the lattice is commutative + associative + idempotent.

### 4.3 Passkeys, signet, and the OIDC tension

- **Passkeys**: device-bound credentials, synced across user devices via vendor E2EE (iCloud Keychain, 1Password). The same logical key signs from iPhone, MacBook, etc. — biometric unlock is local; the credential is one.
- **signet** issues device certificates anchored in the user's passkey identity. Agents in headless contexts use short-lived signet-issued delegation tokens (passkey requires user presence; agents can't touch the keychain autonomously).
- **OIDC tension**: signet has an OIDC layer. OIDC requires a formal IdP, which contradicts the decentralized federation model. **Resolution**: OIDC is used only for *outbound* integration with formal systems (Linear, GitHub, Slack). Inter-peer trust uses signet's device-cert PKI directly, with no central IdP. signet plays both roles; we don't conflate them.

### 4.4 Peer A vs Peer B with divergent workflows

Peer A may treat `tag:p0` as triage urgency; Peer B may treat it as roadmap commitment. Both ship operations into the same G-set. The per-field algebra for `tags` is an OR-set — both interpretations coexist. Each peer's *view* applies its own filter. Convergence at the algebra layer; divergence at the workflow layer. Adoption is per-peer.

## 5. Self-narrated handoffs

Generalizes [`src/handoff.rs`](../../src/handoff.rs) (currently phase-internal: dev→staging→prod) to **session-internal** lifetime — emitted at pause points, just before close, or on interrupt. First-person, written by the working agent.

### 5.1 Capnp shape

```capnp
struct AgentHandoff {
  beadId         @0  :Text;
  sessionId      @1  :Text;
  emittedAt      @2  :Timestamp;
  reason         @3  :Reason;            # pause | nearClose | interrupted | handoffToPeer

  # First-person experience
  whatWorked     @4  :List(Text);
  whatWasHard    @5  :List(Text);
  whatTried      @6  :List(Attempt);     # attempt with outcome
  deadEnds       @7  :List(Text);        # explicit "don't waste cycles here"
  openQuestions  @8  :List(Text);
  confidence     @9  :Confidence;        # high|medium|low + free-text rationale

  # Resume substrate
  nextMove       @10 :Text;              # "if I came back, this is what I'd do"
  contextHooks   @11 :List(FileRef);     # files/symbols/lines next agent should re-anchor on
  externalState  @12 :List(ExternalRef); # PRs, branches, running jobs in flight

  # Self-rating against type's subscribed FindingCategories
  selfRatings    @13 :List(SelfRating);  # agent's own call on findings
}

enum FindingCategory {
  designSmell     @0;  # work surfaced gross choices forced by design constraint
  processSmell    @1;  # the problem/process itself isn't being handled well
  scopeCreep      @2;  # diverged from original spec
  costAnomaly     @3;  # ~10x expected effort
  patternEmerging @4;  # Nth occurrence — promote to recurring agent?
  guardrailGap    @5;  # something slipped past guardrails that shouldn't have
  trustSignal     @6;  # explicit promote/demote evidence for the working agent
}
```

### 5.2 Three uses, one mechanism

The handoff reflog feeds three consumers:

1. **Resume** — next session reads the most recent handoff for the bead and picks up from `nextMove` + `contextHooks`. The AFK problem becomes a 90-second triage instead of re-reading PR descriptions.
2. **Trust ramp** — `selfRatings` with category `trustSignal` accumulate against the producing agent. Consistent `confidence: high` + downstream rejection demotes; consistent acceptance promotes.
3. **Federation payload** — handoffs are canonical capnp + signed (section 4.2). Peers ingest them and merge via the lattice.

### 5.3 Per-type subscription

Each `IssueType.handoff.subscribes` declares which `FindingCategory` variants this type asks its agents to self-rate against. A `bug` subscribes to `designSmell` + `guardrailGap`. A `chore` subscribes to `costAnomaly` + `patternEmerging`. Encourages minimal subscriptions to avoid finding inflation.

### 5.4 Interrupt mechanism is load-bearing

An interrupted agent **must** emit a handoff before its process exits, otherwise the resume story breaks. The interrupt mechanism (existing concept, needs design pass — section 9) is the trigger for `reason: interrupted` handoffs. SIGTERM-trap + bounded-grace-period is the minimum viable shape.

### 5.5 Cost

A handoff adds <500 tokens to the agent's closing turn — the model is already loaded with full context, so the marginal cost of "summarize what you did, what was hard, what's next" is negligible compared to a downstream third-party autopsy pass.

### 5.6 Recursion guard

Handoff emission **does not** trigger another handoff. `IssueType` for handoff-recording operations has `handoff.required = false` and a runtime invariant prevents nesting.

## 6. Adoption surface

Five tiers; same machinery, friction floor moves:

| Tier | Surface | Friction |
|---|---|---|
| 0 (today) | Built-in types via `rsry` CLI / MCP | Zero — unchanged |
| 1 | File `agent-spec` bead → BDR factory synthesizes capnp draft as comment → approve → lands | Conversation |
| 2 | Author capnp directly, inherit from built-ins | Schema literacy |
| 3 | Personal layer: drop schemas in `~/.rosary/types/` (free property of capnp import paths) | None beyond tier 2 |
| 4 (future, not MVP) | Publish capnp via ley-line content addressing for cross-instance reuse | Federation literacy |

Built-in types keep working exactly as today. New machinery is purely additive.

## 7. Cross-source middleware

Promote `src/sync.rs` from Linear-only to a generic `IssueTracker` trait with multiple impls. **GitHub is one provider** that surfaces issues, PR threads, PR review comments, and commit messages under a single namespace — per-source filters in the issue type spec decide which surfaces are first-class for that type.

```rust
trait IssueTracker {
    fn list(&self, filter: SourceFilter) -> Vec<SourceItem>;
    fn upsert(&self, canonical: CanonicalWorkItem) -> Result<ExternalRef>;
    fn subscribe(&self, sink: EventSink);
}
```

Translation between source representations uses the **capnp-typed canonical form as the intermediate**. Heterogeneous mappings (e.g., a Linear field with no GitHub equivalent) are resolved by the middleware LLM at the appropriate tier — the same pipeline machinery the rest of the system uses. The mapping is itself an issue type instance, attestable and reflog-recorded.

## 8. MVP vertical slice

Bounded to ~2 weeks of work. Validates the whole stack end-to-end on one real use case.

### Scope

- Re-express **one built-in type** (`task`) in capnp. All existing behavior preserved; load path tested for round-trip.
- Author **one user type** (`stale-pr-triage`) in `.rosary/types/`:
  - Cron trigger (nightly)
  - Source: beads (only — no GitHub provider yet)
  - Pipeline: `needle` filter (is this PR stale by basic mtime?) → `qwen` synthesizer (draft triage comment + bead body)
  - Guardrails: schema-validate, dedup, secret-scrub
  - Emits: P3 bead per stale PR
  - Subscribes to `trustSignal` only (cheapest post-mortem wiring)
- **Self-narrated handoff** path:
  - Generalize `src/handoff.rs` to session-internal lifetime
  - `stale-pr-triage` agent emits `AgentHandoff` on close
  - Reflog write path (capnp → SQLite, content-addressed)
  - Read-back on next dispatch of same bead — validates resume surface
- Single repo (rosary itself — dogfood).
- **In-process G-set merge only**; cross-instance reflog ship is phase 2.
- llama.cpp subprocess runtime; synchronous calls (interrupt mechanism phase 2).

### Out of scope for MVP (explicitly)

- All other built-ins capnp-ified (phase 2 — one type validates the loader)
- GitHubProvider IssueTracker impl
- ley-line-published schema bundles
- Cross-instance reflog ship + passkey-signed envelopes
- Agent factory CLI (`rsry agent synthesize`)
- llama.cpp interrupt mechanism wiring
- Multi-source canonical translation

### Exit criteria

- `cargo test -p rosary capnp::loader` — built-in `task` capnp round-trips losslessly
- `cargo test -p rosary issue_type::user_type` — `.rosary/types/stale-pr-triage.capnp` loads, inherits, validates
- `cargo test -p rosary pipeline::ladder` — Needle filter + Qwen synth wired, with mock backends
- `cargo test -p rosary handoff::session_internal` — handoff emits, persists, reads back across dispatches
- Manual: run nightly cron once locally against rosary's own open PRs, verify it files a `stale-pr-triage` bead per stale PR, with a handoff on close that resumes correctly on next dispatch

## 9. Open design questions (deferred to implementation)

These are real holes the spec acknowledges but doesn't close — flagged so they don't get implicitly decided during implementation.

1. **llama.cpp runtime + interrupt mechanism wiring.** Subprocess vs library binding for llama.cpp. How interrupt signals propagate (SIGTERM trap + grace period? mid-token cancellation API?). Phase-2 detail; MVP uses synchronous calls.
2. **Capnp schema versioning policy.** When does a field renumber require a migration vs work via capnp's own evolution rules? Probably codify in a `capnp-evolution-policy.md` separately.
3. **Reflog retention / compaction.** Per-bead handoff reflogs grow unbounded. Compaction policy (snapshot every N handoffs? keep last K?) needs benchmarking with real data.
4. **Trust ramp thresholds.** Concrete numbers (how many `trustSignal` rejections demote? promotion debounce?) need empirical tuning. MVP picks defaults; phase 2 makes them per-type-tunable.
5. **OIDC scope discipline.** Signet's OIDC layer must not leak into inter-peer trust paths. Code-level enforcement (e.g., separate crates) preferred over convention.
6. **Cost runaway guard for self-narration.** Per-type budget cap on handoff emissions per day, or just rely on the marginal cost being low? Probably leave uncapped initially with telemetry.

## 10. Phase 2 (explicitly out of MVP scope)

- Remaining built-ins (`bug`, `feature`, `epic`, `design`, `chore`) capnp-ified
- `GitHubProvider` `IssueTracker` impl (issues + PR threads + commit messages)
- Cross-instance reflog ship + passkey-signed envelopes (federation goes live)
- ley-line content-addressed schema bundle publish (tier 4 adoption)
- Agent factory CLI (`rsry agent synthesize` from `agent-spec` bead → capnp draft)
- llama.cpp interrupt mechanism wiring
- Multi-source canonical translation via middleware LLM
- Per-type trust ramp thresholds tunable in `IssueType.pipeline.trust`
- Linear `IssueTracker` impl upgraded to capnp canonical form (bidirectional)

## Appendix A: How this lands the prior conversations

This spec is the unification point for four conversations that kept feeling like the same conversation:

| Original question | How it lands here |
|---|---|
| Portable bead state sync | Canonical capnp bytes + content hash + passkey-signed envelope (section 4.2) |
| Public vs private repos | Per-peer subscription + tier-4 ley-line publish opt-in (section 6) |
| Passkey portability | Vendor E2EE sync of one credential; signet device certs for headless (section 4.3) |
| LLM as cron / middleware | Pipeline ladder per stage, declared in issue type, deterministic guardrails post-validate (section 3) |
| Agent factory from `(role, request)` | Dissolves into "file an issue type"; BDR machinery synthesizes (section 6, tier 1) |
| Per-type post-mortem | Reframed as agent's self-narration before close; same machinery serves resume + trust + federation (section 5) |
| Peer A vs Peer B with diverging workflows | ADR-0010 lattice handles divergence; per-peer view filters apply locally (section 4.4) |
| Adoption | Tier 0 unchanged; new machinery is purely additive (section 6) |
