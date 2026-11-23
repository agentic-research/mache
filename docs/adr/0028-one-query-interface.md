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

## Decision

### 1. One interface that readers hold

Introduce `graph.Query`: the operations readers perform, as one interface. A reader (an
MCP tool handler, a CLI command, a virtual-directory handler) holds a `Query` and calls
methods on it. **A reader never type-asserts on a graph.**

The operations are the ones readers already ask for, named by what is asked, not by how a
backend stores it: node, children, content; callers, callees; definitions of a token (exact,
case-insensitive, qualified); definition search; reference map and reference-token count;
node counts; content hash; AST kind chain; reference resolution; and a SQL scan for the
readers that are SQL by nature (smell rules, LSP tables).

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
- **Unsupported**: with a reason string. The operation returns a typed error carrying it.

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

## Open questions

- **SQL scan.** Smell rules and the LSP readers are SQL by nature. Is the SQL scan an
  operation of `Query` (Unsupported on non-SQL backends), or a separate substrate handle
  that only those readers hold? This ADR assumes the former, so that there is one interface.
- **Derived by default, or Unsupported by default?** When a backend has no native answer and
  a derived one exists but is expensive (a full walk), which does the profile choose? This
  ADR proposes: Derived when the cost is bounded and stated; otherwise Unsupported.
- **`MemoryStore`'s future.** If ADR-0026 makes the arena the substrate, `MemoryStore`
  narrows to tests and small JSON. That would shrink the matrix and should be decided before
  step 2 spends effort on it.
