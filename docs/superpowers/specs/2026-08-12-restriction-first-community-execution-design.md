# Restriction-First Community Execution

**Bead:** `mache-b37cff`

## Problem

Mache's SQLite graph already stores references as addressable
`node_refs(token, node_id)` rows, but the analysis boundary discards that
structure. `SQLiteGraph.RefsMap` scans the entire table into a Go map, and each
community-based tool expands every selected token into all pairs of referencing
nodes before running Louvain. On the Mache corpus, `get_communities`,
`get_diagram`, and `get_architecture` therefore repeat a global projection that
allocates roughly 8 GiB per call.

Ley-line-open's content-addressed storage, SQLite deserialization, and Cap'n
Proto transport make the substrate cheap to open and address. They cannot make
the later global Go-map materialization or quadratic pair expansion cheap.
Mache must preserve restrictions across the SQL-to-analysis boundary so work
irrelevant to a query never enters the projection.

The first increment proves this premise for explicit projected-graph roots
(package/module/file/construct roots when the active schema exposes them). It
does not claim that independently clustering arbitrary modules is equivalent to
global Louvain clustering. Source-filesystem and import-closure selectors are
later selector kinds because schema projection can reorganize source paths.

## Decision

Add an optional restricted-reference capability beside `RefsMapper`. A
restriction is a set of graph node roots. Its induced reference graph contains
only `node_refs` rows whose `node_id` is exactly a root or is a descendant of a
root on a path-segment boundary. A token shared by selected and unselected
nodes connects only the selected nodes; no unselected boundary node is added
implicitly.

The empty-root case is rejected. Whole-graph analysis remains the existing
explicit `RefsMap` operation, preventing an omitted scope from silently turning
into an expensive global request. Roots are normalized, sorted, deduplicated,
and reduced so a descendant of another selected root is removed.

The public graph types have this shape:

```go
type RefRestriction struct {
    Roots []string
}

type RefSnapshot struct {
    Refs              map[string][]string
    RestrictionDigest string
    ContentDigest     string
    RowsRead          uint64
    Tokens            uint64
    Nodes             uint64
}

type RestrictedRefsMapper interface {
    RefsWithin(context.Context, RefRestriction) (*RefSnapshot, error)
}
```

Names may change during implementation to satisfy existing API conventions,
but the semantics above are the contract. `RestrictionDigest` is BLAKE3 over a
versioned canonical encoding of the normalized roots. It identifies the
restriction, not the current graph contents; a later derived-result cache must
combine it with graph snapshot identity, schema identity, and algorithm
parameters.

`ContentDigest` is BLAKE3 over the normalized selected `(token, node_id)` rows.
Unlike `RestrictionDigest`, it changes when the selected graph facts change and
can participate in an exact derived-result cache key.

## SQLite and Backend Behavior

`SQLiteGraph.RefsWithin` pushes every root predicate into SQL against
`node_refs.node_id`. Prefixes are matched on path boundaries, not with a raw
`root%` expression that would confuse `pkg/a` with `pkg/ab`. The query must use
a node-ID index and must not scan or decode rows outside the restriction.
Ley-line-open v0.18.2 already emits `idx_refs_node`; Mache's schema-projection
`SQLiteWriter` must add the equivalent index to its table contract. Overlapping
roots cannot produce duplicate rows after normalization.

`MemoryStore` implements the same capability by filtering its immutable refs
snapshot. This preserves backend parity, although only SQLite can prove storage
pushdown. Composite graphs apply the restriction after resolving mount prefixes
and return one canonical snapshot; unsupported backends continue to expose
only `RefsMapper`.

The projection layer accepts a `RefSnapshot` and records a second set of work
counters:

- distinct projected nodes;
- distinct projected tokens;
- pair candidates considered before self/duplicate suppression;
- final undirected edges.

These counters describe algorithmic work and are stable enough for regression
tests. They are not timing telemetry and do not alter the community result.

## Tool Integration

`get_communities` and `get_diagram` receive an optional `roots` argument. When
present, the handler requires `RestrictedRefsMapper` and analyzes the induced
subgraph. When absent, current whole-graph behavior and response shapes remain
compatible. Responses include the normalized roots, restriction digest, and
work counters as additive metadata.

`get_architecture` stays whole-repository in this increment because its file
counts, top-level breakdown, API surface, and community layers currently form
one repository-wide report. Giving only its community phase a hidden scope
would make that report internally inconsistent. A separately designed scoped
architecture response can follow once all of its fields share one restriction.

Within a process, identical community requests share one computation using the
selected `RefSnapshot.ContentDigest`, normalized restriction, minimum community
size, and filtering options as the key. A warm request may still scan and hash
its selected rows so an externally updated SQLite database cannot return stale
analysis; the expensive pair expansion and Louvain work are reused. This
removes duplicate execution across `get_communities` and `get_diagram`; it also
supports the unrestricted key used by repository-wide consumers. Durable
cross-process reuse remains a follow-on through ley-line-open's
restriction-addressed daemon cache.

## Correctness and Performance Proof

The proof starts at production boundaries rather than calling the Go walker
directly:

1. Build a database through `mache build --schema go` containing a target Go
   module and a large unrelated module with deliberately high-fanout reference
   tokens.
1. Query the target restriction through the same graph capability and handler
   used in production.
1. Independently build the target module alone and assert identical normalized
   refs, community membership, modularity, and quotient edges.
1. Add or enlarge the unrelated module and assert that the restricted result,
   restriction digest, selected-row count, and projection work counters do not
   change.
1. Assert that the unrestricted result still includes both modules and incurs
   more selected rows and pair candidates.
1. Repeat the semantic suite for SQLite and MemoryStore; assert SQL query-plan
   index use only for SQLite.

CI gates correctness and deterministic work reduction, not wall-clock time.
The Mache-on-Mache harness captures CPU and allocation profiles for restricted
and unrestricted calls against the same already-built database. The report
includes input rows, pair candidates, edges, allocated bytes, peak live bytes,
and wall time. Flamegraphs must show whether remaining time is in SQL scanning,
Go materialization, pair expansion, or Louvain; they are diagnostic evidence,
not the pass/fail oracle.

## Delivery Sequence

1. Restricted graph capability, canonicalization, SQL pushdown, backend parity,
   and deterministic counters.
1. Scoped `get_communities` and `get_diagram` plus production-boundary tests and
   profiles.
1. Per-process singleflight/result reuse for restricted and unrestricted keys.
1. Durable daemon reuse after ley-line-open exposes the versioned
   restriction-addressed cache tracked by `ley-line-open-69305e`.
1. Reference/import-closure selectors and a consistently scoped architecture
   report as separate extensions of the same restriction type.

## Non-Goals

- Changing the parser, grammar registry, CDC chunking, or Cap'n Proto format.
- Treating source-byte chunk addresses as the semantic restriction key.
- Claiming module-local clustering is identical to a global partition.
- Making approximate embedding gates part of correctness.
- Adding implicit boundary expansion; reference/import closure must be an
  explicit, depth-bounded selector.
