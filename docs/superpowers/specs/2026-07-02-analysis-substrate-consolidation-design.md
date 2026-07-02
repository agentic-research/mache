# ART Analysis Substrate & Rule Consolidation — Design

**Status:** Draft (brainstorming output), 2026-07-02. A scoping artifact, not yet
decomposed into beads. Cross-repo: mache, ley-line-open (LLO), assay, cloister,
rosary, ley-line.

**Companion context:** external review "mache — remediation order" (M1/M2/M3),
decision-of-record `mache-1c332f` (assay↔mache convergence), ADR-0014 (event log
is the contract, `.db` is a projection), ADR-0006 (pure-Go, CGO removed).

______________________________________________________________________

## 1. Thesis (one paragraph)

Everything is a projection of one substrate. AST/CST, LSP types, mache schemas,
`find_smells` opinions, reachability, and dataflow are **orthogonal views over the
same parsed program**. The *contract* is LLO's capnp-schema'd event log; the `.db`
is its at-rest SQL projection, which mache's `find_smells` queries. The entire plan
is a single move applied repeatedly: **enrich the substrate** (LLO bakes more
facts) and **add projections** (mache SQL rules) — never bolt on external tools or
shell out at gate time. Rule consolidation, assay absorption, and the "next level"
(reachability/dataflow) all fall out of that one move.

______________________________________________________________________

## 2. Layer model — "are we going in circles?" (No: a spiral)

There is not one contract; there are **encodings of one data model**, and capnp is
the *schema that formalizes the model*, not a replacement for the `.db`:

| Layer                          | Form                                                      | Role                                                                     |
| ------------------------------ | --------------------------------------------------------- | ------------------------------------------------------------------------ |
| **Contract / source of truth** | LLO event log, schema in **capnp**                        | The formal contract (ADR-0014).                                          |
| **At-rest projection**         | the **`.db`** (SQLite `_ast`/`nodes`/`node_defs`/`_lsp*`) | Derivative of the log; regenerable. What `find_smells` queries with SQL. |
| **Live wire**                  | daemon UDS, **capnp-framed** (`mache-75d847`)             | Streaming/incremental enrichment + sheaf events.                         |
| **In-memory**                  | the **arena** (zero-copy)                                 | Runtime form Go reads.                                                   |

The capnp move **promoted** the event log to the formal contract and **demoted** the
`.db` from de-facto contract (fragile: a column rename was a silent break) to an
explicit projection. So "the substrate is the seam" means precisely: **the capnp
data model is the contract; the `.db` is its SQL face that mache analyzes.** Same
projection thesis, one level up. Going full circle would be "abandon capnp, treat
the raw `.db` as the contract again"; this design does the opposite.

**Consequence for enrichment:** a new fact type is a **capnp schema addition** →
projects into a new `.db` table → `find_smells` gets a new SQL rule over it.
`find_smells` keeps reading the `.db` projection because SQL-over-SQLite is the right
tool for *bulk, whole-repo* analysis; the capnp wire is right for *live, incremental*
enrichment. Same model, two faces, each used where it is strong.

______________________________________________________________________

## 3. Principles

1. **Share data, never link engines.** The arena works because it is *data with a
   schema*, not *code behind an ABI*. Embedding `semgrep-core` (OCaml app, no stable
   C API, GC-in-host, huge dep tree) is the shell-out pattern moved into the linker —
   rejected. OCaml↔Rust FFI is real in principle; it is the wrong tool here.
1. **Two shell-out classes.** *Build-time enrichment* (gopls → `_lsp*` baked into the
   `.db`, amortized into the artifact) is fine — LLO already does it. *Runtime-gate
   shell-out* (invoking semgrep on every `task check`) is the rejected pattern. **Gates
   read baked facts only.**
1. **No new rule engine where SQL-over-`_ast` suffices.** mache already ships
   AST-pattern rules (`magic_int_in_comparison`, `sleep_in_test`). "Native pattern
   matching" is a proven, shipping class, not reinvention.
1. **Dispatch by capability, not by tool.** structural/containment → SQL over `_ast`;
   ordering/dataflow/taint → the reachability/dataflow tier (native; taint deferred).

______________________________________________________________________

## 4. Current state (six repos)

| Repo              | Rule system today                         | Shape                                                   | Relation to substrate                                                   |
| ----------------- | ----------------------------------------- | ------------------------------------------------------- | ----------------------------------------------------------------------- |
| **mache**         | `find_smells`                             | SQL over `.db` + `MACHE_SMELL_RULES_DIR` external packs | **is the query engine**                                                 |
| **cloister**      | `done-rules/*.json` + 14 `lint-*.mjs`     | JSON drop-in ("mache-shaped") + bespoke shell/JS        | own runner                                                              |
| **assay**         | doc-coverage / staleness                  | Go tree-sitter, **reimplements traversal, Go-only**     | *redundant re-derivation*                                               |
| **ley-line-open** | 1 rule: `lint:blake3` (shell + allowlist) | domain invariant                                        | **produces** the substrate                                              |
| **ley-line**      | none (clippy/rustfmt)                     | standard                                                | produces `_ast` via sheaf                                               |
| **rosary**        | 2 semgrep + 4 shell "Golden Rules"        | semgrep YAML + shell ratchet                            | **only current consumer** of `find_smells` (`task god-files`, advisory) |

Key facts: no unified registry exists; rosary is the sole (advisory) consumer of
`find_smells`; three paradigms in use (SQL, semgrep, shell); the only ratchet is
rosary's god-files-vs-`origin/main`; the smell-debt baseline doc is observational.

______________________________________________________________________

## 5. The spine: `extract_rust` → reachability

**`extract_rust` is the keystone.** LLO's `rs/ll-open/ts/src/refs.rs` has only
`extract_go`, so `node_defs`/`node_refs` (the call/symbol graph) are **empty for
Rust**. Every `node_defs`-based rule (`god_file`, `dead_code`, `duplicate_definitions`,
`fan_out_skew`) and any reachability analysis therefore **returns nothing on the three
Rust repos**. Structural consolidation across the ecosystem is blocked on this one LLO
gap. (Note: *pattern* rules that need only `_ast` — and semgrep-class syntactic checks
— do **not** need it, so some consolidation can start before it lands.)

**Reachability is the "next level," built native.** mache's existing `dead_code` rule
is already a baby reachability analysis over the call graph. Generalized:

- **Near-term / in scope:** call-graph reachability = a recursive CTE over
  `node_defs`/`node_refs` → native `find_smells` SQL rules (dead-code-by-reachability,
  entry-point analysis, "is this export ever reached"). Unblocked by `extract_rust`.
  No new engine.
- **Horizon / out of scope here:** intra-procedural taint/dataflow ("tainted input
  reaches a sink") needs statement-order + variable-flow facts — a build-time LLO tier,
  research-shaped. This is what semgrep-Pro sells; it is the north star, not this plan.

______________________________________________________________________

## 6. Work streams (decomposition)

Each becomes its own bead cluster / plan. Effort tags: S = hours, M = days, L = weeks.

- **W0 — Quick wins (standalone, now).** M1: arena `Version:1` negative-rejection test
  (mirrors the existing magic-guard test). M2: tighten `TestWatcher_TargetIgnored` to
  assert watched-path/FD count, not callback count. Effort: S each. No dependencies.
- **W1 — LLO push (keystone).** `extract_rust` in `refs.rs` (Rust def/ref extraction →
  populates `node_defs`/`node_refs` for Rust); the `leyline_version` daemon op
  (`ley-line-open-b9db7f`); **M3** client-side handshake in mache (compare embedded
  schema-client version vs `compat_min`/`wire_format_major`, refuse on mismatch; closes
  `mache-8kif`, benefits both repos). Effort: `extract_rust` M–L (sizing risk); version
  op + M3 M. **Gates W2 and the polyglot half of W4.**
- **W2 — Reachability projection.** Native `find_smells` SQL rules over the call graph
  (generalize `dead_code`; add reachability/entry-point rules). Likely needs a per-language
  "roots" config (what counts as an entry point). Effort: M. **Depends on W1.**
- **W3 — assay → mache.** Fold assay's falsifiable coverage into mache as language-agnostic
  `drift_doc_*` SQL rules reading the `.db` (per `mache-1c332f`: `drift_doc_undocumented_export`
  = `mache-1bdc95`, staleness = `mache-fa12b3`); delete assay's Go tree-sitter traversal
  (`assay-b8d291`). assay keeps only its genuinely-novel frontier (semantic MiniLM↔HDC).
  Effort: M. Partially depends on W1 for polyglot; Go coverage works today.
- **W4 — Rule consolidation.** Port the *structural class* of cloister `done-rules` +
  rosary Golden Rules into `find_smells` rules/manifest; wire rosary + cloister to consume
  `find_smells` as **real gates** (not advisory `|| true`); ship `find_smells` as a
  distributable/standalone lint (`mache-445887`). Effort: M–L. Depends on W5 (manifest) +
  W1 (for Rust structural rules).
- **W5 — Manifest + baseline/ratchet + CI (the thin unifying layer).** One rule manifest
  (id · class · engine · severity · tags · scope), one findings output format, one
  baseline/ratchet (generalize rosary's god-files-vs-`origin/main` + the smell-debt
  baseline doc into an enforced ratchet), one CI entry (`task check` → `mache <run>`).
  Generalizes cloister's `done-rules` JSON shape. Effort: M. **Underlies W4.**

### Dependency graph

```
W0  (independent, do now)
W1: extract_rust ──┬──► W2 reachability
                   ├──► W4 (polyglot structural rules)
                   └──► W3 (polyglot coverage; Go works pre-W1)
W5: manifest/ratchet ──► W4
```

### Recommended sequence

1. **W0 now** — bank the two tests (highest ROI, S, no deps).
1. **W1** — `extract_rust` first (keystone) alongside version-op/M3.
1. **W2 + W3** in parallel once W1 lands (both ride the enriched substrate).
1. **W5 then W4** — manifest before wiring consumers onto it.

______________________________________________________________________

## 7. Scope boundaries

**In scope:** substrate enrichment (`extract_rust`), call-graph reachability rules,
assay absorption, structural-rule consolidation, the manifest + enforced ratchet, the
W0 test wins, the version handshake.

**Out of scope (north star / explicitly deferred):** intra-procedural dataflow/taint;
embedding or shelling out to any external engine (semgrep/ast-grep) at gate time;
reimplementing semgrep; wholesale physical code relocation — **federation via the `.db`
stays**; a formal federated rule registry with paradigm tags + scope lattice + sheaf
composition (ADR-0018's long-term note — earns its place only when a second hard
consumer demands it).

**Stays paradigm-local (does not consolidate):** rosary's `audit-log-before-status-change`
semgrep rule (an *ordering* property — reproduce as an `_ast` SQL rule only if statement
sequence is present in the table; otherwise leave it in rosary until the dataflow tier);
LLO's `blake3` allowlist (a domain invariant, correctly local); ley-line (nothing to
consolidate).

______________________________________________________________________

## 8. Risks & open questions

1. **`extract_rust` sizing (the gating risk).** Rust def/ref extraction in LLO is the
   keystone and its effort is an outside guess (M–L). Everything polyglot waits on it.
   *Mitigation:* Go-only structural rules + all `_ast` pattern rules + Go assay coverage
   proceed without it, so W3/W4 have real pre-W1 slices.
1. **Reachability entry-point roots.** Call-graph reachability needs a per-language notion
   of "roots" (`main`, exported API, `#[test]`, cli handlers). Needs a small config, not
   a new engine.
1. **Manifest format.** Generalize cloister's `done-rules/*.json` shape, or define fresh?
   Bias: extend the proven cloister shape rather than invent.
1. **The ordering rule.** Whether rosary's audit-ordering rule is expressible as SQL over
   `_ast` depends on whether statement order is in the table. Verify before promising it;
   default to leaving it in rosary.
1. **Consumer buy-in.** W4 flips rosary/cloister gates from advisory to enforcing — a
   behavior change for those repos. Land the ratchet first so nothing regresses on day one.

______________________________________________________________________

## 9. What this obsoletes

By making the substrate rich enough that its projections fall out of a `SELECT`, this
plan **obsoletes** (rather than integrates): assay's duplicate traversal, the case for
embedding/shelling-out semgrep, and the per-repo bespoke structural checks. semgrep's
remaining differentiator (dataflow/taint) becomes the north-star tier — owned natively,
baked once, queried forever — not a runtime dependency.
