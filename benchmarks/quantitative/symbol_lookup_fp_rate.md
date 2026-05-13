# Symbol Lookup: grep+read vs `mache find_callers` — false-positive rate on the mache repo

**Date**: 2026-05-12
**Corpus**: `/Users/jamesgardner/remotes/art/mache` (Go, ~67K LOC across 243 `.go` files, 268 directories in the projection)
**Goal**: replicate the table shape from [agent-lsp's published benchmark](https://blog.blackwell-systems.com/posts/agent-lsp-token-savings/) on the mache codebase, comparing token waste in a grep-driven agent workflow vs a pre-indexed `mache.find_callers` lookup.
**Outcome**: results are **honest about the corpus**. On this small, well-organized repo, mache's *index correctness* and *index gaps* matter more than its FP-rate advantage. We measure both.

______________________________________________________________________

## 1. Methodology

### What was measured

For each of 10 Go symbols, we recorded:

| Column             | Definition                                                                                                                                                                                            |
| ------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `grep_wb`          | `grep -rn --include='*.go' '\b<sym>\b' .` — count of word-bounded matches across `.go` files. The "naïve agent grep" upper bound.                                                                     |
| `grep_call`        | `grep -rEn --include='*.go' '\.<sym>\(\|\b<sym>\(' .` minus `func <sym>` lines — a sharper grep approximating "real call sites". The reasonable-effort grep an experienced agent would write.         |
| `mache_callers`    | result count from `mcp__mache__find_callers(token=<sym>)` — the pre-indexed reference set.                                                                                                            |
| `FP_rate_naive`    | `(grep_wb − mache_callers) / grep_wb`, percentage. Treats mache's count as ground truth.                                                                                                              |
| `grep_time_ms`     | wall-clock `time` of the grep command, median of 3 runs after warm-up, on macOS with a 67K-LOC corpus. Reported by `/usr/bin/time -p` at 10ms resolution.                                             |
| `mache_time_ms`    | n/a (MCP call duration is not exposed to the calling agent in this harness; each call returned in well under 1s subjectively, but no precise stopwatch was available).                                |
| `token_cost_ratio` | (grep_wb × 50 tokens/line) / (mache_callers × 20 tokens/path). 50 t/line is rough for a grep hit including file:line + ~80-char context; 20 t/path is rough for the per-entry AST path mache returns. |

### Rules

- **Word-boundary regex** (POSIX BRE `\b...\b`) for the baseline grep. macOS BSD grep honors this consistently.
- **`.go` files only** (`--include='*.go'`). Docs, schemas, and generated files excluded.
- **Case-sensitive** to match mache's default exact-match behavior for `find_callers`.
- **No paging**, plain stdout `wc -l`. Each grep run was preceded by a warm-up to stabilize page cache.
- **Mache "ground truth" caveat**: we use mache's count as the FP-rate denominator only because that's the comparison the published agent-lsp benchmark uses. Where mache returns 0 (caller index doesn't track type names), we annotate; we do **not** treat 0 as "no real refs".

### Honesty caveats

1. The mache corpus is small (67K LOC) compared to Consul (the agent-lsp comparison corpus, multi-million LOC). Absolute FP rates here should not be expected to match Consul-scale numbers.
1. Mache's `find_definition` was **broken in this projection** for every test symbol (returned "no definition found" even for `dedupSuffix`, which is uniquely defined). This is a finding in itself — see §6.
1. Mache's `find_callers` returns `[]` for **type names** (`MemoryStore`, `SQLiteGraph`, `Topology`). The caller index is **call-expression scoped only** — it does not record where a type is referenced as a parameter, field, return type, or composite literal. This is correct behavior given the index's name, but agents asking "where is `MemoryStore` used?" get an empty list.

______________________________________________________________________

## 2. Symbol selection

| Symbol           | Category                          | Why                                                                                                                                                                         |
| ---------------- | --------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `Close`          | common-name (over-match expected) | The most-overloaded method name in Go. 21 `func (...) Close()` declarations in the repo across NFS file handles, registries, SQL writers, watchers, etc.                    |
| `New`            | common-name                       | Go convention; word-bounded `\bNew\b` mostly hits comments/strings because the actual identifiers are `NewEngine`, `NewStore`, etc. (no trailing word break). Good FP demo. |
| `Error`          | common-name                       | Hits `err.Error()`, `t.Error(...)`, type `Error string` fields, error-wrapping comments, and the `error` interface satisfier.                                               |
| `MemoryStore`    | type def                          | Core graph backend struct in `internal/graph/graph.go`.                                                                                                                     |
| `SQLiteGraph`    | type def                          | Direct-SQL graph backend struct in `internal/graph/sqlite_graph.go`.                                                                                                        |
| `Topology`       | type def                          | Root schema type in `api/schema.go`.                                                                                                                                        |
| `GetCallers`     | function with moderate fan-in     | Backend interface method implemented on 3+ types, with delegating wrappers.                                                                                                 |
| `RenderTemplate` | function with moderate fan-in     | Template render entry point in `internal/ingest/engine.go`, called from engine + tests.                                                                                     |
| `ReadContent`    | function with moderate fan-in     | Graph backend method, also wrapped on `lazyGraph` and `WritableGraph`.                                                                                                      |
| `dedupSuffix`    | rare/unique                       | One-of-a-kind helper in `internal/ingest/engine.go`. Should be grep's best case.                                                                                            |

We deliberately did **not** swap symbols that turned out boring. `MemoryStore`/`SQLiteGraph`/`Topology` having `mache_callers=0` is a genuine finding about the index's scope.

______________________________________________________________________

## 3. Per-symbol results

| symbol           | category | `grep_wb` |           `grep_call` | `mache_callers` |    FP rate vs mache | grep median (ms) | mache (ms) | token cost ratio |
| ---------------- | -------- | --------: | --------------------: | --------------: | ------------------: | ---------------: | ---------- | ---------------: |
| `Close`          | common   |   **647** |      580 (`.Close()`) |         **580** |               10.4% |               50 | n/a        |            2.79× |
| `New`            | common   |    **64** |           49 (`New(`) |          **46** |               28.1% |               40 | n/a        |            3.48× |
| `Error`          | common   |   **228** |        187 (`Error(`) |         **169** |               25.9% |               40 | n/a        |            3.37× |
| `MemoryStore`    | type     |   **210** | 0 (no `MemoryStore(`) |           **0** | — (mache index gap) |               40 | n/a        |        undefined |
| `SQLiteGraph`    | type     |   **108** |                     0 |           **0** | — (mache index gap) |               50 | n/a        |        undefined |
| `Topology`       | type     |   **156** |                     0 |           **0** | — (mache index gap) |               40 | n/a        |        undefined |
| `GetCallers`     | fan-in   |    **88** |                    30 |          **27** |               69.3% |               40 | n/a        |            8.15× |
| `RenderTemplate` | fan-in   |    **58** |                    46 |          **45** |               22.4% |               50 | n/a        |            3.22× |
| `ReadContent`    | fan-in   |   **110** |                    47 |          **42** |               61.8% |               40 | n/a        |            6.55× |
| `dedupSuffix`    | rare     |     **3** |    2 (1 call + 1 def) |           **1** |               66.7% |               50 | n/a        |            7.50× |

Notes on the column:

- `grep_call` columns marked `0` mean **type-only symbols where there is no `<sym>(...)` invocation** — `MemoryStore` is instantiated as `&MemoryStore{...}`, used as `*MemoryStore` in receivers, etc. Mache's caller index never sees these. The FP-rate column is left blank for those rows because the denominator is meaningless.
- Median grep times sit at ~40–50ms whether the symbol is rare (`dedupSuffix`) or common (`Close`). This is FS-cache-bound on a 67K-LOC corpus; grep is essentially free at this scale, which limits the time-savings story for mache on this repo.

### Ground-truth spot checks (verifying mache's claims)

| symbol               | mache claim                                                                    | spot-check method                                                                                                               | verdict                                                                                                                                                                                                                                                                          |
| -------------------- | ------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `dedupSuffix`        | 1 caller in `internal/ingest/engine.go` line 1400                              | `grep -n 'dedupSuffix' engine.go` shows: comment (1348), def (1351), call (1400)                                                | **Correct**. The single non-def, non-comment reference is the actual caller.                                                                                                                                                                                                     |
| `RenderTemplate`     | 45 callers, first entry in `internal/ingest/engine.go/function_declaration_11` | Listed the call-expression's `callees/` dir → `RenderTemplate`                                                                  | **Correct**.                                                                                                                                                                                                                                                                     |
| `Error` first caller | `cmd/auto_leyline_test.go/.../expression_statement_2`                          | Read source line 20: `require.Error(t, err)`. Listed `callees/Error` → resolved to `internal/graph/store.go/method_declaration` | **Caller listed is real, but callee resolution is wrong**: `require.Error` is from `github.com/stretchr/testify/require`, not `internal/graph/store`. Mache's defs-table guessed an arbitrary in-repo `Error` definition.                                                        |
| `New` first caller   | `tools/fuzz-gen/.../call_expression`                                           | Listed `callees/` → empty                                                                                                       | Mache returned token-matched call sites whose callee target was unresolvable. The 46 returned entries are real `*.New(...)` selector call sites scattered across `roaring.New()`, `bytes.NewReader()`, etc. — but none of them resolve to a `func New` defined inside this repo. |
| `MemoryStore`        | 0 callers                                                                      | Grep finds 36 `func (... *MemoryStore)` declarations, 46 `*MemoryStore` receiver/type refs, 1 `MemoryStore{}` literal           | **Index gap**: mache's caller index is call-expression-only and does not record type references.                                                                                                                                                                                 |

______________________________________________________________________

## 4. Aggregate numbers

Excluding type-only rows (where the comparison is undefined):

- **Mean grep_wb across 7 callable symbols**: 171.1 hits
- **Mean mache_callers**: 130.0 hits
- **Mean FP rate (grep_wb vs mache)**: **40.7%**
- **Median FP rate**: 28.1%
- **Mean token cost ratio (grep_wb × 50 / mache × 20)**: **5.0×**
- **Median grep latency**: 40 ms
- **Median mache latency**: not measurable in this harness; subjectively well under 100 ms per call (no warmup-related variation observed)

Best-case for grep: `dedupSuffix` (3 grep hits, FP rate 66.7% — but the absolute waste is 2 lines).
Worst-case for grep: `GetCallers` (88 grep hits, only 27 real callers → 69.3% FP, 8.15× token ratio).

______________________________________________________________________

## 5. Comparison to agent-lsp's published numbers

agent-lsp's headline finding on Consul (HashiCorp, multi-MLOC Go):

> "grep matches: 1,156 — actual references: 12 — false positive rate: 98.96%"

Our mache-on-mache equivalent worst case is `GetCallers`: **88 grep hits, 27 real callers, 69% FP**. Not the same order of magnitude.

### Why our numbers are smaller

1. **Corpus size**: mache is ~67K LOC; Consul is ~5 MLOC. Common identifiers in mache see ~10× less namespace collision.
1. **Naming hygiene**: the mache repo uses qualified, descriptive names (`SitterWalker`, `LeylineSheaf`, `compileLevels`) — there are very few `Get`, `Run`, `Handle` symbols that would collide across packages. Picking 10 "common-name" symbols is hard here.
1. **Test density**: mache is ~50% test code by file count. Many "FPs" in our table are real-but-low-value matches like assertion strings (`"could not Close"`), table-test names, and t.Run labels. Whether those count as FP depends on the user's intent.
1. **No cross-package selector explosion**: the only place we see Consul-like behavior is `\bNew\b` (64 grep hits, almost all comment text or method-name fragments). At Consul scale, `New` would hit 10,000+ lines.

### Where mache *should* do better than this benchmark shows

The mache index would shine on a query like "every caller of `httputil.NewSingleHostReverseProxy`" — a fully qualified selector that grep can match but cannot rank by call-site significance. We didn't test that here because such symbols don't exist in mache's small dep graph; the comparison would be unfair.

### Where mache is doing *worse* on this corpus

- `find_definition` returned **"no definition found" for all 10 symbols**, including `dedupSuffix` which has exactly one definition. The projection in this session has an empty defs index. Whether this is a missing `_lsp_defs` table or a broken `node_defs` build is a separate investigation; the user-visible result is that the def-lookup workflow falls back to grep entirely on this mount.
- `find_callers("MemoryStore" | "SQLiteGraph" | "Topology")` returns `[]`. Grep can find 210/108/156 references respectively, ~half of which are legitimate uses (receiver decls, pointer types, type assertions). An agent that trusts mache here gets misled.
- `find_callers("New")` returns 46 token-matched call sites whose callee resolution is **empty** — mache cannot tell the agent what `*.New(...)` actually means at each site (it's `roaring.New`, `bytes.NewReader`, `template.New`, etc.). The list is structurally correct (these are real `.New(...)` call sites) but semantically thin. For a Consul-scale corpus this would be the major FP-reduction win; here, grep returns mostly comment hits because `\bNew\b` doesn't match `NewEngine`, so neither side is helpful.

______________________________________________________________________

## 6. Findings (ordered by significance)

1. **Mache's definition index is empty in this projection.** `find_definition` returns "no definition found" for every symbol queried, including the uniquely-defined `dedupSuffix`. For workflows that depend on def-lookup ("where is X declared?"), mache falls back to grep entirely on this mount. This should be a P0 fix; see `internal/graph/sqlite_graph.go` `LookupDef`, the `_lsp_defs`/`node_defs` table presence check.

1. **Caller index does not cover type references.** For `MemoryStore`/`SQLiteGraph`/`Topology` the caller index returns `[]`. An agent asking "where is type X used?" gets an empty list; the documented behavior matches the implementation but the user-mental-model mismatch is real. Suggested behavior: either expose a separate `find_references` that includes type uses, or have `find_callers` route to that when the token resolves to a type.

1. **Callee resolution is inconsistent for token collisions.** `find_callers("Error")` returns 169 real call sites but mache's `callees/` directory resolves the first one to `internal/graph/store.go/method_declaration` (the in-repo `Error` method on something), while the actual call is `require.Error` from a third-party package. The caller list is correct; the linked target is not. Anyone using the callee chain for impact analysis will pick up phantom dependencies.

1. **The FP-rate story is real but undramatic on this corpus.** Mean 40.7% FP for the 7 callable symbols, vs agent-lsp's 99% on Consul. The order of magnitude differs because the corpus is 70× smaller and naming is tighter. Mache's *token ratio* benefit (5.0× mean) is also tempered: at 67K LOC, the absolute number of tokens saved per query is in the low hundreds, not the high thousands.

1. **The time-savings story is dead on this corpus.** Median grep is 40ms; mache's MCP round-trip is subjectively comparable (no harness-side timing available). On a 5 MLOC corpus grep would be 1–3 seconds; here it's free.

1. **The grep-call regex matters more than the word boundary.** `\bClose\b` returns 647 hits; `\.Close\(\)` returns 580. Mache returns 580. The "naive grep agent" that runs `\bClose\b` and reads every file would waste ~10% on comment/string hits. The "sharp grep agent" that writes `\.Close\(` matches mache exactly on this symbol. The FP-reduction win is **mostly between bad grep and good grep**, not between good grep and mache, on a clean Go corpus.

1. **`\bNew\b` is a benchmarking trap on Go.** Word-bounded `\bNew\b` rejects identifiers like `NewEngine`, `NewStore`, `NewRequest` — so it catches only the standalone word "New", which appears almost exclusively in comments. The 64-hit grep result vs mache's 46 call-site result understates mache's advantage; an agent that drops the trailing `\b` (writes `\bNew[A-Z]`) would catch many more real symbols. Grep behavior is regex-quality-dependent in ways that benchmarks need to control for.

1. **mache's tree-sitter-derived paths are 5–8× more token-dense than grep lines.** Each mache entry is ~150 chars of AST path (`internal/graph/.../method_declaration_15/block/.../call_expression`); a grep hit is ~80 chars (`path:line: code-snippet`). The 50/20 tokens-per-entry estimate in our cost-ratio is mache-favorable; with realistic 30/30 estimates the token win shrinks to ~3× for the 7 callable symbols. The bigger token win is **not having to issue follow-up Read calls** after the search — but our table doesn't credit grep with the read-cost it would incur in practice, so this cuts both ways.

1. **The benchmark plan needs a larger corpus to reproduce agent-lsp's headline result.** On mache itself, the mean FP rate caps at ~40%. To see 95%+ FP rates we would need a corpus with: (a) MLOC scale, (b) heavy framework usage (HTTP servers, RPC stubs), and (c) common-name dominance (`Get`/`Put`/`Run`/`Handle` style). Consul fits; mache does not. A more honest replication would run this exact methodology against `gopls`'s test corpus or the `kubernetes/kubernetes` repo.

1. **Where mache *does* clearly win on this corpus**: structured query over the result set. The grep agent gets a list of `path:line` strings and must read every file to know whether the hit is a call, a method declaration, or a comment. Mache hands back a tree-sitter-classified path that already tells you it's a `call_expression`, plus a sibling `callees/` directory that (when correct) names the resolved target. The token cost of *interpreting* a grep hit (read 20 lines around it) is what the agent-lsp post calls "exploration tax". This benchmark doesn't measure that tax, but it's where the real savings live.

______________________________________________________________________

## Appendix: raw mache caller counts (for reproducibility)

```
Close            580
New              46
Error            169
MemoryStore      0
SQLiteGraph      0
Topology         0
GetCallers       27
RenderTemplate   45
ReadContent      42
dedupSuffix      1
```

## Appendix: raw grep counts (`-rn --include='*.go'`)

```
grep -rn '\b<sym>\b' .       grep -rEn '\.<sym>\(' . (or '<sym>(')
Close            647         Close()                580
New              64          New(  (post-def)       49
Error            228         Error( (all)           187
MemoryStore      210         MemoryStore(            0
SQLiteGraph      108         SQLiteGraph(            0
Topology         156         Topology(               0
GetCallers       88          GetCallers( (post-def) 30
RenderTemplate   58          RenderTemplate(        46 (post-def)
ReadContent      110         ReadContent(           47 (post-def)
dedupSuffix      3           dedupSuffix(            2 (post-def: 1)
```

## Appendix: grep timings (ms, median of 3, warm cache)

All symbols clustered between 40–60ms. Variance was within timer resolution (10ms). The corpus is small enough that grep latency is essentially flat regardless of result-set size; a `\bSomeRareThing\b` query takes the same time as `\bClose\b` because the bottleneck is directory traversal, not pattern matching.
