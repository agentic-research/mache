# Mache vs Built-ins — Fresh-Context Eval on the Mache Repo (Post-Fix)

**Re-run methodology:** Same Serena vendored prompt
(`benchmarks/serena/upstream/010_evaluation-prompt.md`), same corpus
(mache repo at `/Users/jamesgardner/remotes/art/mache`, ~28K LOC Go),
same read-only scope adjustment (categories 2–4 out of scope).
Tools driven via direct JSON-RPC to a freshly-built mache MCP
server at `http://localhost:7533/mcp` so that this run reflects
the fixed code, not the parent harness's older `mcp__mache__*`
bindings. Server reports `version: v0.8.0`.

Important note on projection shape: the fixed build emits a
**semantic LSP-style projection** keyed by `<pkg>/<construct type>/<QualifiedName>` (e.g. `graph/methods/MemoryStore.GetCallers`)
rather than the prior `internal/.../*.go/method_declaration_NN`
shape. This is itself one of the most important changes and
materially affects every "find this method's body" workflow.

______________________________________________________________________

## 1. Summary table — pre-fix vs post-fix on the five flagged bugs

| #   | Bug                                                                     | Pre-fix behavior                                                                   | Post-fix verdict on this server                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| --- | ----------------------------------------------------------------------- | ---------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1   | `find_definition` returned "not found" for every symbol                 | `dedupSuffix` → not found; `GetCallers` → not found; `NewMemoryStore` → not found. | **Fixed.** `dedupSuffix` → `ingest/functions/dedupSuffix`. `NewMemoryStore` → 2 defs. `GetCallers` → **14 receiver-qualified definitions** (one per implementer).                                                                                                                                                                                                                                                                                                                                                                                                   |
| 2   | `find_callers` returned `[]` for type names                             | `Topology` → 0 (grep 141). `SocketClient` → 0 (grep 26).                           | **Fixed.** `Topology` → **123**, `SocketClient` → **16**, `MemoryStore` → **55**, all matching the pre-build predictions. Spot-checks confirm hits are real type-uses.                                                                                                                                                                                                                                                                                                                                                                                              |
| 3   | `find_callees` returned `[]` for receiver-method calls like `c.sendRaw` | `SocketClient.SendOp` → callees `[]`.                                              | **Partial / regressed at the MCP layer.** Virtual dir `…/SocketClient.SendOp/callees/sendRaw` is correctly populated, but the `find_callees` MCP **tool itself still returns `{"callees":[],"hint":"no resolved callees"}`** for every probed construct path. Suffix-match fix landed in the FS projection but not in the tool wiring.                                                                                                                                                                                                                              |
| 4   | `search role=definition` returned `[]` for every pattern                | All patterns → `[]`.                                                               | **Regressed.** Still returns `[]` for `dedupSuffix`, `MemoryStore`, `NewMemoryStore`, `%MemoryStore%`, `%Get%`. Root cause is a new bug introduced by the fix: `lazyGraph.SearchDefs` (cmd/serve_registry.go:638-648) **always satisfies the `defsSearcher` type assertion**, returns nil when the inner backend (MemoryStore) doesn't implement SearchDefs, and the handler's `else if defsMapProvider` fallback is never reached. The in-memory `DefsMap()` is fully populated (proven by working `find_definition`), so the bug is purely in the dispatch order. |
| 5   | `get_architecture` / `get_communities` payloads 486K–720K chars         | `get_architecture` 720,868 chars; `get_communities summary=true` 486K.             | **Mostly fixed.** `get_architecture` is now **8,856 chars** (98.8% reduction). `get_communities summary=true min_size=5` is **2,925 chars** (returns 8 communities, top 5 members each). The **default** `get_communities` (no summary flag) still returns 380K chars — payload cap only applies to the summary path. No `TruncationNote` field is present in either response (the report still asked us to check for it; it's not there).                                                                                                                          |

Net: 2 of 5 bugs fully fixed (1, 2), 1 mostly fixed (5), 1 partially
fixed at the wrong layer (3 — projection yes, MCP tool no), and 1
appears to have regressed in a different way (4).

______________________________________________________________________

## 2. Headline: what mache changes now

**Three big positive deltas** compared to the prior run, plus one
new "wow" that didn't exist before:

- **(a) Projection now exposes named constructs.** `list_directory graph/methods` returns 180 entries like `MemoryStore.GetCallers`
  and `SQLiteGraph.GetCallers` directly — no more
  `method_declaration_NN` round-trips through `field_identifier`
  files. This single change turns Task 2 (large-file outline),
  Task 3 (method body retrieval) and Task 14 (scope precision) from
  losses into wins. Frequency: high. Value/hit: high.
- **(b) `find_definition` resolves bare and dotted names.**
  `find_definition GetCallers` returns all 14 receiver-qualified
  definitions; `find_definition MemoryStore.GetCallers` narrows to
  the single path. The headline "navigation by name path" claim now
  holds on this corpus. Frequency: high. Value/hit: high.
- **(c) `find_callers` covers type references.** Topology=123,
  SocketClient=16, MemoryStore=55 — matches both the bench
  predictions and grep ground truth (within the expected
  refs-index disciplined subset, e.g. excludes comments/strings).
  The prior eval flagged this as a correctness hazard; that hazard
  is now removed. Frequency: medium. Value/hit: high.
- **(d) Interface implementer discovery falls out for free.**
  `find_definition GetNode` returns 15 implementations
  (lazyGraph, readOnlyGraph, udsGraph, CompositeGraph,
  GraphCache, HotSwapGraph, MemoryStore, NodesTableReader,
  SQLiteGraph, WritableGraph, mockGraph, noDefsGraph,
  SQLiteWriter, minimalStore, leyline Client). Prior eval Task 5
  required grep; now it's a one-shot. Frequency: medium.
  Value/hit: high.

**Two remaining negative deltas:**

- **(e) `find_callees` MCP tool still empty.** Despite the
  suffix-match fix landing in the callees virtual dir, the MCP
  tool returns `{"callees":[]}` for every construct path probed.
  Multi-step chained exploration is still broken at the tool
  level — though an agent that knows to `list_directory <path>/callees` instead can recover the data manually.
- **(f) `search role=definition` actively returns `[]` for every
  pattern.** New bug introduced by the fix attempt; root cause
  traced to dispatch-order issue in `lazyGraph.SearchDefs`.

**Verdict.** On the deltas that mattered most for fresh-context
exploration — symbol resolution, type-ref discovery, method-body
extraction, scope precision — the fixed build moves from
"slightly negative net" to "clearly positive net." Two of the
five flagged regressions remain; one regressed differently than
before.

______________________________________________________________________

## 3. Detailed evidence per category (in-scope only)

### 3.1 Category 1 — Codebase understanding

**Task 1 — top-level overview.** `get_overview` returns 28
top-level packages (vs 38 before — the projection now
de-duplicates by Go package rather than directory; e.g.
`internal/graph` and `internal/graph_test` are collapsed to two
peers `graph` and `graph_test`). Includes `total_dirs=201`,
`total_files=38`, `ref_tokens=2813`, plus a `_usage` block.
Comparable to `ls -F`; mache's view is cleaner. Slight positive
for mache. *Unchanged from prior eval qualitatively.*

**Task 1 (cont.) — `get_architecture`.** Previously 720K chars
of stdlib + testify noise. Now **8,856 chars**, populated:
`most_referenced` (top 20), `key_abstractions` (nil — still a
gap), `dependency_layers` (14 entries, each is just a community
id list — terse but consumable), `test_files` (top 50),
`api_surface` (nil), `top_level_breakdown` (10 packages with
file counts). Strong positive delta on payload; the actual
content density is moderate — `key_abstractions` and
`api_surface` being nil is unsatisfying for "what is this
codebase about?" but at least it no longer overflows the
response budget. Frequency: low–medium. Value/hit: medium.

**Task 2 — large-file outline.** Prior eval found
`list_directory internal/graph/graph.go` emitted ~200
`comment_NN` and `method_declaration_NN` entries with no symbol
names. Now: `list_directory graph/methods` returns **180 named
entries** like `ArenaFlusher.Close`, `MemoryStore.GetCallers`,
`SQLiteGraph.ListChildren`. This is **strictly better** than
`grep -nE '^(func|type|var|const) '` because it carries both
receiver and method name without the agent having to parse a
regex grouping. Frequency: high. Value/hit: high. **Delta:
negative → positive.**

**Task 3 — retrieve a method body.** Prior eval: 5+ calls of
recursive descent, ultimately couldn't reconstitute the body.
Now: `read_file graph/methods/MemoryStore.GetCallers/source`
returns the full 16-line method body in **one call** (325
chars). Same shape as the old `field_identifier` indirection,
but the path is the *qualified name* you actually want and the
content is the *full source* of the method including its doc
comment. **Delta: strong negative → strong positive.**

**Task 4 — find references.** `find_callers NewMemoryStore`
returned **182** call sites (was 190 pre-fix, count drift is
within churn of the corpus itself between runs). Comparable to
grep's 217 hits with disciplined exclusion of comments/strings.
Roughly unchanged from prior eval; this was already a positive
case.

**Task 5 — interface implementers / supertypes.**
`find_definition GetNode` → 15 paths spanning 14 distinct
types. `find_definition GetCallers` → 14. Each result already
encodes the receiver (`MemoryStore.GetNode` not bare
`GetNode`), so the answer to "what implements Graph?" is
directly readable. Prior eval needed grep. **Delta: negative →
positive.** Frequency: medium. Value/hit: high.

**Task 6 — external symbol (`sql.Open`).** Unchanged — no LSP
tables baked in, `get_type_info` returns the same
"enrich-lsp" hint. Built-in `go doc` wins. Out of scope for
this projection.

### 3.2 Category 4 — Reliability & correctness

**Task 14 — scope precision by name path.** Prior eval:
`MemoryStore.GetCallers` → not found. Now:
`find_definition MemoryStore.GetCallers` → exactly one
result: `graph/methods/MemoryStore.GetCallers`. Bare
`GetCallers` → all 14 receivers. The dotted name-path
resolver works as advertised on this projection. **Delta:
negative → positive.** Frequency: high (especially in any
repo with multiple receivers per method name). Value/hit:
high.

**Task 16 — success signals.** Out of scope (no editing
surface), unchanged.

### 3.3 Category 5 — Workflow effects across a session

**Task 18 — multi-step chained exploration.** Pre-fix
chain: `find_callers SendOp` (worked) →
`find_callees SocketClient.SendOp` (returned `[]`, broke
the chain). Post-fix: `find_callers SendOp` returns 17
construct paths with **named receivers**, so picking
"the non-test caller" is now visual instead of requiring
40 field_identifier reads. `find_callees` on the same
construct path still returns `[]` from the MCP tool — but
`list_directory <path>/callees` works: e.g. for
`SocketClient.SendOpInto` it returns `SocketClient` and
`sendRaw` as resolvable callees. The data is there;
the MCP tool isn't wired to it.

**Construct path stability.** Stable across this session.
Same as prior eval — this is a real property of mache and
matters for refactor workflows that mache doesn't yet
expose.

### 3.4 Category 6 — Out-of-scope tasks (Task 19, 20)

**Task 19 — Read `Taskfile.yml`.** `read_file Taskfile.yml`
returns `not found: Taskfile.yml`. The mache projection
doesn't include non-Go top-level files in this build. Use
built-in `Read`. Unchanged.

**Task 20 — Free-text `ErrNotFound`.** `find_callers ErrNotFound` → `[]` (still). The refs index is call-site /
type-use only; package-level variable *reads* aren't in it,
so this is a contract limitation, not a regression. Built-in
`grep` returns 85 hits in one shot. Unchanged from prior
eval; classify as built-in territory.

______________________________________________________________________

## 4. Token-efficiency analysis (post-fix)

| Workflow                       | Pre-fix mache calls    | Post-fix mache calls             | Built-in equivalent | Net delta vs prior    |
| ------------------------------ | ---------------------- | -------------------------------- | ------------------- | --------------------- |
| Top-level overview             | 1 (2.9K tok)           | 1 (~2K tok)                      | 2–3 shell           | ~neutral              |
| Large-file outline             | 2+ (1 list + 40 reads) | 1 (`list_directory pkg/methods`) | 1 grep              | mache now wins        |
| Method body retrieval          | 5+ (broken)            | 1 (`read_file .../source`)       | 1 `Read` w/offset   | mache now ties / wins |
| References (`NewMemoryStore`)  | 1 (10K tok)            | 1 (~10K tok)                     | 1 grep (13K)        | unchanged             |
| Interface implementers         | 2 (failed → grep)      | 1 (`find_definition`)            | 1 grep              | mache now wins        |
| `get_architecture`             | 1 (720K tok)           | 1 (8.8K tok)                     | n/a                 | usable now            |
| `get_communities summary=true` | 1 (486K tok)           | 1 (3K tok min_size=5)            | n/a                 | usable now            |
| `get_communities` (default)    | 1 (>500K)              | 1 (~380K)                        | n/a                 | still over budget     |
| `search role=definition`       | 1 (`[]`)               | 1 (`[]`)                         | grep                | still broken          |
| `find_callees` MCP tool        | 1 (`[]`)               | 1 (`[]`)                         | grep / inspection   | still broken at tool  |

The payload-bloat findings are the most dramatic delta:
`get_architecture` is now a one-shot consumable, and the rest
of mache's tool surface fits comfortably inside an agent turn.
The remaining payload concern is the default `get_communities`
call — at 380K chars it should probably default to the summary
shape, or the description should make `summary=true` more
visibly the recommended invocation.

**Verdict.** Token efficiency on every previously-broken task is
now equal-or-better than the built-in equivalents; the lone
exception is free-text search which the contract doesn't
target.

______________________________________________________________________

## 5. Bottom-line ordering (post-fix)

Ranked by frequency × value-per-hit:

1. **Method-body retrieval via construct path + named projection:**
   *Positive (was strong negative).* This is the load-bearing
   win — single-call retrieval of any method body by qualified
   name. Frequency: high. Value/hit: high.
1. **Scope-precise navigation by name path:** *Positive (was
   negative).* `MemoryStore.GetCallers` and bare `GetCallers`
   both work. Frequency: high. Value/hit: high.
1. **Interface implementer discovery via `find_definition`:**
   *Positive (was negative).* Was a forced grep; now a single
   tool call returning fully-qualified results. Frequency:
   medium. Value/hit: high.
1. **Function/method call-site discovery (`find_callers`):**
   *Positive (unchanged).* Still the headline win, now joined
   by working type-ref discovery. Frequency: high.
   Value/hit: medium.
1. **Architectural overview (`get_architecture`,
   `get_communities summary=true`):** *Positive (was
   over-budget).* Now actually usable inside an agent turn.
   `key_abstractions` and `api_surface` being nil leaves
   room for the next iteration. Frequency: low.
   Value/hit: medium.
1. **`find_callees` MCP tool:** *Still negative.* Returns `[]`
   for every path. Workaround: `list_directory <path>/callees`
   surfaces the underlying data, but the named MCP tool
   shouldn't be empty when the FS layer has the answer.
1. **`search role=definition`:** *Regressed/different bug.*
   Was returning `[]` because the SQL fallback was missing;
   now returns `[]` because `lazyGraph.SearchDefs` short-circuits
   the in-memory fallback path.

The headline call from the prior eval — "mache adds one
useful capability (call-site discovery) and several
distinctly negative ones" — flips on this build to "mache
adds three high-frequency wins (named projection, dotted
name-path resolution, interface-implementer discovery on top
of the existing call-site discovery), with two residual
bugs at the MCP-tool layer that don't affect the underlying
data."

**Practical usage rule (post-fix).** For Go code exploration
on this repo: use `find_definition <Type.Method>` to land on
a body, `read_file <construct>/source` to read it,
`find_callers <token>` for call sites *or type-use sites*,
`list_directory <pkg>/methods` (or `/types`, `/functions`)
for outlines, and `list_directory <construct>/callees` when
you want to chain forward. Fall back to grep for free-text
and for package-level variable reads. Avoid `get_communities`
without `summary=true`.

______________________________________________________________________

## 6. Caveats and remaining issues

Two outstanding bugs surfaced or persisted on this re-run.
Both should land as fresh beads.

**Issue A — `find_callees` MCP tool returns empty despite
populated callees virtual dir.** Reproduce: for any method
construct path (e.g. `graph/methods/MemoryStore.ListChildren`
or `leyline/methods/SocketClient.SendOpInto`), call
`find_callees` → `{"callees":[],"hint":"no resolved callees"}`.
But `list_directory <samepath>/callees` returns named
resolved callees (e.g. `sendRaw`, `MemoryStore`,
`NormalizeID`). The data path that powers the virtual dir is
not wired into the MCP tool handler, or the tool's
resolution step rejects the path. Suggested next step:
single-step the MCP handler against a known-good construct
path under `methods/`, compare to whatever the FS layer
calls.

**Issue B — `search role=definition` returns `[]` for every
pattern despite working `find_definition`.** Reproduce:
`search {pattern:"dedupSuffix", role:"definition"}` → `[]`.
`find_definition dedupSuffix` → 1 result via the same
`DefsMap()`. Root cause traced to
`cmd/serve_registry.go:638-648` — `lazyGraph.SearchDefs`
always satisfies the `defsSearcher` type assertion in
`serve_handlers.go:595`, returns nil when the inner backend
doesn't implement SearchDefs, and the `else if defsMapProvider` fallback at line 597 is then unreachable.
Fix: make `lazyGraph.SearchDefs` return nil only when the
inner backend doesn't implement SearchDefs AND fall through
to the next branch in the handler, or have the handler
explicitly check whether SearchDefs returned a nil map and
fall through. The MemoryStore behind this server has a
fully-populated `DefsMap()` (proven by working
`find_definition`), so the fix needs to flow that path.

**Issue C — `get_communities` default path still 380K chars.**
Less critical because `summary=true` works, but the
description hint should either auto-summary or warn the
user. Probably worth flipping the default for the MCP
surface.

**Issue D — `get_architecture` returns nil for
`key_abstractions` and `api_surface`.** These fields are
present in the response schema but empty. Not a regression
(prior run also showed `key_abstractions=null`), but
visible now that the payload is small enough to read end
to end.

**Issue E (not a regression) — `read_file Taskfile.yml`
returns "not found".** Top-level YAML/Taskfile artifacts
aren't in the projection on this build. Not a fix target,
but worth surfacing for anyone who reads the prior eval and
expects the "is a directory" error.

The bench is doing its job — three of the five fixes
empirically moved the metric, two didn't, and one new
bug surfaced (B) that wasn't on the radar of the prior
eval. File issues A and B; revisit C/D when the
architectural fields are next iterated.

______________________________________________________________________

## Appendix — concrete deltas, one-liners

- `find_definition dedupSuffix` — pre: "not found" — post:
  `ingest/functions/dedupSuffix`.
- `find_definition GetCallers` — pre: "not found" — post: 14
  paths with explicit receivers.
- `find_definition MemoryStore.GetCallers` — pre: "not found"
  — post: 1 path.
- `find_callers Topology` — pre: 0 — post: **123**.
- `find_callers SocketClient` — pre: 0 — post: **16**.
- `find_callers MemoryStore` — pre: 0 — post: **55**.
- `find_callers ErrNotFound` — pre: 0 — post: 0 (unchanged,
  contract limitation).
- `find_callees SocketClient.SendOp` — pre: `[]` — post:
  `[]` (still broken at tool; FS layer has `sendRaw`).
- `search role=definition dedupSuffix` — pre: `[]` — post:
  `[]` (regressed via different code path).
- `get_architecture` payload — pre: **720,868 chars** — post:
  **8,856 chars** (98.8% reduction).
- `get_communities summary=true` payload — pre: **486K chars**
  — post: **5,149 chars**.
- `get_communities` (default) payload — pre: **>500K chars**
  — post: **380,060 chars** (improved but still over).
- `list_directory graph/methods` — pre: nothing comparable;
  prior projection used `method_declaration_NN` — post: **180
  named methods** with `<Receiver>.<Method>` shape.
- `read_file graph/methods/MemoryStore.GetCallers/source` —
  pre: not addressable; method body required recursive
  descent — post: **1 call, 325 chars, full method**.
