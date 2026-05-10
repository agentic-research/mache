# ADR-0016: Cross-Language Reference Resolver

Date: 2026-05-10
Status: Proposed
Relates to:

- ADR-0013 (refs/defs canonical schema, fidelity poset)
- ADR-0014 (mache in the constellation, `current_root` as arena clock)
- ADR-0015 (syntax-aware write protection — validates writes to mounted sub-graphs)
- Bead `mache-q43l` (cross-language reference graph epic; this ADR is its design)
- Beads `mache-bd97d9`, `mache-bdcd2b`, `mache-be0b9f`, `mache-be3da9`, `mache-be650a` (implementation sequence — see § Implementation Strategy)

## Context

Mache today has two related primitives for refs that span beyond a single
parse tree:

- `RegisterAddressRefQuery(langName, scheme, query string)` in
  `internal/ingest/address_refs.go:29` — a per-language registry that lets a
  tree-sitter capture emit refs whose **token is a typed address**, not a
  bare identifier. The Terraform grammar registers `(string_lit) @mod` and
  emits `mod:./modules/vpc` instead of `vpc`.
- `CompositeGraph` in `internal/graph/composite.go:17` — a prefix-mount
  coproduct: multiple `Graph` backends are mounted under disjoint path
  prefixes, with calls dispatched by `resolve(id)` stripping the mount
  prefix and forwarding. `LookupDef` / `GetCallers` / `GetCallees` already
  cross mount boundaries via `DefsMap()` aggregation.

Together these cover an honest slice: a Terraform file emits
`mod:./modules/vpc`; the single-arm resolver in
`cmd/serve_resolve_ref.go:79` (`resolveModScheme`, gated by
`isLocalRelativeLocator` at line 138) maps it to a local path in the same
mounted graph. **What works**: the address layer is real, the
capture-to-token plumbing is in place, the prefix-mount coproduct composes
graphs without rewriting them. **What's missing**: the resolver covers
one scheme, one direction, one effect class (synchronous, local,
fallible-by-`nil`). `npm:` import specifiers, `oci:` `FROM` lines,
`openapi:` `$ref` pointers, `gomod:` paths, `git:` URLs, `http(s):` schema
URIs — all unimplemented, and the single-arm shape does not generalize
because the resolver's *signature* is wrong before its *coverage* is.

The companion ADRs constrain the design:

- **ADR-0013** introduced the refs/defs fidelity poset
  `L₀ mention ⊑ L₁ binding ⊑ L₂ reachability` and the canonical views
  `v_defs` / `v_refs` over multiple producers (`node_*` tables + `_lsp_*`
  tables). The projection `π_{j → i}` is one-way; higher fidelity dominates
  without a merge step. Cross-language refs must extend this — not replace it.
- **ADR-0014** located mache as a structural-observation producer in the
  ART constellation, with `current_root` (the Σ-anchored arena clock) as
  the identity of a mache state and the unit of cache-invalidation across
  processes.

The published comparables each cover a subset:

- **Kythe** (Google): tickets `kythe://corpus?lang=go?path=...#sig` are a
  fixed naming algebra over a pre-indexed corpus. No open scheme registry,
  no fidelity stratification, no resolver layer.
- **SCIP** (Sourcegraph): `Symbol = scheme manager package version descriptor` — a free monoid with separator discipline. Pure identity;
  resolution is left to the consumer.
- **Stack-graphs** (GitHub): partial scope graphs compose categorically
  under pushdown name resolution. The closest published model for
  intra-language refs, but monolithic in fidelity (everything is L₁) and
  with no address-layer notion of "cross-system".
- **Glean** (Meta): typed Datalog over facts; predicates carry the schema.
  No native cross-fidelity story, no resolver-as-arrow framing.

Mache's slice is narrower than Kythe's and wider than SCIP's: an **open
scheme registry, per-scheme resolvers as arrows in an effects monad, and
a fidelity poset extended to language pairs**. This ADR writes that down.

## Decision — Formal Model

### Three orthogonal axes: naming, resolution, fidelity

The naive proposal collapsed three independent axes into a single
`scheme:value` string. Separating them gives independent extension axes
(schemes, effects, fidelity levels) without combinatorial blowup, and
surfaces the wedge cases where the axes interact.

**Naming.** An address is a typed locator.

```go
type Address struct {
    Scheme string   // mod | npm | oci | openapi | git | gomod | http
    Value  string   // canonicalized per-scheme payload
}
```

Each scheme contributes a canonicalization function `canon_s: String → String`
that strips redundancy (URL fragments, default ports, version-spec
whitespace). Two addresses are *naming-equal* iff their schemes match and
their canonicalized values agree. This is a free monoid over a scheme
alphabet, quotiented by per-scheme canonicalization.

**Resolution.** Resolution is the operation that takes an address and
returns a locus in the projected graph — or refuses, or waits, or expires.
The honest type is not `Address → Graph` but a Kleisli arrow into an
effects monad (next subsection).

```go
type Resolver interface {
    Scheme() string
    Resolve(ctx context.Context, addr Address) Result
}
```

**Fidelity.** ADR-0013's poset already orders fidelity intra-language:
`L₀ ⊑ L₁ ⊑ L₂`. Cross-language refs carry fidelity at *both* endpoints, so
the right structure is a product poset (see §Decision — Fidelity extension).

Adding `oci:` is a naming change. Adding capability gating is a
resolution change. Adding semantic-link refinement is a fidelity change.
The current single-arm resolver conflates all three: the registry knows
the scheme, but the resolver returns a path and the resulting edge
carries no fidelity tag.

### Resolution as a Kleisli arrow

A pure `Resolve : Address → Maybe Graph` cannot express:

- a fetch in progress (`npm:react@^18.2` is queued; ETA 1.4s);
- a capability denial (`http://internal.example/...` requires a vault
  slice this caller does not hold);
- a staleness window (the resolution was computed at Σ-root `σ_0` but the
  current arena clock is `σ_1`; the answer is *valid as of `σ_0`*).

The honest codomain is `T(GraphLocus)` for an effects monad

```
T = Maybe ∘ Fetching ∘ Forbidden ∘ StaleAt(σ)
```

with constructors

```
Now(locus)            -- resolved synchronously at the current arena clock
Fetching(eta, job)    -- pending; consumer may poll or subscribe
Forbidden(reason)     -- capability missing
StaleAt(σ, locus)     -- resolved, but as of an earlier arena clock
Missing               -- not resolvable in principle
```

Resolvers are then Kleisli arrows `Address → T(GraphLocus)`. Composition
of resolvers (e.g. `gomod:` resolves to `git:` resolves to a local
checkout) is Kleisli composition: effects accumulate down the chain;
the first non-`Now` short-circuits.

**Worked example.** Terraform emits `mod:./modules/vpc`; the `mod`
resolver sees a local-relative locator and returns
`Now(graphLocus(mountPrefix + "/modules/vpc"))`. Effect-trivial, single
arrow, single fiber. Same module emits
`mod:github.com/org/vpc-tf?ref=v1.2.0`; the chain composes `mod → git`
and returns `Fetching(eta=2s, job=fetch-job-7f3a)`. The MCP tool wraps
that into a result that says "pending, retry after". The graph never
lied about the locus existing — it told the truth that the locus does
not exist yet and stated when it will. Without the monad this honest
conversation cannot happen; the pure signature forces a choice between
blocking, returning `nil`, or fabricating a placeholder.

### Cross-language refs as a Grothendieck fibration

With naming and resolution separated, the right categorical model is a
**Grothendieck fibration** `p: GraphMounts → Σ` over the scheme category.

- **Σ** (the base): objects are schemes (`mod`, `npm`, `oci`, `openapi`,
  `git`, `gomod`, `http`). Morphisms are scheme-pair coercions witnessed
  by resolvers — for example, `mod → git` is a morphism in Σ witnessed by
  the resolver chain that takes a Terraform module address to its git
  checkout. Composition is resolver chaining.

- **GraphMounts** (the total category): the fiber `p⁻¹(s)` over a scheme
  `s` is the projected subgraph mounted under that scheme — exactly the
  per-prefix backend in today's `CompositeGraph`. Morphisms over a Σ
  morphism `f: s → t` are the cartesian liftings — resolver-witnessed
  injections of the source subgraph into the target subgraph.

Why this level of abstraction:

1. **Composition works.** A `mod:` ref resolving through `git:` to `file:`
   is a chain of cartesian morphisms in `GraphMounts`. The prefix-mount
   coproduct in `CompositeGraph` is the *colimit* of the fibers — correct
   as far as it goes, but it does not see the morphisms between fibers.
   The fibration does.
1. **Per-scheme local reasoning.** Each resolver is developed, tested,
   and authorized independently; the fiber over `npm:` does not need to
   know about `oci:`.
1. **Cross-scheme refs are first-class** — they are exactly the
   non-trivial cartesian morphisms.

**It is not a sheaf.** A full sheaf on Σ would require gluing data on
overlaps with strict equality on intersections. Here intersections are
arenas where two fibers project the same piece of the world (an `npm:`
package's `package.json` and an `oci:` image's build manifest both
referencing the same git tag). At any instant the two projections may
disagree, be in flight, be denied, or be stale at different Σ-roots. The
sheaf axiom fails.

What we have is a **separated presheaf on a coverage**: any two sections
agree on overlaps when they both have *resolved* answers, but gluing into
a single section requires arena-clock advance. ADR-0014's `current_root`
is exactly that clock. Advancing `current_root` over a window during
which every in-flight `Fetching` settles is the **sheafification step**.
Mache without arena-clock advance gives correct local answers and correct
overlap agreement; mache with arena-clock advance gives a coherent global
cut. The `current_root` hot-swap is not a workaround — it is the
sheafification step in the only categorical model where the design
type-checks.

## Decision — Fidelity extension

ADR-0013's poset orders fidelity at a single endpoint. A cross-language
ref has *two* endpoints with independent fidelities: a Terraform mention
of a Go binary may be `L₀` on the Terraform side (string literal) and
`L₁` on the Go side (LSP resolved the exported symbol).

The natural extension is the **product poset**

```
(L_src × L_tgt, ≤_componentwise)
```

with `(i, j) ≤ (k, l)` iff `i ≤ k` and `j ≤ l`. The canonical view at
fidelity `(i, j)` is

```
V_{(i, j)} = ⋃_{(k, l) ≥ (i, j)} π_{(k, l) → (i, j)}(R_{(k, l)})
```

— ADR-0013's pattern, indexed on a product instead of a chain. Producers
that emit higher-fidelity rows project down; no merge step.

**L₂ saturates at L₁ across language boundaries.** ADR-0013's L₂
(reachability) is intra-language: a Go SSA producer answers "this binding
is executed". Cross-language reachability — "this Terraform `apply`
reaches this Go function via the binary it invokes" — requires lifting
reachability to the runtime category (data plane, not code plane). This
ADR declines that lift and defines:

> For any cross-language edge `(s, t)` with `s ≠ t`,
> `L_s = min(L_s, L₁)` and `L_t = min(L_t, L₁)` in the canonical view.

The highest fidelity promised across a language boundary is **binding**,
not reachability. Cross-language reachability will require an explicit
cross-language SSA layer or a runtime-trace producer; until then the
product poset truncates.

**Schema impact.** The `v_defs` / `v_refs` views generalize mechanically.
Each gains a `target_lang` column and splits the single `fidelity` into
`(src_fidelity, tgt_fidelity)`. Producers that only know one endpoint
(an unresolved `mod:` ref where target language is unknown until
resolution lands) fill `target_lang = NULL`, `tgt_fidelity = NULL`.
Existing rules keep working under a view alias
`fidelity := min(src_fidelity, tgt_fidelity)`.

## Decision — Pitch and scope honesty

Two pitch lines worth retiring or trimming:

- **"We project, we don't index."** Categorically real: indexers build a
  derived algebra over the source functor; mache builds a coalgebra (the
  projection unfolds the source). At the *runtime cost* layer it is a
  wash — index build, cache warm, and query path costs are equivalent.
  The ergonomic difference (one surface) is the real win; the cost
  argument is not. Drop the slogan from the public pitch; keep the
  algebra/coalgebra observation as a technical remark.

- **"Monorepo = polyrepo with different URI schemes."** True at the
  **naming layer**: `file:./local` and `git://host/repo@<sha>` both refine
  to content-addressed identities (per ADR-0014's Σ root). False at the
  **resolution layer**: polyrepo refs resolve against a product of arena
  clocks, and `Resolve` lives in the effects monad whether or not the
  source is in the same filesystem. The equivalence is narrow; pitching
  it unqualified obscures the eventual-consistency engineering the
  fibration model exists to make honest.

**The pitch that survives.** Mache's cross-language refs are
**graph-integrated, address-level cross-system references that share one
MCP/NFS surface with everything else mache projects**. Polyglot at the
*address* layer, not the *type* layer. The math claim: the fibration
`p: GraphMounts → Σ` plus the product fidelity poset gives a correct
composition algebra the prefix-mount coproduct alone does not. The
ergonomic claim: `find_callers` / `resolve_ref` / `get_definition` work
over the federated cross-scheme graph without consumer branching.
Neither claim depends on cost savings versus indexing.

## Consequences

### Positive

- **Correct composition of subgraphs.** `CompositeGraph`'s prefix-mount
  coproduct gives the colimit of fibers; the fibration adds the
  morphisms *between* fibers, which is what cross-language refs actually
  are. Consumers gain `find_callers("npm:react")` returning Go files that
  import a generated TS binding — not by accident of prefix overlap, but
  as a cartesian morphism along `npm → gomod` witnessed by a
  generator-output resolver.
- **Per-scheme local reasoning.** Each resolver is an independent
  Kleisli arrow; adding a scheme is a registration. The
  `RegisterAddressRefQuery` registry extends naturally to a resolver
  registry with the effects monad as the new contract.
- **Correct behavior under partial information.** `Fetching`, `StaleAt`,
  and `Now` resolvers all live in the same monad; the canonical view at
  any fidelity unions projections from higher fidelities. Rules that
  need fresh answers gate on `tgt_fidelity ≥ L₁`; rules that tolerate
  staleness do not.
- **Arena-clock semantics inherited.** Sheafification is the existing
  `current_root` advance from ADR-0014, not a new mechanism. Cross-process
  cache coherence for cross-language refs falls out of the constellation.

### Negative

- **Effects-monad complexity surfaces in consumers.** A tool handler that
  did `if locus == nil { return error }` now has four cases (`Now`,
  `Fetching`, `Forbidden`, `StaleAt`) plus `Missing`. The MCP result
  shape must carry enough metadata for callers to retry, escalate, or
  accept staleness. Honest typing of a problem the single-arm resolver
  was hiding, but real cost on every consumer.
- **No cost win versus indexing.** Dropping the "we project" slogan means
  the marketing argument leans entirely on unification, not on speed.
- **L₂ truncation is a real ceiling.** Cross-language reachability needs
  a cross-language SSA layer or a runtime-trace producer. Until one
  ships, rules wanting cross-language reachability
  (`untested_function` across a Go/TS RPC boundary, say) return the L₁
  binding view and may overcount.
- **Schema break on `v_defs` / `v_refs`.** Splitting `fidelity` and adding
  `target_lang` touches every consumer; the compatibility alias keeps
  existing rules working but the columns are now public API.

### Reversibility

Naming is additive — schemes can be added without consumer rewrites.
Resolution is the commitment point — once a resolver returns `Fetching`,
every consumer must handle it. Fidelity is reversible until the first
cross-language producer writes rows with `src_fidelity ≠ tgt_fidelity`;
after that, collapsing back to a chain loses information.

## Implementation Strategy

The 5-bead sequence below ships the formal model in working code, with each step independently testable and the first three on the critical path. Bead IDs are clickable in the rosary tracker; details (depends-on, file scope, test scope) live in those bead descriptions.

**Critical path (serial):**

1. **`mache-bd97d9` — `internal/resolve/` package: `Resolver` interface + `Registry`.** Pure Go, no CGO. Defines `Resolver`, `Registry`, sentinel errors `ErrNotResolvable` / `ErrSchemeUnknown`. V1 codomain is `(graph.Graph, error)` — the full effects-monad surface (`Now` / `Fetching` / `Forbidden` / `StaleAt`) lands in a later bead once a consumer needs more than `Now` / `Missing`. The interface is shaped so the additional cases are an additive extension, not a breaking change.

1. **`mache-bdcd2b` — `LocalPathResolver` (the `mod:` local-relative case).** Migrates `isLocalRelativeLocator` from `cmd/serve_resolve_ref.go:138` into the resolver package. Uses `ingest.Engine` with auto-schema to ingest the resolved directory; caches by absolute path; coalesces concurrent resolution via `singleflight.Group`. Returns `Now(subGraph)` semantically — `(graph.Graph, error)` mechanically.

1. **`mache-be0b9f` — wire `resolve_ref` to mount the sub-graph in `CompositeGraph`.** This is the Terraform-`mod:`-end-to-end milestone. Today's `makeResolveRefHandler` ignores its `graph.Graph` parameter; after this bead, successful resolutions `composite.Mount("resolve/<sha-prefix>", subGraph)` and the response gains a `graph_path` field that other MCP tools (`find_callers`, `list_directory`, `find_definition`) can immediately query. Federation across the mount boundary is automatic via `CompositeGraph.DefsMap()` — no changes to consumer handlers.

**Parallelizable after step 1:**

4. **`mache-be650a` — second scheme to prove generalization.** Either `NPMResolver` (read `package.json`, resolve against `node_modules`) or a `GitResolver` stub (return `ErrNotResolvable` with a structured `RemoteHint` payload — no network I/O at test time). Validates that adding a scheme is two lines of wiring: one `RegisterAddressRefQuery` call, one `Registry.Register` call. If this bead requires touching the core graph, MCP plumbing, or the resolver interface, the abstraction is leaking and we revisit before scheme #3.

**Parallelizable after step 3:**

5. **`mache-be3da9` — `callers/` virtual-dir entries for scheme-typed ref tokens.** Extends `internal/nfsmount/graphfs.go` so that `callers/` entries on constructs that emit `mod:` (or other resolvable-scheme) tokens also expose a symlink into the resolved sub-graph's mount prefix when one exists. Parallels the existing `callees/` symlink pattern; higher blast radius than the MCP-only changes in bead 3, hence sequenced after.

**Out of scope for this ADR's first cut:**

- The full effects monad in the resolver signature. V1 uses `(graph.Graph, error)`; the surface is widened only when `npm:` / `oci:` / `git:` resolvers need `Fetching` / `StaleAt`. Splitting this avoids a churny refactor before the use case is in tree.
- Cross-arena clock coordination (sheafification at the constellation level). Single-arena mache + ley-line-open is the supported configuration today (per ADR-0014); cross-arena consistency is a coordinator concern, not this ADR's problem.
- L₂ (reachability) across language boundaries. The product poset truncates per the rule in § Decision — Fidelity extension; lifting requires a runtime-trace producer that does not yet exist in this codebase.
- Cycle handling across cross-scheme chains (A `mod:` → B `git:` → C `mod:` → A). V1 ships single-hop resolution only; transitive resolution is a later bead with explicit budget and depth guards.

## References

- `internal/ingest/address_refs.go:29` — `RegisterAddressRefQuery`
- `internal/graph/composite.go:17` — `CompositeGraph` prefix-mount coproduct
- `cmd/serve_resolve_ref.go:79` — `resolveModScheme`, current one-arm
  resolver
- ADR-0013 — refs/defs canonical schema, fidelity poset, `v_defs` /
  `v_refs`
- ADR-0014 — mache in the constellation, `current_root` as arena clock
- Stack-graphs (GitHub) — published categorical model for intra-language
  scope composition; closest comparable
- Kythe (Google), SCIP (Sourcegraph), Glean (Meta) — point-of-comparison
  systems, each addressing a subset of the naming / resolution / fidelity
  axes
- Math-friend analysis transcript, 2026-05-10 (this session) — source of
  the fibration / product-poset / Kleisli-arrow framing
- Paradigm-assessor red-team transcript, 2026-05-10 (this session) — source
  of the scope-honesty corrections in § Decision — Pitch and scope honesty
