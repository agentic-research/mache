# ADR-0024: Incremental Dataflow & Taint as Substrate Queries

Date: 2026-07-09
Status: Proposed

Relates to:

- ADR-0023 (Unified Code-Fact IR — property graph over a content-addressed symbol set; the fact substrate this generalizes to dataflow/taint)
- ADR-0016 (cross-language reference resolver — "separated presheaf on a coverage, not a sheaf")
- ADR-0013 (refs/defs canonical schema, `v_defs`/`v_refs` fidelity poset — the query surface a dataflow rule would extend)
- ley-line-open ADR-0026/0027 (merkle-AST content address + IR producer)
- ley-line-open's daemon **sheaf** (`rs/ll-open/cli-lib/src/daemon/sheaf_ops.rs`): regions (communities) as cells, restriction maps as edges, content-hash stalks, δ⁰-driven invalidation → "report changed regions, get structurally affected neighbors"
- Bead `mache-463612` (analysis-as-substrate-query thesis; this ADR is arm-C: dataflow/taint)
- Bead `mache-238673` (node_hash memoization for incremental rule eval — the subtree-level inner loop this rides)
- Bead `ley-line-open-caf423` (extraction fidelity: Python/JS zero-symbol, unqualified method tokens, empty `source_file` — the gating prerequisite, in progress upstream)
- Bead `mache-caaae9` (rules assume the Go-schema taxonomy — the cross-language correctness constraint)

## Context

Mache is, architecturally, a **query-over-facts engine** — SQLite (pure-Go
modernc, no CGO) over code facts, with `WITH RECURSIVE` for the recursive
cases. It already runs reachability queries: `dead_code`'s alive-check is
reachability over the ref graph; `get_callers`/`get_callees` traverse edges.
The natural next capability is **dataflow and taint analysis** (which value
reaches which sink, under which sanitizers).

Two observations motivate treating this as a substrate question rather than a
bolt-on:

1. **Invalidation ≈ taint.** Sheaf invalidation (a code change marks a region
   stale, and "stale" propagates over the restriction-edge graph to a fixpoint)
   and taint (a source marks a value tainted, and "tainted" propagates over the
   dataflow graph to a fixpoint) are the **same computation**: monotone
   propagation over a directed graph to a least fixpoint. They compose —
   *making taint incremental under edits is sheaf-invalidation applied to the
   taint relation.*

1. **Half of it already exists.** ley-line-open's daemon ships a genuine
   cellular **sheaf**: regions (the Louvain/connected-component communities) as
   cells, restriction maps as weighted edges (boundary hash + learned co-change
   rate), content-hash stalks, and δ⁰-driven invalidation that computes
   *structurally affected neighbors* from a change. mache shipped `node_hash`
   memoization (`mache-238673`) — subtree-level content-addressed caching. So
   the incremental machinery is present at two levels (region and subtree); what
   is missing is the **fine-grained dataflow facts** to run the query over.

### Honest starting state (the gaps)

- **No CFG.** Neither mache nor LLO emits a control-flow graph. It is buildable
  from `_ast` — the control-flow nodes are already there (we count
  `if`/`for`/`case` for `cyclomatic_complexity`) — but requires per-language
  control semantics.
- **Def-use is declaration-granularity, not variable-level.** `node_defs`/
  `node_refs` link a *symbol* def to its *reference* sites; there is no
  intra-procedural, variable/SSA-level def-use.
- **The ref graph is Go+Rust-only, bare-token, mention-fidelity.** LLO's HDC
  extraction covers only Go and Rust (`hdc_enrich.rs:31`); Python parses to
  `_ast` but yields **zero** `node_defs`; method tokens are unqualified; and
  `nodes.source_file` is empty after `leyline parse` (`ley-line-open-caf423`,
  in progress upstream).

## Prior art (how everyone else does this — as queries, not passes)

Verified against primary sources (deep-research 2026-07-09, 23/25 claims
confirmed by 3-vote adversarial verification):

- **CodeQL** models dataflow as a graph of *nodes* (classes) and *edges*
  (predicates), and structures **taint** as a reusable query configuration:
  `isSource` / `isSink` / `isBarrier` (sanitizers) / `isAdditionalFlowStep`.
  Plain dataflow vs taint is the *same signature* instantiated as
  `DataFlow::Global<Config>` vs `TaintTracking::Global<Config>` — taint = flow +
  non-value-preserving steps.¹² **Flow states** (metadata carried per tracked
  value) get field/object-level precision *at the query layer* — but they
  *refine* value-carrying base facts, they do not manufacture variable-level
  tracking from a declaration-granularity graph.³
- **Doop / P-Taint (Datalog).** Points-to *and* taint are Datalog rule sets;
  taint is a **clean orthogonal add-on to the unmodified points-to fixpoint** —
  variables point to real heap objects *and* artificial "taint objects", so
  taint rides the same recursive relation and needs changes to "only a handful
  of rules."⁴ **Critical caveat:** this minimalism is *contingent on a
  pre-existing points-to analysis* — which we do not have. (The maximal framing
  "taint *is* points-to, one fixpoint computes both" was **refuted** 1-2 by
  verifiers as overreach; the supported claim is the weaker "add-on to a
  pre-existing fixpoint.")
- **Meta's Glean** stores code facts as typed, uniquely-id'd **predicates**
  queried by structural pattern-match (Angle).⁵
- Datalog is, in the field's own words, "SQL with full recursion"; points-to is
  a transitive-closure fixpoint. The whole tradition is *analysis-as-recursive-
  queries-over-facts* — exactly mache's shape.

## Decision drivers

- Preserve mache's **pure-Go / no-CGO / SQLite** invariant (ADR-0012).
- The **prerequisites are fact-extraction problems** (CFG, variable-level
  def-use, cross-language coverage) — native to the Rust tree-sitter producer,
  independent of where the fixpoint runs.
- Reuse the **incremental machinery that already exists** (LLO sheaf + mache
  node_hash) rather than build a second one.
- **Incrementality is not free lunch.** Soufflé's *elastic* result: on real
  static-analysis workloads, large-impact changes make from-scratch
  recomputation **cheaper** than single-strategy incremental engines (which
  degrade or time out); the right design switches Bootstrap (recompute) vs
  Update (incremental) by measured change impact.⁷

## Considered options

### Option A — Hand-rolled SQL-IVM in mache

Extend the node_hash/sheaf memo into a general incremental-view-maintenance
engine in Go, express dataflow/taint as `WITH RECURSIVE` over the fact tables.

- **+** No new dependency; stays in the projector; reuses shipped node_hash memo.
- **−** Re-implements a Datalog/IVM engine by hand (the hard, bug-prone part).
- **−** SQLite `WITH RECURSIVE` is **too limited for a large share of these
  analyses** — mutual and non-linear recursion (which points-to/taint routinely
  need) are unsupported; a recent benchmark could not run ~half its recursive
  suite on SQL engines.⁸ We would be fighting the query language.

### Option B — Embedded Go Datalog (Mangle) in mache

Add Google **Mangle** (pure-Go, 99.1% Go, embeddable) as a query layer: it has
semi-naive evaluation, aggregation, and **stratified negation** (the primitive
sanitizers/barriers need).⁶

- **+** Pure-Go; real Datalog expressiveness (recursion + stratified negation)
  that SQL lacks; natural fit for taint rules.
- **−** **Pre-1.0** (v0.4.0, "not an officially supported Google product") —
  experimental dependency.
- **−** Its "incremental" is **intra-query semi-naive evaluation, NOT
  incremental view maintenance** across fact updates — it does *not* give
  change-driven recompute. So it solves *expressiveness*, not *incrementality*.

### Option C — Compute in the LLO producer (Rust), project in mache *(recommended)*

LLO builds CFG + variable-level def-use, runs the dataflow/taint fixpoint using
**differential-dataflow** (the idiomatic Rust IVM, driven by the existing sheaf
invalidation), and **materializes `_cfg` / `_dfg` / `_taint` fact tables** that
mache reads **read-only** — serving reachability via `WITH RECURSIVE` (or
materialized transitive-closure tables) over pre-computed edges.

- **+** The prerequisites (CFG, fine def-use, cross-language coverage) *must* be
  solved in the Rust extractor regardless — this puts the fixpoint next to the
  facts it needs.
- **+** Reuses LLO's **δ⁰ sheaf** as the change-driven incremental substrate;
  differential-dataflow natively handles insertions **and deletions** (taint
  retraction), which a monotone-only scheme does not.
- **+** Keeps mache a pure-Go read-only projector — no Mangle, no hand-built IVM
  in the no-CGO path.
- **−** Cross-repo coordination; the heavy analysis logic lives in Rust.
- **−** mache's query surface is then bounded by what LLO materializes (barriers/
  sanitizers likely pre-applied as filtered edges so the projector needs only
  positive recursion — see open questions).

## Decision

**Adopt Option C**: the incremental dataflow/taint **engine lives in the LLO
producer** (Rust + differential-dataflow, driven by the existing sheaf),
emitting materialized `_cfg`/`_dfg`/`_taint` fact tables; **mache stays the
pure-Go read-only projector**, exposing taint reachability as MCP tools /
filesystem via `WITH RECURSIVE` (or producer-materialized closure tables) over
those facts. **Mangle is held as a fallback** *only* for query-layer
expressiveness in mache if SQL recursion proves too limiting for a specific rule
— never as the incremental engine.

Follow Soufflé's **elastic** lesson: the producer chooses Bootstrap (full
recompute) vs Update (incremental via sheaf deltas) by change impact, rather
than an always-incremental scheme.⁷

**Scope the thesis honestly.** "Sheaf invalidation == making taint incremental"
is **sound as a north star** — both are monotone-fixpoint-over-a-graph, and
P/Taint shows taint rides the reachability fixpoint — but the equivalence only
*pays off after the CFG/def-use fact tables exist*. It is a direction, not a v1
shortcut, and P/Taint's minimalism is contingent on a pre-existing points-to we
must first build.

### Sequencing

1. **Prereq (upstream, in progress):** close `ley-line-open-caf423` — real
   def/ref extraction across languages, qualified method tokens, populated
   `source_file`.
1. **`_cfg`:** LLO emits an intra-procedural CFG from `_ast` (control-flow nodes
   already identified for `cyclomatic_complexity`).
1. **`_dfg`:** variable-level def-use edges (intra-procedural first).
1. **`_taint`:** a small rule set (source/sink/barrier/step) over `_dfg`, run in
   differential-dataflow, driven by sheaf deltas; barriers pre-applied as
   filtered edges.
1. **mache projection:** `find_dataflow` / `find_taint` MCP tools + a `taint/`
   virtual dir, `WITH RECURSIVE` over the materialized edges; promote to
   producer-materialized closure tables if query-time recursion degrades.

## Consequences & risks

- **Incremental ≠ always faster** (Soufflé elastic).⁷ Mitigation: impact-aware
  Bootstrap/Update in the producer; do not ship an always-incremental engine.
- \*\*SQLite recursion ceiling.\*\*⁸ Mutual/non-linear recursion in the projector is
  a real risk; mitigation is to push the fixpoint into the producer and
  materialize closures, leaving the projector positive/linear recursion only.
- **Mangle risk if we fall back:** pre-1.0 API churn; its "incremental" is not
  IVM. Only adopt for expressiveness, eyes open.
- **Fidelity ceiling:** flow-states/precision need value-carrying base facts;³
  field-level taint is bounded by extractor fidelity, not query cleverness — the
  work is in LLO.
- **Cross-repo blast radius:** the contract is the `_cfg`/`_dfg`/`_taint` table
  schema; version it like ADR-0023's fact IR.

## Open questions

1. Can LLO's current tree-sitter pipeline cheaply produce intra-procedural CFG +
   variable-level def-use? The whole plan is gated on it.
1. Can differential-dataflow in LLO be driven *directly* by sheaf region-
   invalidation events, and what is the concrete `node_hash`-change →
   differential-dataflow-collection-delta mapping?
1. Is SQLite `WITH RECURSIVE` performant enough for interactive taint queries at
   mache's fact volumes, or must the producer materialize transitive-closure
   tables?
1. Do sanitizers need stratified negation *at query time* (favoring Mangle), or
   can barriers be pre-applied as filtered edges in the producer so the
   projector needs only positive recursion? This decides whether Mangle is
   needed in mache at all.

## References

1. CodeQL — About data flow analysis. https://codeql.github.com/docs/writing-codeql-queries/about-data-flow-analysis/
1. GitHub — New dataflow API for custom CodeQL queries (isSource/isSink/isBarrier/isAdditionalFlowStep; DataFlow vs TaintTracking). https://github.blog/changelog/2023-08-14-new-dataflow-api-for-writing-custom-codeql-queries/
1. CodeQL — Using flow labels/states for precise data flow. https://codeql.github.com/docs/codeql-language-guides/using-flow-labels-for-precise-data-flow-analysis/
1. P/Taint: Unified Points-to and Taint Analysis (OOPSLA 2017). https://yanniss.github.io/ptaint-oopsla17.pdf · Doop: https://github.com/KnowSciEng/doop · Bravenboer & Smaragdakis, Strictly Declarative Specification of Points-to (OOPSLA 2009). https://dl.acm.org/doi/10.1145/1639949.1640108
1. Meta Glean — Angle guide (typed predicate facts). https://glean.software/docs/angle/guide/
1. Google Mangle (pure-Go Datalog; semi-naive; stratified negation). https://github.com/google/mangle · engine: https://pkg.go.dev/github.com/google/mangle/engine · analysis: https://pkg.go.dev/github.com/google/mangle/analysis
1. Elastic Incremental Datalog (Soufflé; Bootstrap vs Update; incremental not universally faster) — PPDP 2021. https://souffle-lang.github.io/pdf/ppdp21incremental.pdf · Soufflé parallel scale (SOAP 2017). https://dl.acm.org/doi/10.1145/3088515.3088522
1. Recursive-query limits of SQL engines (mutual/non-linear recursion unsupported on DuckDB/Umbra for ~half a recursive-analysis suite). https://arxiv.org/pdf/2511.00865
1. DDlog — Differential Datalog (compiles Datalog → Rust + differential-dataflow; minimal-work delta updates). https://github.com/vmware-archive/differential-datalog
1. IncA — Incremental whole-program analysis in Datalog with lattices. https://www.pl.informatik.uni-mainz.de/files/2021/04/inca-whole-program.pdf
1. Salsa (rust-analyzer incremental query system; memoized recompute-on-change). https://rustc-dev-guide.rust-lang.org/queries/salsa.html
