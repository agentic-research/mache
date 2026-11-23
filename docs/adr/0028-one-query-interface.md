---
title: "ADR-0028: One Query Interface — Readers Ask for Operations, a Profile Says Where Each Is Answered"
status: Proposed
date: 2026-11-22
tags: [architecture, graph, mcp, cli, ingest, correctness, parity]
---

**Relates to:**

- ADR-0027 (One Startup Path — decides how a graph is *opened*; this ADR decides what a
  reader *holds* once it is open. The two share one object: see "One profile, not two".)
- ADR-0011 (Pointer abstraction), ADR-0013 (canonical `v_refs` / `v_defs`)
- Bead `mache-4dd685` (this ADR's tracking bead)
- Bead `mache-35e730` (quadratic `--out`), `mache-e3d9bb` (merge inert on SQLite),
  `mache-a6c283` (`get_overview` 4x slower through a wrapper)
- Issues #821 (`trace/` virtual directory) and #74 (`detect_changes`), which wait on this

## Context

Mache has one small mandatory interface, `graph.Graph`, and fifteen optional ones in
`graph/capabilities.go`. A reader that wants more than the mandatory interface type-asserts
for an optional one and falls back when the assertion fails.

Measured on `main` at 2026-11-22 (method-name match over `graph/`, `internal/`, `cmd/`,
tests excluded):

| Optional interface       | MemoryStore | SQLiteGraph | Writable | Composite | HotSwap | UDS | lazyGraph |
| ------------------------ | ----------- | ----------- | -------- | --------- | ------- | --- | --------- |
| `DefsLookuper`           | yes         | yes         | -        | yes       | -       | -   | yes       |
| `DefsSearcher`           | yes         | yes         | -        | yes       | -       | -   | yes       |
| `DefsMapper`             | yes         | yes         | -        | yes       | -       | -   | yes       |
| `RefsMapper`             | yes         | yes         | -        | -         | -       | -   | yes       |
| `RefTokenCounter`        | -           | yes         | -        | -         | -       | -   | yes       |
| `NodeCounter`            | yes         | yes         | -        | -         | -       | -   | yes       |
| `RefsQuerier`            | yes         | yes         | -        | -         | -       | -   | yes       |
| `DBPathProvider`         | -           | yes         | yes      | -         | -       | -   | yes       |
| `RefResolver`            | -           | yes         | -        | -         | -       | -   | yes       |
| `ASTKinder`              | -           | yes         | -        | -         | -       | -   | yes       |
| `DefTokener`             | -           | yes         | -        | -         | -       | -   | yes       |
| `ContentHasher`          | -           | yes         | -        | -         | -       | -   | yes       |
| `DefsNodeLookuper`       | -           | yes         | -        | -         | -       | -   | yes       |
| `MountPrefixer`          | -           | -           | -        | yes       | -       | -   | yes       |

There are 54 such assertions outside tests. They are made by the readers themselves:
`find_definition` probes three interfaces (`DefsLookuper`, `DefsMapper`, `RefsQuerier`) and
so has three ways to find a definition; `get_overview` probes four; `pack` (CLI) probes two;
the smell engine probes two more. `lazyGraph`, the wrapper every MCP session holds, has 42
methods, most of them forwards.

### The defect

**What a reader gets depends on which backend happens to be underneath, and nothing says
which answer is the right one.** The table is not a feature matrix that someone designed.
It is the residue of each capability being added to the backend that needed it first.

Three consequences, all observed:

1. **A missing forward is a silent slow path, not an error.** `DBPath` went missing from
   `lazyGraph` once. `RefTokenCount` went missing after it and made the live daemon's
   `get_overview` four times slower than the same code in process (`mache-a6c283`).
   `NodeCounter` needed a forward and a registry-test entry to avoid a third (#823).
2. **The same question has more than one answer.** A reader that falls back from an
   indexed lookup to a map scan, or from a count to a walk, is running a different
   algorithm with different edge cases. Whether the fallback is "the same answer, slower"
   or "a less complete answer" is not written down anywhere, and is not tested as a
   property. `get_overview` reported 316 directories for a graph of 164,000 because its
   fallback counted two levels (#820).
3. **The write side reads through the read interface.** `ingest.IngestionTarget` embeds
   `graph.Graph`, so every writer implements the whole read interface (mostly as no-ops)
   and the engine reads back from the store it is writing. A merge that worked in memory
   was inert on SQLite (`mache-e3d9bb`). `mache --out` re-listed a shared directory's
   children for every record under it, which made a 164,464-record projection an estimated
   28 hours (`mache-35e730`). The fix for that added a sixteenth optional interface.

`find_clones` on the tree shows the same thing from the outside: after the mechanical
duplicates were merged (#826), 25 production clone sets remain and none is identical apart
from renames. They are forwarding methods, no-op methods on writers, and one handler type
per virtual directory. Duplication is the symptom here, as ADR-0027 found for startup.

## The test this design is held to: developer experience

Four people meet this layer. Each open question below was settled by asking what that
person has to know.

| Who                                   | Today has to know                                                                                             | Should have to know                                                              |
| ------------------------------------- | ------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------- |
| Someone adding a tool or CLI command  | Which of 15 optional interfaces to probe, in what order, and what to do when each is missing                  | The operations. One call each.                                                   |
| Someone adding or wrapping a backend  | Which optional interfaces readers happen to probe, and to forward every one by hand                           | The interface and its conformance suite. The compiler and the suite say the rest |
| An agent or person calling a tool     | Nothing, and so cannot tell a complete answer from a fallback's partial one, or why a call took 14 seconds    | That an answer is the specified one, or a message saying what is needed instead  |
| Someone writing a smell rule          | SQL against the documented views. This is the one place SQL is the right thing to hold                        | The same                                                                         |

Two rules follow, and they decide most of what is below:

- **Nobody chooses an algorithm by inspecting a backend.** Not the tool author (no
  assertions), not the caller (no silent fallback).
- **A slow or missing answer is announced, with what to do about it.** An error that says
  "node counts are unavailable on a live arena; build a database with `mache --out`" is
  better developer experience than a correct answer that sometimes takes 14 seconds.

## Decision

### 1. One interface that readers hold

Introduce `graph.Query`: the operations readers perform, as one interface. A reader (an
MCP tool handler, a CLI command, a virtual-directory handler) holds a `Query` and calls
methods on it. **A reader never type-asserts on a graph.**

The operations are the ones readers already ask for, named by what is asked, not by how a
backend stores it: node, children, content; callers, callees; definitions of a token (exact,
case-insensitive, qualified); definition search; reference map and reference-token count;
node counts; content hash; AST kind chain; and reference resolution. Raw SQL is not one of
them: see "SQL is a separate, named surface".

Because it is one interface, a wrapper that does not implement an operation **does not
compile**. Forgetting a forward stops being possible.

### 2. One meaning per operation, fixed by a conformance suite

Each operation has a single specified result. The specification is a conformance suite: a
planted corpus with known answers, run against every backend, in the style already used for
`find_clones` (`internal/smells/find_clones_test.go`) and the transport matrix
(`internal/mcpserve/transport_matrix_test.go`).

This is what answers "which backend is right". None of them is, by default. The suite is,
and a backend passes or does not. A backend that returns a different answer for an operation
it claims is a failing test, not a tier.

### 3. A profile says where each operation is answered

Each backend carries a `Profile`: for every operation, exactly one of

- **Native**: the backend answers it directly.
- **Derived**: answered by a shared implementation over other operations, with a stated
  cost (for example, node counts by walking children). Written once, in `graph`, not in
  each reader.
- **Unsupported**: with a reason and a remedy. The operation returns a typed error carrying
  both: what this backend cannot do, and what the caller can do to get the answer.

**Derived is allowed only when its cost is bounded and stated** (a walk capped at a fixed
number of directory listings, say, that reports when it stopped short). An unbounded
derivation is Unsupported. `get_overview` in `--mount` mode took 14 seconds per call for a
week of `main` because a derivation with no real bound was the default (`mache-553d34`).

The profile is data. It is printed by `mache doctor`, returned by `get_overview`, and
committed as a golden table so a change to it is a reviewed diff.

**Readers contain no fallback logic.** A reader calls the operation. It gets the specified
answer (Native or Derived) or the typed Unsupported error, which the tool reports to the
user as "this needs X". There is no third path in which a reader quietly computes something
approximately equivalent.

This removes the heuristic. Today the choice between an index, a map scan and a walk is made
by a chain of type assertions in each handler. After this it is made once per backend, in a
table, and tested.

### 4. Writers are sinks, not graphs

Replace `IngestionTarget` with `ingest.Sink`: the write operations, plus a deliberately
small read-back contract (does a node exist; what are its children) whose cost each sink
states. The engine may depend on that contract and on nothing else, so it cannot behave
differently depending on which store it is writing to. `childrenIgnorer` (#828) is folded
into it.

### 5. Readers map to operations; shared machinery sits on `Query`

- **MCP tools and CLI commands** are thin adapters: arguments in, one or a few `Query`
  operations, a response out. The same adapter serves both surfaces, so the CLI and an MCP
  tool cannot disagree about an answer. (`find-smells` already works this way through
  `newSmellResponse`.)
- **Traversal** (`get_impact`, `get_dataflow`, and the walks #821 and #74 need) is one
  package over `Query`: one bounded walk with one visited set, one cap and one rule for
  generic names.
- **Virtual directories** (`callers/`, `callees/`, `context`, `trace/`) are entries in a
  registry: a name, a list function and a target function over `Query`. One generic handler
  serves them.

### 6. SQL is a separate, named surface

Raw SQL is not an operation of `Query`. It is the substrate handle: the documented views
(`v_ast`, `v_defs`, `v_refs`, `v_nodes`) over a parsed database, held by the readers whose
*product* is a SQL query — the smell engine, whose rules users write in SQL, and anyone
debugging a projection.

A tool that answers a question about the graph must not hold it. Several do today as a
shortcut (`find_callers`, `find_definition`, the LSP readers); each becomes a typed
operation as it migrates. The reason is the table above: a tool author who can reach for
SQL will, and then that tool works on one backend and its answer is defined by a query
nobody else can see. Keeping the handle separate makes "this reader depends on the SQL
substrate" a visible fact about it, where a wide interface would hide it.

### 7. One store a user has to think about

A person serving a graph should not have to know that mache has two stores. Today they do:
`mache serve data.json` ingests into memory on every start (four minutes for a 164,464-record
graph), while `mache --out` then `mache serve out.db` opens in five seconds.

`serve` on a data source uses a built projection when one exists for its inputs, and
builds one when none does. The in-memory store narrows to what it is good for: tests, and
inputs small enough that building anything is not worth it. It is then held to the
conformance suite for the operations it keeps, not for all of them, which is most of the
parity work this ADR would otherwise have to do twice.

**The cache is ley-line's, not mache's.** Ley-line-open already owns the cache substrate
(blob store, content-defined chunks, sheaf, daemon protocol), and its ADR-0021 defines the
manifest every consumer should use: *(input hash + processor version + schema version) →
content-addressed chunk*. A data projection is exactly that shape: the input is the data
file's content, the processor is the mache version, the schema is the topology's hash.
Mache states those three and asks; it does not keep a second cache with its own keys and
its own invalidation. `$MACHE_HOME/parse` and `mache cache` are the parallel substrate
bead `mache-e64f36` already says to retire, and this decision must not add a third.

**What is built should be small.** The engine path materialises every node as a row: two
minutes and 2.4 GB for the 164,464-record graph. The direct path already serves records
lazily from one row each, with no ingestion, but ignores `refs`, so it cannot answer
`find_callers`. A direct path with a refs index (one row per record, one per reference,
nodes projected on demand) is the candidate for what gets built and cached. It is
unmeasured; step 1 of the migration includes measuring it against the engine path.

### One profile, not two

ADR-0027's `Plan` already names capability axes for *opening* a graph (backend, freshness,
cascade, writable). The `Profile` here is the same idea for *reading* one. They must be one
object: `Open(plan)` returns a `Query` whose `Profile` is a consequence of the plan. A
capability is requested in the plan and reported in the profile. Designing them separately
would recreate the defect.

## Alternatives considered

- **Keep optional interfaces; add an unwrap helper (`graph.As[T]`, in the manner of
  `errors.As`).** Fixes forgotten forwards in wrappers. Does not fix the defect: readers
  still choose an algorithm by what they find, and nothing defines the right answer.
- **Generate the forwards.** Same limitation, plus a generator to maintain.
- **Make every backend implement everything natively.** Forces, for example, an index into
  `MemoryStore` that exists only to match SQLite. `Derived` gives the same guarantee for the
  answer without requiring the same storage.
- **Leave it and rely on the clone and smell gates.** The gates report the symptom after it
  ships. Two of the three incidents above were found by users.

## Consequences

**Removed.** The 54 reader-side assertions; the hand-written forwards in `lazyGraph`,
`WritableGraph`, `HotSwapGraph` and `CompositeGraph`; the no-op read methods on
`SQLiteWriter` and `UDSGraph`; the per-directory handler types in `internal/vfs`; the second
traversal. Each is a number that can be checked: `grep` for assertions, `find_clones` for
sets, and an assertion ratchet in `internal/lint` in the style of the `regexp` import
ratchet.

**Added.** The conformance suite, which is new work and is the point. `Query` is a wide
interface; that is deliberate, and its width is the honest size of what readers ask for.

**Changed for users.** A tool on a backend that cannot answer says so, where today it may
return a slower or thinner answer. Some calls that "worked" will start reporting
Unsupported. That is the behaviour change to review, and the conformance run in step 1 below
lists every such case before any code changes.

**Risk.** A wide interface can become a dumping ground. The rule that keeps it honest: an
operation is added to `Query` only with its conformance cases and a profile entry for every
backend.

## Migration

Each step is independently shippable and leaves `main` working.

1. **Measure before changing.** Write the conformance suite against *today's* backends
   through today's interfaces, and commit the resulting matrix, including every
   disagreement. No behaviour changes. This turns "we are likely handling things subtly
   differently" into a list.
2. **Decide each disagreement** from that list: which answer is specified. Fix the backends
   that differ, one PR each.
3. **Introduce `Query` and `Profile`** as an adapter over the existing backends. Migrate
   readers tool by tool; each migration deletes that reader's assertions. The assertion
   ratchet only goes down.
4. **Introduce `ingest.Sink`.** The scale gate from `mache-35e730` and the golden projection
   tests are the parity proof.
5. **Traversal package and virtual-directory registry**, then #821 and #74 on top of them.
6. **Join with ADR-0027**: `Open(plan)` returns a `Query`.

## Decided during review

- **SQL**: a separate surface, not an operation (decision 6). It is closer to a debugging and
  extension surface than to the rest, and the tools that use it as a shortcut are the ones
  whose answers are least defined.
- **No native answer**: Derived only with a bounded, stated cost; otherwise Unsupported with a
  remedy (decision 3).
- **The in-memory store**: narrows to tests and small inputs; `serve` on data builds and
  caches a database (decision 7).

## Open questions

- **Where the size line sits** for "small enough to stay in memory", and whether it is a
  fixed number of records or a setting.
- **Ley-line has no keyed artifact cache yet.** Checked with the ley-line-open release
  session against `origin/integration` 8907b793 on 2026-11-22:
  - LLO ADR-0021 (the manifest) is Proposed, implemented in neither v0.30.0 nor v0.31.0, and
    not scheduled. No CLI, daemon op or library call maps (input, processor, schema) to a
    stored artifact.
  - LLO caches only its own derivations. Foreign bytes can be held by `leyline load` or as a
    blob in the git-shaped store (LLO ADR-0045), addressed by path or content hash. Neither
    is looked up by the consumer's keys.
  - `leyline load` copies the whole file into the arena and hashes it: an unchanged file is
    cheap to *detect* (same root) and not cheap to *load* (O(size); a 2.4 GB database needs
    an arena sized above that). Content-defined chunking covers `source_blobs`, not arena
    images, so a loaded database is not deduplicated.

  So decision 7's "the cache is ley-line's" is a request to ley-line, not something mache can
  adopt today. It needs a ley-line bead and an ADR-0021 decision. Until then the honest
  options are: keep re-ingesting; or key the artifact by a path convention in ley-line's
  store, which is mache keeping the mapping after all. This ADR does not choose the second
  without that being said out loud.
- **Size of what is cached matters more now.** Because a load is O(size), the small form
  (records plus a refs index, nodes projected on demand) is what makes any cache cheap. It
  is still unmeasured.
