---
title: "ADR-0013: Refs/Defs Canonical Schema (Fidelity Poset Over Producers)"
status: Proposed
date: 2026-05-05
tags: [architecture, refs, defs, canonical-schema, fidelity-poset]
---

**Supersedes:** implicit two-table consumer-branching pattern (`node_refs`/`node_defs` vs. `_lsp_refs`/`_lsp_defs` queried by separate code paths)

## Context

Mache's structural query layer reads from two physically separate tables that
encode the same logical concept (a "reference" edge) at different fidelities:

- **Tree-sitter producer** (in-process, or via ley-line-open's `leyline-ts`):
  syntactic, polyglot, fast, **shallow**. Writes:

  - `node_defs(token TEXT, node_id TEXT, PRIMARY KEY(token, node_id))` — "the
    symbol named `token` is defined at `node_id`."
  - `node_refs(token TEXT, node_id TEXT, PRIMARY KEY(token, node_id))` — "the
    node at `node_id` references **something** named `token`." Dispatch is
    unresolved; `obj.Read()` and a free function `Read()` produce the same row.

- **LSP producer** (ley-line-open's `leyline-lsp`, spawning per-language
  servers — `gopls`, `rust-analyzer`, `pyright`, `clangd`, `jdtls`, `zls`,
  etc., 28 languages): semantic, type-resolved, slower. Writes:

  - `_lsp_defs(node_id, def_uri, def_start_line, ..., def_end_col)` — definition
    location for a node.
  - `_lsp_refs(node_id, ref_uri, ref_start_line, ..., ref_end_col)` — reference
    location for a node, with **dispatch already resolved** (LSP knows which
    `Read` is meant when the source says `r.Read()` and `r` is `io.Reader`).

These are not isomorphic. Two independent mismatches:

1. **Direction is flipped.** `node_refs.node_id` is the *referrer*; `_lsp_refs.node_id`
   is the *referenced definition*. Reversible — costs an index, not information.
1. **The non-keyed endpoint lives in different ontologies.** Tree-sitter's
   "other end" is a *string* (`token`); LSP's "other end" is a *byte range*
   (`ref_uri + line:col`). Tree-sitter doesn't know which definition a token
   resolves to. LSP doesn't store the textual lemma it resolved through.

The ontology mismatch is asymmetric: **LSP knows tree-sitter's view, tree-sitter
does not know LSP's view.** Given a `_lsp_refs` row you can recover the textual
token by reading `ref_uri[ref_start..ref_end]`. Given a `node_refs` row you
cannot in general recover which definition was meant — that's exactly the
`dead_code` skip-list bug.

## Symptoms in the current code

1. **Consumer-side coupling**. `cmd/serve_find_smells.go` registers nine SQL
   rules; every rule joins `node_refs`/`node_defs`/`_ast`/`nodes` and **never**
   touches `_lsp_*`. The richer LSP data sits in the same database, ignored.

1. **Hardcoded skip-list as a tree-sitter compensation**. The `dead_code` rule
   (line 95–200) ships a SQL skip-list of interface methods (`String`, `Error`,
   `Read`, `Write`, `Close`, `MarshalJSON`, `ServeHTTP`, …) plus prefix patterns
   (`Test%`, `Benchmark%`, `Example%`, `Fuzz%`). The rule comment admits it:
   *"these are interface contracts invoked by external runtimes / libraries;
   static call extraction can't see the dispatch site, so the methods always
   look dead."* With LSP-resolved refs, the skip-list becomes redundant for
   the cases LSP actually sees.

1. **Two query paths in the consumer for one logical concept**.
   `cmd/serve_lsp.go:queryLSPDefs` and `queryLSPRefs` implement a
   "suffix-match, fall back to broader LIKE" pattern because `_lsp_*.node_id`
   has no `token` column — callers reverse-engineer it from `node_id LIKE`
   matching at query time.

## Decision: model the producers as a fidelity poset, project at write time

The right abstraction is **not a refinement lattice** (which would imply meets
and joins both directions). The projection between fidelities is one-way.
Use a **poset of fidelity levels with a forgetful functor**:

```
  L₀ = mention      (a node uses a name)              ← tree-sitter
  L₁ = binding      (a node uses *this* definition)   ← LSP
  L₂ = reachability (this binding is executed)         ← future SSA
```

For `j ≥ i`, the projection `π_{j → i}: L_j → L_i` strips fidelity. Concretely
`π_{1 → 0}(lsp_ref) = (referrer_node, token_at_ref_uri)` — keep the textual
mention, drop the binding. Higher producers project *down* into lower views;
lower producers don't project up.

**The canonical view at level L_i is `⋃_j π_{j → i}(R_j)`** where `R_j` is the
rows produced at level `j`. Higher fidelity dominates without a merge step;
the dominance is the projection doing its job, not a CASE in a view definition.

### Concrete schema

Producer tables stay physically separate (no collapse into one wide table —
that's "schema-as-disjunction = consumer-side branching by another name").
Consumers query **canonical views** that union them:

```sql
CREATE VIEW v_defs AS
  SELECT token, node_id, 'mention' AS fidelity FROM node_defs
  UNION ALL
  SELECT def_token, node_id, 'binding' FROM _lsp_defs;

CREATE VIEW v_refs AS
  SELECT node_id AS referrer_node_id, token,
         NULL AS target_node_id, NULL AS ref_uri, NULL AS ref_line,
         'mention' AS fidelity
  FROM node_refs
  UNION ALL
  SELECT referrer_node_id, ref_token, node_id AS target_node_id,
         ref_uri, ref_start_line AS ref_line, 'binding'
  FROM _lsp_refs;
```

For this to be a trivial UNION ALL with no helper functions, **the LSP producer
must extract `ref_token` and `referrer_node_id` at write time** (it has source
bytes loaded — one `tree.root_node().descendant_for_byte_range(...).utf8_text(...)`
call per row). This is the migration's biggest single win: token extraction
happens once, in the producer that has the bytes already open.

### Rule rewrites

`dead_code` (and siblings) become correct by construction:

```sql
WITH alive AS (
  SELECT DISTINCT d.node_id
  FROM v_defs d
  LEFT JOIN v_refs r
    ON r.target_node_id = d.node_id           -- L₁: exact binding
    OR (r.target_node_id IS NULL              -- L₀: token fallback
        AND r.token = d.token)
  WHERE r.referrer_node_id IS NOT NULL
)
SELECT d.node_id FROM v_defs d
WHERE d.node_id NOT IN (SELECT node_id FROM alive);
```

The interface-method skip-list **disappears from rule SQL**. Where LSP coverage
exists, binding rows mark `Read`, `ServeHTTP`, etc. as referenced — LSP
already resolved the dispatch. Where LSP coverage is absent (build-tagged
files, generated code, reflection), the OR-fallback to token match runs and
the rule degrades to today's behavior. Same SQL, no consumer branching.

The skip-list survives only as **external per-rule config** for the lattice
ceiling — edges no static producer can see (reflection, runtime code-gen
dispatch, JSON Marshal/Unmarshal). The `MACHE_SMELL_RULES_DIR` mechanism
already supports this; the rule body becomes shorter and the skip set becomes
a config artifact tagged with "edges below the lattice ceiling."

## Wedge cases (where the design folds — be honest)

1. **LSP coverage is partial, not total.** gopls indexes `.go` files in modules
   it can resolve; ignores `//go:build ignore`, vendored generated code,
   non-resolvable build tags. Pyright misses dynamic `__getattr__`. clangd
   misses unresolved `#include`s. So `_lsp_refs` is a *subset* of true bindings.
   A token only ever called from a build-tagged file would still be flagged
   `dead_code` because no binding row exists. Mitigation: add an
   `_index_coverage(source_id, producer, fidelity, complete)` table the
   producers populate; consumers can tell "no binding row exists" from
   "no binding row was looked for."

1. **Indexers disagree on what "reference" means.** gopls counts `&Foo{}` as a
   reference to type `Foo`; pyright counts `Foo()` but not `Foo` as a value;
   rust-analyzer's reference set differs from the others on trait impls and
   macro expansions. The poset assumes one "reference" relation; in practice
   it's a family indexed by language. Fine for `find_callers` (any-of
   semantics work). Breaks metric-bearing rules (`fan_out_skew`) because the
   metric becomes language-dependent. Mitigation: scope metric rules to a
   single language at query time; document that cross-language metric
   comparisons aren't valid.

1. **Reflection / generated dispatch / macro expansion.**
   `reflect.ValueOf(x).MethodByName("Read")` is invisible to gopls just like
   it's invisible to tree-sitter. SSA wouldn't catch it either. The poset has
   a **ceiling** — there exist edges `L_2` doesn't reach. The skip-list isn't
   going away forever; it's going away for the cases LSP *does* see.

## Migration plan

Five steps, reversible until step 4. Each step ships independently and the
test suite stays green throughout.

### Step 1 — Producer-side token extraction in ley-line-open

Have `leyline-lsp` (and `leyline-ts` if it doesn't already) write the new
columns at write time:

- `_lsp_refs(referrer_node_id, ref_token, target_node_id, ref_uri, ref_line, ref_col)`
- `_lsp_defs(node_id, def_token, def_uri, def_start_line, ...)`

Producer has source bytes loaded; this is one `descendant_for_byte_range`
call per row. Cross-repo: lands in `~/remotes/art/ley-line` first, then mache
adopts. **Keystone bead** — unblocks steps 3, 4, 5 and both falsifiability
experiments.

### Step 2 — Index coverage tracking

Add `_index_coverage(source_id, producer, fidelity, indexed_at, complete BOOL)`.
Both producers write a row per source file they touched, with completeness
flag set when the indexer claims full coverage. Resolves wedge case (1) —
distinguishes "no binding row exists" from "no binding row was looked for."
Independent of step 1; can land in either order.

### Step 3 — Canonical views

Define `v_defs` and `v_refs` as SQLite views (or materialized tables —
decision depends on query plan inspection). Both producers' tables flow
through the view contract. Depends on step 1 (so the UNION ALL is trivial,
no helper functions or `_source` byte-range joins at query time).

### Step 4 — Rewrite rules against views

`dead_code`, `untested_function`, `duplicate_definitions`, `fan_out_skew`,
`god_file`: stop reading `node_refs`/`node_defs` directly; read `v_refs`/
`v_defs`. Skip-lists migrate to per-rule external config with explicit
"lattice ceiling" annotations. `find_callers` and `find_callees` likewise.
Depends on step 3. **Commitment point** — once rules read views, removing
the views requires rewriting them back.

### Step 5 — Drop the suffix-then-LIKE fallback

`queryLSPDefs` and `queryLSPRefs` in `cmd/serve_lsp.go` lose the broader-LIKE
fallback once `_lsp_refs.referrer_node_id` is a real column. Replace with a
direct join through `v_refs`. Pure consumer-side cleanup; depends on step 1.

## Falsifiability

Two experiments — **first-class deliverables, not afterthoughts.** Both
land as PR-ready beads. The user's directive: "have a way to falsifiably
prove it works."

### Experiment A — Skip-list ablation

**First-class deliverable**, not an afterthought. With `_lsp_refs` populated
by ley-line-open's gopls run (step 1), drop the skip-list from `dead_code`
entirely and run on the mache repo. Compare against the current
(skip-list-aware) results.

- **Pass**: the diff is empty or contains only methods on types that the
  skip-list catches but gopls also misses (genuine LSP-coverage gaps —
  wedge case 1).
- **Fail**: a method that gopls indexed gets flagged dead because the new
  rule isn't using `v_refs.target_node_id`. That's a bug in the view, not
  in the design.

If A passes, the skip-list can be deleted from production rules with
confidence. Depends on steps 1 and 4.

### Experiment B — Fidelity-projection round-trip

**First-class deliverable**, not an afterthought. For every `_lsp_refs` row,
compute `(referrer_node_id, token)` via the projection `π_{1 → 0}` and
check that the resulting pair appears in `node_refs` (modulo tree-sitter's
known blind spots — function values, type-only references the call
extractor doesn't see).

- **Pass**: every projected row has a matching textual mention, with
  documented exceptions.
- **Fail**: the producer at level `L_1` is reporting edges that the
  level-`L_0` producer should also see textually but doesn't — meaning
  either tree-sitter's call extraction is missing patterns, or the
  projection is wrong (e.g. extracted the wrong byte range as `token`).

If A and B both pass, the design is sound and the skip-list can be deleted
from production rules with confidence. Depends on step 1.

## Avoiding duplicated code

The whole point of the canonical view layer is to **eliminate the duplicate
query path** in `queryLSPDefs`/`queryLSPRefs` (which today contains the same
"suffix match, fall back to broader LIKE" pattern twice). Producers also
stop duplicating token-extraction work: tree-sitter and LSP both compute
`token` from source bytes today, in different code paths, in different
languages. After step 1, the LSP producer extracts once at write time;
consumers never re-extract.

## Consequences

### Positive

- `dead_code` skip-list shrinks to "above-ceiling" edges only; everything
  LSP can see disappears from the SQL.
- One canonical query path replaces two; `queryLSPDefs`/`queryLSPRefs`
  fallback dies.
- New producers (future SSA, hand-curated, etc.) plug in without rule changes.
- Cross-repo asymmetry between mache and ley-line-open is now mediated by a
  documented schema contract, not implicit table discovery.

### Negative

- Step 1 is producer work in ley-line-open (Rust, separate repo) before
  mache benefits. Coordination overhead.
- View definitions are SQL — not all of the projection can be expressed
  tersely; `_index_coverage` joins make some queries verbose.
- Rule authors gain a knob (which fidelity to query at) and might pick
  wrong. Mitigation: default to `v_refs` and document when not to.

### Reversibility

Steps 1–3 are reversible: producer tables stay, views can be dropped, rules
keep working against the legacy tables. Step 4 is the commitment point —
once the rules read views, removing the views requires rewriting them back.

## References

- Math friend analysis transcript (this session, 2026-05-05)
- `cmd/serve_find_smells.go:95-200` — `dead_code` rule with skip-list
- `cmd/serve_lsp.go:388-510` — `queryLSPDefs`/`queryLSPRefs` LIKE fallback
- `cmd/leyline_test.go:300-348` — `_lsp_*` table schemas
- `internal/ingest/sqlite_writer.go:74-78` — `node_refs`/`node_defs` schemas
- ADR-0012 — CGO removal migration (orthogonal but related; same producer-side
  delegation arc to ley-line-open)
