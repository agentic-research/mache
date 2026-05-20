# ADR-0010: Observation Lattice — Multi-Source Reconciliation Substrate

**Status:** Proposed
**Date:** 2026-05-05
**Depends on:** ADR-0005 (reactive store), ADR-0009 (cross-repo linkage)
**Relates to:** rosary-7023a9 (substrate bead), rosary-45518d (CRDT-lattice draft superseded)

## Context

Today rosary treats `Bead` as the source of truth and pushes derived state into
Linear and GitHub via one-off paths (`persist_status`, `linear_tracker`,
`github_mirror`). External systems are demoted to "downstream sinks." The author
wants to invert this: every external system — Linear, GitHub, git remotes, fork
state, beads themselves, future Slack/Notion/calendar — is a **peer source of
observations** about the same underlying work item. Rosary becomes the substrate
that ingests observations, computes a derived view via deterministic fold, and
surfaces conflicts + staleness without the user having to run commands.

Three forces shape the design:

1. **Anti-shadowban constraint.** Aggressive polling got the author's GitHub
   account flagged as a crypto miner in March 2026. The substrate must be
   **webhook-first**, with rate-budgeted polling as fallback only, and writes
   identity-attributed via signet ephemeral certs.

2. **Late, replayed, out-of-order observations.** Webhooks fail and re-send.
   Polls double-emit during retries. Manual user input arrives whenever. The
   fold must be invariant under re-ordering and idempotent on duplicates.

3. **Honest formalism.** A prior draft (rosary-45518d) called this a "CRDT
   lattice" with a chain `backlog < open < ... < done`. Math review (this ADR's
   companion analysis) found that framing decorative: `BeadState` has no
   `Ord` impl, `valid_transitions()` has back-edges (`Verifying → Open`), and
   the situation is observers recording an external object — not replicas
   doing concurrent edits. We adopt only the pieces that are actually
   load-bearing.

## Decision

### Substrate shape: G-set of observations + deterministic fold

The substrate is **append-only set of authenticated observations** plus a
**per-field deterministic fold** that produces the derived view. We do not
claim "CRDT" in the Shapiro 2011 sense — we have observers, not replicas.
Convergence here means: same set of observations → same derived view, in any
order. That follows from set membership (G-set) plus per-field algebras
chosen below; it does not require a global semilattice over the whole record.

```
Observation = {
    work_item: WorkRef,           // (repo, scope, bead_id) identity, see ADR-0009 + src/store.rs
    source: SourceId,             // "linear", "github", "git", "user", ...
    source_event_id: String,      // the webhook id / poll cursor / manual ack
    field: FieldName,             // "status" | "assignee" | "pr_url" | ...
    value: FieldValue,            // typed per field
    observed_at: Timestamp,       // wall-clock of the observation event
    cert: Option<SignetCert>,     // ephemeral cert; None for inbound webhooks
    payload_hash: ContentHash,    // BLAKE3 of canonical (source, field, value)
}
```

Dedup happens **before** fold ingest, on
`(source, source_event_id, payload_hash)`. Idempotence of the join is necessary
but not sufficient for correctness — content-hash dedup is what stops a
replayed webhook from being counted as two distinct observations.

### Per-field algebra (this is what's actually load-bearing)

Each field declares its algebra; the fold dispatches accordingly.

| Field | Algebra | Conflict semantics |
|-------|---------|-------------------|
| `pipeline_verdict` | **Chain-max** on `Dispatched < Verifying < Pass < PrOpen < Done` (the `Verdict` enum in `src/dolt/observations.rs`) | Monotone agent progress; max wins; back-step is not a transition |
| `assignee` | LWW-register, tiebreak `(observed_at, source_id)` | Latest valid observation wins |
| `pr_url` | LWW-register, tiebreak `(observed_at, source_id)` | Latest wins; nulled out only by explicit "unset" obs |
| `merge_sha` | LWW-register | Latest wins; immutable in practice once set |
| `deadline` | LWW-register | Latest wins |
| `ahead`, `behind` | LWW-register (numeric, replaces) | Latest poll wins; not accumulated |
| `comments` | OR-set with unique tags `(source, source_event_id)` | Add/remove are commutative; no LWW needed |
| `labels` | OR-set | Same as comments |

**`pipeline_verdict` legitimately has a chain ordering** because agent runs
within a bead are monotone — no agent ever steps a bead from `Pass` back to
`Dispatched`. The existing `src/dolt/observations.rs::Verdict` already
implements this chain by hand; under this ADR it becomes one registered field
in the substrate, not a separate mechanism. Note this is **distinct** from
`PipelineState::pipeline_phase` in `src/store.rs`, which is a `u8` index into
the agent sequence (dev=0, staging=1, prod=2, feature=3) — a different
quantity entirely. The ADR uses `pipeline_verdict` to avoid the name
collision.

**User-facing `status`** is **not a primitive field**. It is a *derivation*
over (`pipeline_verdict`, `deadletter`, `pr_url`, `merge_sha`, sink-reported
status) computed at read time per source. The substrate's job for status is
**conflict detection across sources** — when Linear's derived status disagrees
with GitHub's derived status for the same `WorkRef`, the cross-source
combination is the flat lattice with `⊤ = Conflict`. There is no single
field whose algebra is `flat lattice over status strings`; the flat lattice
applies to the *result of derivation, joined across sources*.

This separation is the load-bearing one: the chain (`backlog < open < ...`)
narrated in rosary-45518d is a workflow ordering, not a mathematical partial
order — back-edges in `valid_transitions()` make it not a poset, and the
"chain" framing only applies to the genuinely-monotone slice (`pipeline_verdict`).
Workflow validity is enforced separately (see LTS monotonicity below) and is
**not** part of the fold.

### LTS monotonicity is enforced at write-time, not at fold-time

Workflow validity (only allow `Queued → Dispatched`, etc.) is a property of
the labeled transition system over `BeadState`. It is checked at the
**source** (where rosary writes back to a sink) and at the **observer**
(where rosary records its own bead-internal transitions). It is **not** the
fold's job to reject a backwards transition; the fold is a deterministic
function of the observation set and must converge on whatever set it sees.

Stale or invalid observations from an observer are **quarantined** with a
diagnostic, not silently dropped. Quarantine state is queryable.

### Two trust boundaries: HMAC-at-the-door for inbound, signet cert for outbound

The substrate has two distinct authentication concerns that must not be
conflated. Both produce observations that land in the same log, but they
prove different things.

**Inbound from external sources (HMAC at the receiver).** When GitHub or
Linear fires a webhook to rosary, the receiver verifies the source's HMAC
signature against the configured webhook secret (`[github].webhook_secret`,
`[linear].webhook_secret`). That proves "this observation really came from
the source we registered." The observation lands in the log with
`cert: None` and `source` set to the verifying source. The user's signet
key is *not involved* here — GitHub does not sign with the user's key.

**Outbound from this rosary (signet ephemeral cert on user-authored obs).**
When the user (or an agent dispatching on the user's behalf) authors an
observation — manual bead closure, agent verdict, mirrored write to Linear —
the observation carries `cert: Some(SignetCert)`. The cert is an
*attestation of authorship*, not encryption. Anyone with the user's public
key can verify the signature and confirm "this came from the holder of the
matching private key"; only the private-key holder can produce one. This is
what enables federation (the "wasteland rig" model): another rosary instance
can mirror your observation log read-only and cryptographically verify
*which observations were authored by you* without trusting your hardware.

This split also clarifies what "GitHub as a source for the user's rsry DB"
means in practice: GitHub fires HMAC-signed webhooks; rosary verifies the
source-side signature, lands the observation tagged `source=github`,
`cert=None`. The user's key is not involved on the inbound path. Only when
the user (or their agent) writes back — to Linear, to GitHub, to Dolt — does
the signet cert attach.

A cert that fails validation produces a quarantined observation
(`QuarantineReason::InvalidCert`). Inbound webhook observations with
`cert: None` are **not** quarantined — that is the expected state for them.
What ingest checks: if `cert` is `Some(_)`, it must verify; if it is `None`,
the observation must come from a source whose HMAC verification already
passed at the receiver. There is no "valid cert" lattice element; cert state
is metadata that gates ingest, not a value the fold consults.

Encryption of observation content is orthogonal and out of scope for this
ADR — the existing ley-line / age-encrypted scoped-notes layer composes on
top for private observations a user does not want to share even with their
own federated mirrors.

### BDR parent-child is a tree fold, not a sheaf

Decade ⊃ Thread ⊃ Bead is a strict tree partition. There is no overlap and
no non-trivial restriction map — invoking "sheaf" would be decorative. Parent
state is computed by a deterministic bottom-up fold over child states (e.g.,
"thread is `done` iff all member beads are at terminal states"). This is a
catamorphism over the tree, not a CRDT operation.

### Relationship to existing pipeline observations

`src/dolt/observations.rs` (rosary-45518d) is **already an instance of this
substrate** — append-only observations of agent verdicts within a bead,
folded by chain-max — but only for one field (`pipeline_verdict`) and only
from one source (the in-process reconciler). It is not a separate
mechanism; under this ADR it becomes the canonical implementation of one
registered field.

This means the migration plan does **not** require rewriting that file.
Phase 1 lifts the algebra surface (field/algebra registry, fold dispatch,
quarantine path) into a generic substrate; phase 2+ adds new fields
(`assignee`, `pr_url`, etc.) and new sources (Linear, GitHub) that plug in
without touching the pipeline-phase code path.

The conceptual pivot is recognizing that the existing module was a special
case all along. The wrong claim in rosary-45518d wasn't "observations form
a lattice" — it was "the user-facing status string is the field that's
laticed." User-facing status is a *derivation*, not a primitive.

## Architecture

### Observer trait

```rust
#[async_trait]
pub trait Observer: Send + Sync {
    fn id(&self) -> &str;                          // "linear", "github_webhook", ...
    fn cadence(&self) -> Cadence;                  // Webhook | Poll(Duration) | OnDemand

    /// Produce zero or more observations from a wake event.
    async fn observe(&self, ctx: &ObserveCtx) -> Vec<Observation>;
}
```

Three concrete observers ship in v1, ported from existing one-off code paths:

| Observer | Replaces | Cadence |
|----------|----------|---------|
| `LinearObserver` | `linear_tracker.rs` polls + `persist_status` push | Webhook + 5min budget poll |
| `GithubObserver` | `github_webhook.rs` + `vcs::poll_pr_status` | Webhook + 5min budget poll |
| `BeadObserver` | direct Dolt reads from triage | OnDemand (every reconcile tick) |

Future observers (Slack, Notion, calendar, fork-state) drop in without
changes to the fold.

### Storage

Observations append into an `observations` table (orchestrator SQLite, mirrors
to Dolt for cross-repo). The derived view is materialized on read and cached
per `WorkRef`. Cache invalidation is keyed on a **monotonic ingest cursor**
(per-`WorkRef` max row id, not `observed_at`), so any new ingest invalidates
— including late/out-of-order observations whose `observed_at` is older than
what the cache already saw. Using ingest order rather than wall-clock prevents
stale derived views when retried/delayed webhooks land.

Quarantined observations append to a sibling `observations_quarantine` table
with a `reason` column. `rsry status --quarantine` surfaces them.

### Rate budget

The substrate maintains a per-source rate budget. Polls consume budget;
webhooks do not. Budget exhaustion does **not** retry — it logs and waits for
the next webhook or explicit user trigger. This is the anti-shadowban
guarantee.

## Test contract — the 14 invariants

These tests must all pass for the substrate's framing to hold. Each maps to
a question raised in math review.

| # | Invariant | What it tests |
|---|-----------|---------------|
| 1 | `chain_max_idempotent` | `pipeline_verdict=Pass ⊕ pipeline_verdict=Pass = Pass` |
| 2 | `chain_max_monotone` | `pipeline_verdict=Dispatched ⊕ pipeline_verdict=Pass = Pass` (max wins) |
| 3 | `chain_max_unranked_ignored` | `Fail` and `Deadletter` don't advance the chain |
| 4 | `lww_tiebreak_total` | LWW with equal `observed_at` resolved by `source_id` lex |
| 5 | `lww_unset_explicit` | `pr_url=None` requires explicit observation; never inferred |
| 6 | `or_set_add_remove_commute` | Comment add(c, t1) + remove(c, t1) in any order = absent |
| 7 | `or_set_unique_tags` | Same comment from two sources is two distinct entries |
| 8 | `dedup_before_fold` | Replaying a webhook with same `(source, source_event_id, payload_hash)` is a no-op |
| 9 | `reorder_invariance` | `fold(perm(O)) = fold(O)` for any permutation, all field types |
| 10 | `cross_source_status_conflict_is_top` | When two sources' derived status disagree, cross-source result is `⊤(Conflict)` with witnesses |
| 11 | `quarantine_does_not_join` | An invalid-cert observation never appears in the derived view |
| 12 | `quarantine_is_queryable` | Quarantined obs are surfaced via dedicated path, not silently dropped |
| 13 | `tree_fold_deterministic` | Decade/Thread/Bead rollup is a pure function of child states |
| 14 | `convergence_under_partition` | `fold(O₁ ∪ O₂) = merge(fold(O₁), fold(O₂))` for arbitrary partition, where `merge` is the pointwise per-field join (chain-max for chain fields, LWW tiebreak for LWW registers, set-union for OR-sets) lifted to derived views |

A `tests/observation_lattice.rs` integration test owns these 14. Anything
that breaks them is a substrate bug, not a fold-rule bug.

## What we are explicitly **not** claiming

- We are **not** claiming a CRDT in the Shapiro 2011 sense. We have observers,
  not replicas, and we use the discipline (G-set + per-field algebras) without
  importing the convergence theorem we don't need.
- We are **not** claiming `BeadState` is a poset under workflow transitions.
  It isn't (back-edges exist). The flat lattice is over **observed values
  for the field**, not over workflow steps.
- We are **not** claiming a sheaf for BDR hierarchy. It's a tree partition;
  the rollup is a catamorphism.
- We are **not** claiming progress monotonicity from the fold. The LTS gate
  is a separate write-time check at sinks.

## Migration path

Existing one-off paths port into `Observer` instances. Crucially,
`src/dolt/observations.rs` already implements the `pipeline_phase` field's
algebra correctly — it does not need rewriting, only adapting to the
substrate's registry interface.

1. **Phase 1** (this ADR's implementation PR): land observer trait, generic
   `Observation` type, field/algebra registry, fold, quarantine path, the
   14 tests. Pipeline-phase observations stay where they are; the substrate
   provides the surface that future fields and sources plug into. No
   behavior change.
2. **Phase 2**: register `pipeline_verdict` in the new substrate; have
   `dolt/observations.rs` write through it. Logically a no-op but proves
   the registry handles the existing field.
3. **Phase 3**: port `linear_tracker` push/pull into `LinearObserver`
   (writes `assignee`, `pr_url`, `labels`, `comments`). Run in shadow mode
   one week.
4. **Phase 4**: port `github_webhook` + `vcs::poll_pr_status` into
   `GithubObserver`. Same shadow mode.
5. **Phase 5**: cut `rsry status` over to read from the derived view —
   user-facing status is now a *derivation* over the field set, with
   cross-source conflict detection surfaced as `⊤`. Decommission the
   one-off paths.

The "Linear / GitHub / Dolt are equal peers" property emerges at Phase 5;
the preceding phases preserve current behavior while the substrate proves
out.

## Consequences

**Wins:**
- Single conflict-detection point. No more "Linear says X, Dolt says Y, GitHub
  says Z, who's right?" — `⊤ = Conflict` with witnesses.
- New sources (Slack, Notion, calendar, fork-state) plug in via `Observer`
  with no fold changes.
- Lossless audit trail by construction (G-set of observations).
- The `notme.bot` failure mode (account flagged for aggressive polling) is
  structurally prevented by the rate budget.

**Costs:**
- Storage growth for the observation log (mitigated by per-source retention
  and cold-archive — default 90 days hot, then move to a compressed archive
  table; configurable per source via `[observations.retention.<source>]` in
  `~/.rsry/config.toml`).
- Read path is fold-on-read with cache; stale cache means stale UI for ~one
  reconciler tick.
- Observers must declare their algebra correctly. A mis-declared field type
  (e.g., treating a multi-value field as LWW) silently loses observations.
  Mitigated by explicit `FieldName → Algebra` registry, reviewed at PR time.

## References

- Math review (companion analysis to this ADR) — establishes which formal
  claims are load-bearing vs decorative.
- ADR-0005 (reactive persistent store) — was going to be Dolt-as-substrate;
  this ADR reframes it as observation-log-as-substrate.
- ADR-0009 (cross-repo linkage) — `WorkRef` identity model carries through.
- rosary-45518d — the prior "CRDT lattice" framing this supersedes.
- `notme.bot/why` — the shadowban incident motivating the rate budget.
