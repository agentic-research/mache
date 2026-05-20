# ADR-0009: Cross-Repo Linkage — Stratified Acyclicity + Modal Evidence

**Status:** Proposed
**Date:** 2026-04-30
**Depends on:** mache-iegm (`MultiRepoGraph` federation)
**Relates to:** ADR-0007 (BDR Enrichment Pipeline)

## Context

`LinkageStore` already persists cross-repo edges in the orchestrator backend
(`src/store.rs:100-166`). The data structure exists; the table exists in Dolt; it is
unused. Triage doesn't consult it, the MCP surface doesn't expose it, and humans
end up documenting cross-repo coupling in bead description prose instead.

Three forces hit this design at once and must be resolved together:

1. **mache will discover edges automatically** — once `MultiRepoGraph` (mache-iegm)
   federates per-repo `.db` artifacts, mache can walk symbol references, doc backlinks,
   and BDR `derived_from` chains across repos and emit `(from, to, evidence_kind)`
   records. We need a target shape for those records.

1. **Cycles are real work, not bugs** — repo A's bead "expose API" naturally cycles
   with repo B's bead "consume API" because the work is genuinely circular: A stubs,
   B writes the consumer to test, A iterates. Standard DAG models reject this.

1. **Three layers share the same algebraic shape** — mache's `ScopedRef{repo, token}`,
   rsry's `WorkRef{repo, bead_id, scope}`, and BDR's `ProvenanceRef::Code{repo, path,
   symbol}` all instantiate `{repo, id}`. The substrate built here generalizes.

This ADR locks the data model so multiple consumers (reconciler, MCP, mache writer,
janitor) can be implemented in parallel.

## Decision

### Graph structure: stratified acyclicity

Single edge table with a scope discriminator. Per-repo subgraphs `E_r` must be DAGs;
the global graph is allowed cycles **provided every cycle traverses at least one
cross-repo edge between distinct repos**. Equivalently: collapse each repo's
strongly-connected-component to a point and require the quotient is a DAG. Per-repo
Tarjan check at write time, O(V+E).

Cross-repo SCCs are **not an error** — they surface to triage as a "co-dispatch
needed" signal. Two cycle-bound beads in different repos can be dispatched concurrently
to break the fixed point.

A presheaf-of-graphs framing is technically defensible but operationally vacuous
(the base category over repos is discrete; restriction maps are trivial). Implement
as one table with two queries.

### Consistency: mache writes edges, reconciler reads status

**Mache only writes edges; it never writes status.** This is the load-bearing
decoupling. Edge discovery runs on mache's clock (~30 min); bead status reads cross
repo boundaries on the reconciler's clock (~30s) via the existing `pool.rs` snapshot
read. When B:bead-12 closes, A:bead-7 unblocks at the next reconciler tick — not at
the next mache scan.

Compute readiness on demand at triage:

```
is_blocked(b) := ∃ e ∈ deps_of(b).
                   e.dep_type = "blocks"
                ∧  e.tier ≥ Derived
                ∧  status(e.to) ≠ Closed
```

No incremental view, no cross-repo lock. Each repo's Dolt is its own consistency
boundary; the cross-repo read is a snapshot at decision time.

**Rejected alternatives:**
- **CRDTs** — wrong shape. Edges are append-mostly; status is single-writer per repo.
  No concurrent-write-to-same-cell to merge.
- **Stratified-negation Datalog / DDlog** — correct theoretical fit (readiness is
  non-monotone) but no production-ready Rust embedding. Differential-dataflow exists
  but is research-grade, not a query engine.

Safety/liveness tradeoff: trade *eager push-on-unblock* for *no global state*. The
30s reconciler tick is the liveness floor; a reactive layer would just double-cover.
Safety is preserved because each readiness query reads each repo's Dolt at the
moment of decision.

### Discovery soundness: 3-tier modal calculus

Reject probabilistic confidence as the primary axis. Bayesian confidence on a
discovery edge is underdetermined — no defensible prior, no principled update rule
when mache rescans the same artifact. The consumer (reconciler) needs a yes/no
decision at triage; a continuous value just gets thresholded into discrete tiers.
Skip the middle step.

Three modal tiers:

| Tier          | Source                                                          | Effect          |
| ------------- | --------------------------------------------------------------- | --------------- |
| `Asserted`    | Human or `rsry_bead_link --cross-repo`                          | Blocks dispatch |
| `Derived`     | BDR `derived_from`, `depends_on:` frontmatter, ProvenanceRef    | Blocks dispatch |
| `Conjectured` | Mache heuristics: markdown mentions a bead ID, Rust `use` edges | Annotates only  |

Promotion (Conjectured → Asserted) is a human action via `rsry_bead_link` or Linear.
Demotion is automated via TTL (see Failure Mode below).

Evidence is *not* consumed (linear-logic style); mache re-derives edges every scan.
Deduplication keys on `(from, to, evidence_kind, source_artifact_hash)`.

Structurally this is a collapsed provenance polynomial (Green/Karvounarakis/Tannen)
over a 3-level lattice. The collapse loses *why* an edge exists for tractability and
to give triage a binary answer.

### Schema delta

```sql
ALTER TABLE cross_repo_deps
  ADD COLUMN evidence_tier ENUM('asserted','derived','conjectured') NOT NULL,
  ADD COLUMN source        TEXT NOT NULL,            -- mache scan id, "human", agent name
  ADD COLUMN observed_at   TIMESTAMP NOT NULL;
```

Existing `from_repo / from_bead / to_repo / to_bead / dep_type` unchanged.

## Consequences

### Positive

- **One substrate, three layers.** Mache (mache-iegm), rsry, and BDR all use the
  same `{repo, id}` pointer shape and the same federation pattern. Whatever
  mache-iegm's `MultiRepoGraph` ships becomes the substrate; rsry surfaces it
  for work-tracking, BDR surfaces it for provenance.
- **Cycles modeled correctly.** Cross-repo SCCs become a co-dispatch signal, not a
  rejection. Real circular work (A↔B API) ships.
- **No central lock.** Each repo's Dolt remains an independent consistency boundary.
- **Mache decoupled from reconciler.** Edge discovery and dispatch run on
  independent clocks; mache failures degrade discovery, not dispatch.

### Negative / Failure mode

**Tier inflation via stale derivation.** Mache reads a `derived_from` chain whose
target file has moved or whose target bead has been re-numbered, but the chain's
textual content (and therefore its source-artifact hash) is unchanged. The stale
edge is promoted to `Derived`; it gates dispatch; work blocks indefinitely waiting
on a phantom blocker.

**Mitigation:** every `Derived` edge carries `last_observed_at`. A janitor pass
demotes Derived → Conjectured if mache hasn't re-confirmed within a TTL (7 days
starting). Costs a periodic sweep, not a global lock. Bound on damage: one TTL
window of false-blocking.

**Secondary worry:** the on-demand readiness query reaches across the connection
pool to read target-repo status. If a repo's Dolt is unavailable, **fail closed**:
treat blocking edges as still-blocking, surface a `degraded` operator signal, with
a configurable timeout. Fail-open trades silent stalls for silent dispatches of
genuinely-blocked work — worse.

### Neutral

- mache scan id becomes a first-class identifier consumed by rosary; this is a
  cross-tool coupling we accept (mache is already a hard dependency).
- Janitor adds one periodic sweep job (low frequency, bounded by TTL).

## Reasoning Trace

Full theoretical analysis lives at
`_agent_log/theoretical-foundations-analyst_2026-04-29_agent_log.md`.

## Open Questions

- TTL value (7 days starting; calibrate from production observability)
- Should `Conjectured` edges show in `rsry status` output, or only in a `--verbose`
  view? (Probably verbose-only — too noisy otherwise.)
- Promotion UX: Linear comment trigger, MCP tool, or CLI flag? (CLI flag first;
  others layer on later.)
