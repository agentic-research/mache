---
title: 'ADR-0014: Mache as observation producer in the constellation'
status: Proposed
date: 2026-05-09
tags: [architecture, constellation, content-addressing, sheaf, cache, observation-lattice, git]
relates_to:
  - rosary ADR-0009 (cross-repo linkage)
  - rosary ADR-0010 (observation lattice)
  - cloister ADR-0003 (content-addressed bead store)
  - cloister ADR-0004 (capnp manifest)
  - cloister ADR-0010 (vault + bundle clusters)
  - cloister ADR-0011 (hypervisor-bundle boundary)
  - ley-line-open ADR-0014 (capnp as protocol)
  - mache ADR-0007 (git object graph as fs projection)
  - mache ADR-0010 (hosted mache architecture)
  - mache ADR-0011 (pointer abstraction)
  - mache ADR-0013 (refs/defs canonical schema)
---

## Context

A design conversation surfaced 2026-05-09 (post-PR #362, the
DefsMap/RefsMap memoization derived from the e2e tool harness):

> The sheaf cache as proposed (mache-b37cff) is the per-process
> reuse of computed graph products. The actually-needed thing is
> a **repo-level cache, durable across processes, invalidated by
> diffing two states of the input**. Plus a way for that cache to
> live remotely (the leyline daemon's auto-download pattern is
> a partial sketch). Plus git ingestion as the natural state-
> transition channel. These feel like pieces of one design.

The first instinct was to file an ADR-0015 inventing the whole
framework. Reading the sibling repos showed that framework
**already exists**, distributed across rosary, cloister, and
ley-line-open ADRs. Mache hasn't been positioned within it.

This ADR locates mache in the constellation, names the existing
ADRs that constrain the design, and documents the mache-specific
contracts (state identity, diff granularity, ingestion channel)
that the existing beads can be implemented against without
re-litigating the framing.

## The constellation, summarized

| Sibling ADR                                        | What it locks in                                                                                                                  | Mache's relationship                                                                                                                                                                                                                                                                                                                                      |
| -------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **rosary ADR-0010** Observation Lattice            | Append-only set of authenticated observations + per-field deterministic fold. Webhook-first, idempotent under replay/reorder      | Mache reading source code IS observation in this algebra. `(repo_state, source_file) → ast/refs/defs` is a per-field fold over file-level observations                                                                                                                                                                                                    |
| **rosary ADR-0009** Cross-Repo Linkage             | Stratified acyclicity (per-repo DAG, cross-repo cycles allowed). `MultiRepoGraph` (mache-iegm) federates per-repo `.db` artifacts | Mache `.db`s are the federation primitive. Cross-repo edges land at this layer, not inside individual mache instances                                                                                                                                                                                                                                     |
| **cloister ADR-0003** Content-Addressed Bead Store | Two-layer abstraction: immutable CAS DAG + mutable refs. Substrate-portable (workerd / native / KV)                               | Mache's **capnp event log set** is content-addressable per LLO ADR-0014's Σ root and fits cloister's CAS layer directly. The `.db` projection is a derivative — it can also be cached but its identity is `(Σ_root × schema × parser_version)`, not Σ_root alone, because two `.db`s built from the same logs on different machines aren't byte-identical |
| **cloister ADR-0004** Capnp Manifest               | Declarative routes + backends, capnp because workerd parses it. Constellation-wide composition mechanism                          | The path for serving mache MCP via cloister-routed bundles. Mache contributes a manifest fragment, not a runtime registration                                                                                                                                                                                                                             |
| **cloister ADR-0010** Vault + Bundle Clusters      | Cloister is a v8 hypervisor; bundles are units of trust; capability-scoped via `VaultSliceGrant`                                  | When mache runs as a bundle, its credentials (LLO daemon access, R2 keys, signet identity) come from a vault slice, not env vars                                                                                                                                                                                                                          |
| **cloister ADR-0011** Hypervisor-Bundle Boundary   | Hypervisor owns trust + routing + capability mediation; bundles are workloads. External services reached via `httpForward`        | Mache running externally (today's typical deployment) is reached via `httpForward`; mache running as a cloister bundle gets vault-scoped capabilities                                                                                                                                                                                                     |
| **LLO ADR-0014** Capnp as Protocol                 | Typed cross-runtime contract via capnp. Canonical encoding for byte-stability                                                     | Already shipping. Mache reads `.bindings.capnp` per T8.8 (mache-6bd4d8). Producer-side stability gives mache stable input identity                                                                                                                                                                                                                        |

The framework is already there. Mache is the structural-observation
producer that hasn't been written into the picture.

## Decision

### Mache's role in the constellation

Mache is the **structural-observation consumer/projector** for source code:

```
source bytes
  → tree-sitter / leyline parse
  → capnp event logs (.bindings.capnp, future .ast.capnp,
                      .source.capnp — the canonical wire format
                      per LLO ADR-0014, Σ-anchored)
  → mache projects into queryable form (.db / MemoryStore / NFS)
```

**Critical correction surfaced in Copilot review (2026-05-09):**
the canonical artifact in the constellation is the **capnp event
log set**, not the `.db`. LLO ADR-0014 §1: *"SQLite tables are
local projections, not the contract. Σ root advances are computed
over the producer's emitted segments — the bytes-on-wire are the
substrate."* An earlier draft of this ADR conflated the two. The
correction below takes the LLO ADR's framing as load-bearing: the
`.db` is one possible projection of the canonical capnp record
set; mache's contract with the constellation is the **upstream
event log chain**, not its local SQL projection.

Three properties make this fit:

1. **Content-addressable identity at the event-log layer.** Per
   LLO ADR-0014's canonical encoding, the capnp event log set
   for a given source state has a stable Σ root
   (`BLAKE3(canonical_bytes(segment_files))`). Same source +
   same parser version → same Σ root, regardless of the local
   projection (mache `.db`, future MemoryStore variant, future
   alternative graph backend). The CAS layer (cloister ADR-0003)
   keys on the Σ root of the log set; the `.db` is regenerable
   from that set + the schema/parser version.

1. **Per-file delta locality (in the projection).** Mache's
   existing `DeleteFileNodes` (and the upstream `DefsMap`/`RefsMap`
   invalidation it triggers) is file-scoped within the local
   projection: a per-file source change invalidates exactly the
   nodes/refs/defs derived from that file. This matches git's
   natural diff granularity (`git diff old new` → file paths) and
   the rosary observation-lattice's per-source-event idempotence.
   The event-log layer above mache also sees per-file deltas,
   but at the source-segment record level rather than the SQL
   row level.

1. **Producer-agnostic consumption.** Per ADR-0013 (canonical
   views), every `find_smells` rule and MCP tool reads `v_refs` /
   `v_defs` rather than `node_refs` / `node_defs` directly.
   Adding a new producer (LSP via capnp event log, future SSA via
   `_lsp2_refs`, etc.) is a new UNION arm, not a fan-out across
   consumers. The same shape generalizes upward: "the CAS layer
   served me an event-log chain, project it locally" is the same
   read-path as "I just produced this event-log chain locally."

### State identity and diff

| Question                                 | Decision                                                                                                                                                                                                                                                                                                                                                                                         |
| ---------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| **What identifies a mache state?**       | The Σ root over the **capnp event log set** for the source state (per LLO ADR-0014). The `.db` projection is reproducible from `(event log set, schema, parser version)` and is therefore not itself the canonical authority — two `.db`s built from the same event log set on different machines are not byte-identical (SQLite page layout, write order). The event log set IS byte-identical. |
| **Cache key for the CAS layer?**         | Σ root of the event log set, optionally extended with `(schema_hash, parser_version)` when the projection is what's being cached. "Did this exact source state produce this exact projection?" is `Σ_root × schema × parser_version → projection`.                                                                                                                                               |
| **What's the diff granularity?**         | File-level. Matches git's natural unit, matches mache's existing `DeleteFileNodes` API, matches the rosary observation-lattice's per-event boundary. Within-file changes go through the existing splice + ShiftOrigins path. The event-log layer's natural granularity is the segment record; file-level diffs are the consumer-facing aggregation.                                              |
| **What's the invalidation alphabet?**    | Sheaf restrictions (mache-b37cff) at the per-process projection layer. When file F changes, the set of regions touching F's tokens are dirty; sheaf restriction edges propagate the dirty set across cross-file references. The cross-process / CAS layer keys on event-log Σ root identity; the projection layer keys on sheaf restrictions over the local SQL/in-memory derivative.            |
| **What's the state-transition channel?** | Git, when ADR-0007 ships. Until then, file-system events (the existing fsnotify watcher) plus explicit re-ingest via the writeback pipeline. The shape is the same — both produce file-level deltas that bubble up to event-log advances.                                                                                                                                                        |

### How the existing mache beads compose

The current mache backlog has 5 beads that each looked
independently filed before this ADR; they're actually 5 steps
of one arc:

| Bead                                   | What it builds                                                                              | Where it lands in the constellation                                                                                                                                                             |
| -------------------------------------- | ------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **mache-b37cff** Sheaf cache reads     | Per-process cache of `DetectCommunities`/`DefsMap`/projection, served by sheaf restrictions | The invalidation algebra inside one mache instance. Below `.db` identity                                                                                                                        |
| **mache-2f0075** R2 StorageBackend     | Durable `.db` storage in Cloudflare R2                                                      | The CAS layer (cloister ADR-0003) substrate for mache outputs                                                                                                                                   |
| **mache ADR-0007** Git object graph    | Git as a queryable mache source                                                             | The state-transition channel. Each commit is a new `.db` keyed by tree SHA                                                                                                                      |
| **mache ADR-0010** Hosted Mache        | Hosted-mode: cluster, R2, BYO storage                                                       | Mache running as a cloister bundle (ADR-0010 + ADR-0011), consuming vault slices                                                                                                                |
| **mache ADR-0011** Pointer Abstraction | Path/token/SHA/range/record/ref unified as Pointer                                          | Two new Pointer kinds: `cas:<Σ-root>` for content-addressed event-log-set lookups, and `proj:<Σ-root>:<schema>:<parser-version>` for the derivative `.db` lookup keyed on the projection triple |

Each can ship as a step. None re-invent the framework.

### What this ADR does NOT decide

Deliberately deferred:

- **Sheaf restriction policy.** mache-b37cff names "sheaf
  restrictions as the invalidation alphabet" but the specific
  restriction-edge weighting (co-change rate? boundary token
  count? semantic distance?) is its own design decision.
- **R2 vs other CAS substrates.** mache-2f0075 picks R2 for
  reasons specific to that bead. The CAS-layer interface
  (cloister ADR-0003) is substrate-agnostic; if a future
  decision swaps R2 for IPFS or local FS, the mache-side
  contract is unchanged.
- **Git diff vs AST diff.** This ADR locks file-level diff as
  the invalidation granularity. Future work might add an
  AST-level diff for finer invalidation (per-construct rather
  than per-file), but that's an optimization on top of the
  file-level baseline.
- **Hosted-mode auth model.** mache ADR-0010 + cloister
  ADR-0010/0011 already lock the bundle/vault shape; this ADR
  doesn't restate it.

## Consequences

**Positive:**

- Five mache beads have a shared context. The next person
  implementing any one of them sees the whole picture without
  re-deriving it.
- New design questions get filed against existing slots. "How
  should mache cache LSP enrichments?" → that's an observation
  fold per rosary ADR-0010, not a mache-internal cache.
- Cross-repo cache coherence becomes a property of the
  framework, not a per-repo invention. When mache-b37cff +
  mache-2f0075 both ship, mache instances can share `.db`s
  through the CAS layer trivially.

**Cost:**

- Implementation order matters more. b37cff (per-process
  cache) doesn't depend on 2f0075 (durable CAS), but 2f0075's
  consumer story is much weaker without ADR-0007 (git
  ingestion) providing the state-transition events. Wrong
  order ships a CAS that nobody pushes to.
- This ADR is research, not implementation. Until at least
  one of the dependent beads ships, the framework is just
  a citation list.

**Constellation-scope coupling:**

- Mache-side decisions can leak into rosary/cloister/LLO if
  this ADR's contracts drift. Specifically: the **canonical
  encoding** of capnp event logs is owned by LLO ADR-0014;
  mache as a consumer/projector inherits Σ root identity from
  upstream and cannot independently change it. Changing
  mache's projection format (`.db` schema, MemoryStore layout)
  is mache-internal — those derivatives are not Σ-anchored
  and consumers query the canonical view layer (ADR-0013), not
  the storage. Changing the file-level diff granularity
  requires rosary's observation-lattice fold to accept the
  new granularity.

## Open questions

These are flagged for follow-up, not blocked here:

1. **Mache-iegm `MultiRepoGraph` status.** rosary ADR-0009 cites
   it as the federation primitive. Is that bead currently
   shippable, or does it need work? The cross-repo cache
   coherence story depends on it.

1. **Capnp manifest contribution.** cloister ADR-0004 expects
   downstream repos to contribute manifest fragments. Does
   mache ship a `mache.capnp` slice today? If not, what shape
   should it take?

1. **Observation-fold vs in-memory cache split.** The sheaf
   cache (b37cff) is in-memory per-process. The observation
   lattice (rosary 0010) is durable across processes. When
   does an observation graduate from one to the other? The
   honest answer is probably "when the process dies and the
   next one wants to skip recomputation," but the protocol
   for that handoff isn't sketched yet.

1. **Pointer kinds for CAS + projection lookups.** ADR-0011
   says every navigable thing is a Pointer. Two related kinds
   are implied: `cas:<Σ-root>` resolves to the canonical capnp
   event-log set, and `proj:<Σ-root>:<schema>:<parser-version>`
   resolves to the derivative `.db` projection. Concrete
   syntax + resolver chain design TBD; also TBD whether to
   carry separate kinds at all vs. resolve "give me the
   projection" through the resolver chain on a `cas:` pointer
   plus a context-supplied projection spec.

## Refs

- Surfaced post-PR #362 (DefsMap/RefsMap memoization)
- Triggered by the design conversation about "repo-level cache + diff invalidation"
- Companion beads: mache-b37cff, mache-2f0075, mache ADR-0007, mache ADR-0010, mache ADR-0011
- Cross-repo: rosary ADR-0009, ADR-0010; cloister ADR-0003, ADR-0004, ADR-0010, ADR-0011; LLO ADR-0014
