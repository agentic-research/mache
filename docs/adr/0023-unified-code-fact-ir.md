# ADR-0023: Unified Code-Fact IR — Property Graph over a Content-Addressed Symbol Set

Date: 2026-07-05
Status: Proposed
Relates to:

- ADR-0013 (refs/defs canonical schema, fidelity poset — the pattern this generalizes)
- ADR-0014 (mache in the constellation, `current_root` as arena clock; capnp typed records over SQL-string protocols)
- ADR-0015 (cache identity must live on the substrate, not the projection)
- ADR-0016 (cross-language reference resolver — "separated presheaf on a coverage, not a sheaf"; this ADR is the fact substrate beneath it)
- Bead `mache-472f60` (this ADR is its design)
- Bead `mache-cff68e` (smell-rule shape as capnp IDL — shares the fact schema's IDL)
- Bead `mache-37ae8b` (CGO tree-sitter removal — this makes the selector DSL unnecessary)
- Bead `mache-d5e158` (consume LLO passes — leyline is the producer of these tables)

## Context

Mache's "code facts" — the AST, LSP enrichment, cross-references, bindings,
communities — live in a set of tables that grew independently and do not share
a join contract:

- `_ast`, `_source`, `_imports`, `_lsp*` (defs/refs/hover/diagnostics), produced
  by ley-line-open's `leyline parse`.
- `node_defs`, `node_refs`, `nodes`, `file_index`, produced by both the leyline
  path and mache's standalone `SitterWalker`.
- `${db}.bindings.capnp`, a typed binding-event log (post-T8.2, mache-6bd4d8).
- `v_refs` / `v_defs` — TEMP per-connection views created by
  `cmd/serve_find_smells.go::ensureCanonicalViews` that UNION the mention and
  binding arms with a `fidelity` discriminator column. Every `find_smells` rule
  queries these views, not the raw tables.

Three problems follow from this shape.

**1. Joins can miss silently (the `be6136` incident).** The refs/LSP tables
joined on `node_id` / `_source.path` — mutable, path-shaped strings that carry
no integrity constraint. A one-byte canonicalization mismatch in `_source.path`
made every JOIN in the `_lsp_refs` arm miss, silently degrading binding-fidelity
rows to zero. Nothing errored; the regression was invisible until a consumer's
output was falsified. ADR-0014 diagnosed the root cause ("the SQL schema was
acting as a cross-process protocol with no compile-time type check across the
producer/consumer boundary, no canonicalization contract") and the fix at the
time was to move binding data off SQL JOINs and onto typed capnp records. That
fixed the binding arm; the underlying *join-key fragility* remains wherever
facts are related by path-shaped strings.

**2. No cross-language normalization.** A "function" is `function_declaration`
in Go, `function_item` in Rust, `function_definition` in Python. Every consumer
that wants "all functions" either special-cases per language or queries raw
`node_kind` strings. There is no language-agnostic fact shape to query once.

**3. Runtime views recompute per query.** `v_refs`/`v_defs` are logical TEMP
views — their UNION-of-arms SQL re-runs on every `find_smells` connection.
That is the perf tax of a view: correct, but recomputed.

The larger gap is that there is no single **intermediary representation** over
AST + LSP + graph info that is both *joinable* (relate facts across types
robustly) and *smartly traversable* (callers/callees, containment, transitive
closure) — the substrate a normalized, cross-language, DSL-free query surface
would sit on.

**Key observation: ~70% of this IR already exists in miniature.** The
`v_refs`/`v_defs` fidelity-discriminator UNION *is* the normalization pattern.
This ADR does not introduce a new formalism; it (a) fixes the join key so it
cannot silently miss, (b) generalizes the three ad-hoc union arms into one
typed edge table, and (c) materializes it instead of recomputing a view.

## Decision

Adopt a **property graph encoded relationally** as the unified code-fact IR:
typed node tables plus one generic typed-edge table, keyed on a
content-addressed symbol identity, materialized by leyline at parse time, and
queried with plain SQL (no query DSL).

### 1. Structure — property graph as relational tables

A property graph `G = (V, E, λ)` with edge labels `λ: E → Σ_edge`, encoded as
two relations: nodes are rows keyed by `symbol_id`; edges are rows in **one**
`fact_edges` table. A join is then a SQL join, and a traversal is a
`WITH RECURSIVE` over `fact_edges` — the least-fixed-point of the
union-of-joins functional, which is exactly transitive closure. This is the
unique shape that serves both operations natively on SQLite:

- Pure-relational (one table per relation) makes heterogeneous traversal a
  hand-written union of per-relation recursive CTEs — awkward, rejected as the
  top-level shape.
- Datalog (Glean-style) traverses beautifully but *is* a query DSL (embedded
  engine or transpiler) — rejected as the substrate, borrowed as schema
  discipline (§6).
- Property-graph-as-relational-tables gives the graph logical model with the
  relational physical model and no DSL. Adopted.

### 2. Identity — dual key (the load-bearing decision, the `be6136` cure)

- **`symbol_id` — the join key. Content-addressed, parse-run-invariant.**
  `symbol_id = BLAKE3(source.contentHash ‖ canonical_span ‖ kind ‖ name)`,
  where `canonical_span` is the byte range normalized against the content hash,
  not the path. Every input already exists: `_source.contentHash` (BLAKE3, in
  `source.capnp`), `range` (`common.capnp`), `node_kind`, name. Two properties
  fall out: the fragile path **never enters the key**, so `be6136` cannot recur
  by construction; and unchanged files yield identical `symbol_id`s across
  parse runs, making the IR diffable and generation-keyed materialization cheap.
- **`node_id` — the locator. Kept, demoted.** It remains the human/tool-facing
  path address and the parse-run-local pointer into `_ast`. It is **never a
  cross-fact join key**. Resolution `node_id → symbol_id` happens once, at parse
  time, in leyline, where AST and source bytes are both in hand.

**Fail-loud, to remove the silent-miss *class*, not just its cause:**

- **Referential integrity as a materialized invariant.** `fact_edges.src` and
  `.dst` are `REFERENCES symbols(symbol_id)`; with `PRAGMA foreign_keys=ON` at
  write time, a dangling reference is an *insert error* in the producer, not a
  zeroed row in the consumer.
- **`unbound_facts` counter in `head.capnp`.** When leyline legitimately cannot
  resolve a ref (cross-repo target, file deleted mid-pass), it emits the edge
  with `dst = NULL` and increments a counter hashed into the Head. "N facts
  failed to bind this generation" becomes a first-class, monotonic number the
  W5 ratchet gate can assert on (`unbound_facts ≤ baseline`). `be6136` would
  have shown as this counter jumping to ~100% — a red gate on the first CI run.

**Caveat (state explicitly):** content-addressed identity *changes when bytes
change*. That is correct for a per-generation snapshot IR, but "the same
function across edits" is therefore a **separate** `lineage` edge produced by a
diff pass, not `symbol_id` equality. Do not overload `symbol_id` to mean
identity-over-time — that would re-introduce a fuzzy-match join.

### 3. The normalized fact schema

All tables materialized by leyline, keyed on `symbol_id` + `gen`.

```sql
CREATE TABLE symbols (
  symbol_id   BLOB    NOT NULL,   -- BLAKE3(contentHash ‖ span ‖ kind ‖ name)
  gen         INTEGER NOT NULL,   -- parse generation (head.capnp clock)
  source_id   TEXT    NOT NULL,   -- repo-relative path
  node_id     TEXT    NOT NULL,   -- parse-run locator (NOT a join key)
  kind        TEXT    NOT NULL,   -- CANONICAL, cross-language (κ below)
  raw_kind    TEXT    NOT NULL,   -- tree-sitter kind (function_declaration/…)
  lang        TEXT    NOT NULL,
  name        TEXT,
  span_start  INTEGER NOT NULL,   -- canonical byte offsets
  span_end    INTEGER NOT NULL,
  PRIMARY KEY (symbol_id, gen)
);

CREATE TABLE fact_edges (
  src        BLOB    NOT NULL REFERENCES symbols(symbol_id),  -- FK = fail-loud
  dst        BLOB             REFERENCES symbols(symbol_id),  -- NULL = unbound (counted)
  kind       TEXT    NOT NULL,   -- contains|calls|references|defines|binds|imports|has_type|lineage
  fidelity   TEXT    NOT NULL,   -- mention|binding|reachability
  gen        INTEGER NOT NULL,
  token      TEXT,               -- lemma at the ref site (mention arm)
  qualifier  TEXT,               -- T8.7 selector LHS
  span_start INTEGER, span_end INTEGER
);

CREATE TABLE symbol_attrs (       -- narrow EAV, QUARANTINED to open LSP data
  symbol_id BLOB NOT NULL, gen INTEGER NOT NULL,
  key TEXT NOT NULL, value TEXT
);
```

**Cross-language collapse `κ: (lang, raw_kind) → kind`** lives in leyline's
language registry (mirroring mache's `internal/lang` single source of truth).
Base kinds (closed set): `function`, `method`, `type`, `field`, `variable`,
`constant`, `module`/`file`, `import`, `parameter`. Anything unmapped keeps
`kind = raw_kind` (open-world escape hatch). `raw_kind` is retained so
language-specific rules can still discriminate. This promotes the friendly-name
grouping mache already does in `ProjectAST` (`function_declaration → functions/`)
from a directory-naming convenience to a first-class typed column.

**Every source maps in:** `_ast` node → a `symbols` row + `contains` edge to
its parent; `node_defs` → `symbols` row + `defines` edge (`fidelity='mention'`);
`node_refs` → `references`/`calls` edge (`fidelity='mention'`, `token` set);
`bindings.capnp` → `binds` edge (`fidelity='binding'`, `qualifier` set,
`dst = resolved symbol_id`); `_lsp_hover`/`_lsp` → `symbol_attrs` rows (not
edges); communities → a *derived* `symbol_community` table (downstream, §5), not
base IR; `get_impact` → a *query* over closure, not stored.

`fact_edges` **is `v_refs` generalized**: `v_refs ≡ SELECT … FROM fact_edges WHERE kind IN ('calls','references','binds')`. The `fidelity` discriminator
already in the view design lifts from a runtime UNION to a materialized column —
one table, one index, no per-connection recompute.

`symbol_attrs` is EAV **only** for genuinely open-ended LSP metadata (hover text,
diagnostics). Everything with fixed arity stays a typed column. Resisting EAV
creep is the one discipline that keeps this from rotting into a schemaless
triple store.

### 4. Traversal — hybrid

- **Mechanism A: recursive CTE (ad-hoc, unbounded).** Callers/callees/def→refs
  at arbitrary depth over `fact_edges`, indexed on `(src, kind, gen)` so each
  hop is an index seek; depth-bounded (leyline's existing N-hop op caps at 4).
  The default for the general case.
- **Mechanism B: materialized closure (hot, cheap).** Materialize
  `containment_closure(ancestor, descendant, gen)` — the AST containment
  relation is a **forest**, so its closure is `O(nodes × depth)`, small and fast
  to rebuild per changed file; it powers `constructNodeId` / "all symbols under
  a file" as one indexed lookup. `call_reach(src, dst, depth, gen)` is
  **optional, depth-bounded, measure-gated** — the call graph can be dense
  (`O(V²)`), so default to Mechanism A and promote only if `get_impact`
  profiling (`task profile-tools-pprof`) shows the recursive CTE hot.

Every query is `gen`-scoped (`head.capnp.generation` is the clock) — the
SQL-materialized analogue of ADR-0014's `current_root`, giving incrementality
for free: a new parse writes gen N+1 rows for changed files, queries always run
against one consistent snapshot, old gens are GC-able.

### 5. Sheaf — declined for the IR (kept in the invalidation layer)

The sheaf formalism is real in `sheaf.go`/`quotient.go`, where the space is the
community topology, stalks are region vectors, restriction maps are the δ⁰
agreement-subspace projections, and gluing consistency answers *cache
invalidation* ("did a change cross a boundary"). It stays there.

It does **not** structure the IR. The IR's join-consistency is **referential
integrity on a content-addressed key** — an edge's `dst` references an existing
`symbol_id` or it does not (FK error). There is no overlap on which two sections
must agree up to restriction; calling FK-satisfaction "the gluing axiom" adds a
word, not a constraint. The fidelity poset (`mention ⊑ binding ⊑ reachability`)
is a poset-indexed family of relations with *lossy* downward projections — a
**presheaf** at most, never glued (higher fidelity dominates; no reconstruction
from local agreement). This is consistent with ADR-0016, which reached the same
verdict one level up ("a separated presheaf on a coverage, not a sheaf"). The
ADR states the guarantee honestly as *referential integrity + a fidelity poset*
and notes the presheaf reading as a one-line technical remark, pointing at
`sheaf.go` as where the actual sheaf lives.

### 6. Prior art

The synthesis is **Glean's typed content-addressed facts + CodeQL's
relational-closure query model + LSIF/SCIP's edge taxonomy, expressed as plain
SQLite tables with no query DSL.** Each borrowed piece fits a materialized
SQLite substrate; each skipped piece is a bespoke engine or DSL.

- **Glean (Meta):** typed, content-hashed, immutable facts — exactly the
  `be6136` fix. Borrow the identity discipline; skip the Angle engine.
- **CodeQL:** relational algebra + recursion proves relational-base + closure
  covers deep code queries. Confirms the core bet; its `*`/`*+` = our recursive
  CTEs.
- **SCIP / LSIF:** stable symbol identity as a first-class value; LSIF is a
  serialized graph of typed edges ≈ `fact_edges`. Its edge taxonomy
  (`definition`/`references`/`hover`/`moniker`) maps ~1:1 to our edge kinds;
  SCIP monikers = a naming layer above `symbol_id` (ties to ADR-0016 schemes).
- **Stack Graphs:** per-file incremental composition — maps to per-`source_id`,
  per-`gen` materialization.
- **Property graphs (Neo4j):** the logical model we adopt; skip Cypher.
- **RDF / triples:** a cautionary bound — a universal untyped triple store is
  EAV for the whole graph. `fact_edges.kind` stays a **closed enum** with FK'd
  endpoints; `symbol_attrs` is the only place open data is allowed.

## Consequences

**Positive:**

- `be6136`'s failure *class* is closed by construction (content-addressed key)
  and made loud (FK integrity + `unbound_facts` gate) rather than silent.
- One cross-language, DSL-free query surface: consumers query `symbols` +
  `fact_edges` in plain SQL; smell rules (already SQL) and `mache-cff68e`'s rule
  format sit directly on it; `mache-37ae8b`'s selector DSL becomes unnecessary,
  so the CGO deletion loses its hardest dependency.
- Materialized, not recomputed — the `v_refs` per-connection view cost is paid
  once at parse time.
- Incremental + diffable via content-addressed identity + `gen` clock.

**Costs (name them):**

- **Storage roughly doubles** (normalized form stored alongside raw
  `_ast`/`node_refs`). This is the "materialized not views" trade — bytes for
  latency.
- **Producer complexity moves to leyline:** it must compute `symbol_id`, run
  `κ`, resolve refs or count them unbound, and enforce FK integrity at write
  time. Consumers become trivial indexed SQL. This is the right direction — one
  hard producer, many easy consumers — and the same trade ADR-0014 made with
  capnp.
- **Cross-repo commitment:** the producer is ley-line-open. This ADR needs a
  ley-line-open ADR mirror (the 0013/0014 pattern) before mache builds a reader.
  Nothing downstream works until the tables exist.
- **Cross-gen tracking is explicit** (`lineage` edge), not free.

## Implementation strategy

1. **ley-line-open ADR mirror + producer** (critical path, cross-repo): add
   `symbols` + `fact_edges` + `κ` + `symbol_id` + `unbound_facts` in
   `head.capnp`. Content-addressed key + FK integrity land here.
1. **mache reader over the new tables:** re-express `v_refs`/`v_defs` as
   `SELECT … FROM fact_edges WHERE …` (materialized, not TEMP view). **Prove
   byte-parity** with current smell-rule output — same discipline as the T8.8
   capnp migration; the strongest possible de-risk for a substrate change.
1. **`containment_closure` materialization;** wire `constructNodeId` /
   `get_impact` / containment queries to it.
1. **Ratchet gate on `unbound_facts`** (fits W5).
1. **(Deferred, measure-gated):** `call_reach` materialization; `symbol_lineage`
   diff pass.

Steps 1–2 are the whole thesis in working code; 3–5 are optimization and
hardening. Step 2 is byte-parity-provable against today's behavior.

## Open questions

- **`symbol_id` producer confirmation.** Recommended: leyline (only place with
  source + AST + LSP together). Confirm before mache builds a reader.
- **`call_reach` — decide the *criterion*, not the answer, here.** Materialize
  only if `get_impact` flamegraphs show the recursive CTE hot at real scale.
- **Generation GC policy.** Diffing/lineage wants ≥2 gens retained; storage
  wants 1. Suggest current + previous, GC older, config knob.
- **`κ` closed-set boundary.** The 9 base kinds cover the current registry; new
  languages may surface kinds that do not map cleanly (Rust `impl`, Python
  decorators, TS namespaces). The open-world escape prevents breakage but
  fragments cross-language queries — a per-language review surface, same as
  `internal/lang`.

_Design analysis by the theoretical-foundations-analyst (2026-07-05); full log
at `_agent_log/theoretical-foundations-analyst_2026-07-05_agent_log.md`._
