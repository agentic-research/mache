# Mache benchmarks

This directory anchors mache's benchmarking work. Two categories live here:

1. **Micro-benchmarks** — Go `testing.B` benchmarks that live next to the code they measure (in `internal/...`). This README cross-links the ones that matter for cross-PR comparison and reports their measured baselines.
1. **Comparative bench harness** *(planned, scoped under bead `mache-7937c5`)* — runs mache against peer code-intelligence MCPs (codebase-memory-mcp, Serena, agent-lsp) over a fixed repo corpus + question set. Not yet implemented; lives here once methodology lock-in lands.

## Wire-decode microbench (`internal/leyline/socket_bench_test.go`)

Paired benches that exercise both daemon-response decode paths through a stub UDS daemon: the legacy `SocketClient.SendOp` (decodes into `map[string]any`) and the typed `SocketClient.SendOpInto` (decodes into the structs in `internal/leyline/wire.go`).

Payloads match the v0.3.0 daemon wire shape — Int64 fields as quoted JSON strings (the capnp-json codec adopted in ley-line-open's b0ea2e migration). See PR #372 for the decode-correctness story; this bench is the regression baseline.

### Coverage

| Op            | Payload shape                      | Bench prefix                             |
| ------------- | ---------------------------------- | ---------------------------------------- |
| list_children | 64 child entries (Int64 size each) | `BenchmarkSend{Op,OpInto}…_*Decode`      |
| get_node      | single Node + 27 B record          | `BenchmarkSend{Op,OpInto}_GetNode_*`     |
| read_content  | 8 KiB content string               | `BenchmarkSend{Op,OpInto}_ReadContent_*` |
| find_callers  | 32 Ref entries                     | `BenchmarkSend{Op,OpInto}_FindCallers_*` |
| find_callees  | 16 Ref entries                     | `BenchmarkSend{Op,OpInto}_FindCallees_*` |

Fixture sizes are illustrative middle-of-the-road values, not production-sourced. The 16/32/64 split between callees/callers/children is arbitrary; future work (bead `mache-7937c5`) should re-baseline against real production p50/p95 distributions before claims about "realistic workloads" are made from these numbers.

### Running

```sh
go test -bench='BenchmarkSendOp' -benchmem -run='^$' -benchtime=2s -count=10 ./internal/leyline/
```

For statistically meaningful deltas use `-count=10` and feed both runs to `benchstat`. The headline numbers below come from a single 10-run sample on a M3 Max under low background load; rerun on your target platform before quoting them elsewhere.

### Baseline results (benchstat, n=10, Apple M3 Max, darwin/arm64, Go 1.25.5)

`p` values from a Mann-Whitney U test; `~` means the wall-clock difference is not statistically distinguishable from noise at α=0.05.

| Op            |    Map sec/op | Typed sec/op |     Δ time | sig.    |
| ------------- | ------------: | -----------: | ---------: | ------- |
| list_children |  101.5 µs ±2% | 109.8 µs ±1% | **+8.13%** | p=0.000 |
| get_node      |  6.932 µs ±1% | 6.924 µs ±1% |          ~ | p=0.896 |
| read_content  |  50.64 µs ±1% | 50.48 µs ±1% |          ~ | p=0.393 |
| find_callers  | 35.83 µs ±12% | 34.53 µs ±2% | **−3.62%** | p=0.000 |
| find_callees  |  20.11 µs ±2% | 19.47 µs ±2% | **−3.19%** | p=0.000 |
| **geomean**   |  **30.33 µs** | **30.36 µs** | **+0.09%** | —       |

| Op            |       Map B/op |    Typed B/op |      Δ B/op |
| ------------- | -------------: | ------------: | ----------: |
| list_children |  80.21 KiB ±0% | 62.90 KiB ±0% | **−21.58%** |
| get_node      |  2.927 KiB ±0% | 2.363 KiB ±0% | **−19.25%** |
| read_content  |  46.29 KiB ±0% | 45.99 KiB ±0% |      −0.66% |
| find_callers  |  28.93 KiB ±0% | 17.54 KiB ±0% | **−39.37%** |
| find_callees  | 14.417 KiB ±0% | 8.612 KiB ±0% | **−40.26%** |
| **geomean**   |  **21.44 KiB** | **15.95 KiB** | **−25.61%** |

| Op            | Map allocs/op | Typed allocs/op |    Δ allocs |
| ------------- | ------------: | --------------: | ----------: |
| list_children |         1,716 |           1,455 | **−15.21%** |
| get_node      |            72 |              65 |  **−9.72%** |
| read_content  |            44 |              40 |  **−9.09%** |
| find_callers  |           464 |             332 | **−28.45%** |
| find_callees  |           255 |             187 | **−26.67%** |
| **geomean**   |       **230** |         **188** | **−18.25%** |

### Reading the table

The honest one-line summary: **wall-clock is essentially unchanged overall (geomean +0.09%, p>0.05); memory drops materially across the board (geomean −25.6% bytes, −18.3% allocations).**

Per-op:

- **`list_children`** is the one significant wall-clock regression (+8.13%, p=0.000). 64 children × 6 fields = 384 fields decoded; that's where reflection cost over `json.Unmarshal` into a typed slice shows up. Memory drops 21.6% so the trade is real, not free.
- **`get_node`** and **`read_content`** are statistically indistinguishable on wall-clock (p=0.896, p=0.393). The README's earlier "+0.7%" and "+0.2%" framing was inside noise; these ops should be reported as flat.
- **`find_callers`** and **`find_callees`** are modestly faster (−3.6%, −3.2%) and substantially lighter on memory (~−40%). The `map[string]any` path boxes every JSON value as an `any`; the typed path stuffs them into struct fields directly. On Ref-list payloads the boxing cost dominates.

### Net read

PR #372's typed-decode is a wash on wall-clock and a clean memory win. The marketing in the previous revision of this README ("net win across realistic workloads") was overclaiming — the only defensible "win" headline is the memory/allocation one.

What this bench does NOT prove:

- p99 latency in production (uses a localhost stub, no kernel/IPC contention)
- Performance on linux/amd64 (M3 Max numbers don't transfer cleanly)
- That fixture sizes reflect real workloads (16/32/64 entry counts are arbitrary)

The `list_children` reflection overhead is bounded by field count. If it ever shows up in production p99, the fix is field-level cleanup (drop unused `*string` pointers, replace with `string` where capnp's nullability isn't load-bearing) rather than reverting to map decode.

## Comparative bench (planned)

Tracked under bead `mache-7937c5`. Three peer MCPs in scope:

- **codebase-memory-mcp** (DeusData) — tree-sitter + Cypher; published a 12-question × 64-repo PASS/PARTIAL/FAIL grid we can adopt as the navigation-axis methodology.
- **Serena** (Oraios) — live LSP, no published bench; serves as the "live-data" comparator. Initial install / single-repo run sketched in this PR (see "Serena comparison sketch" below).
- **agent-lsp** (Blackwell Systems) — LSP + workflow-enforcing skills; published per-codebase ratio benchmarks (rename, FP rate, speculative-edit) we can adopt as the edit-axis methodology.

The 2-axis question is the meaningful one: how does mache's projected-FS structural view compare on **navigation** (codebase-memory-mcp's strength) AND on **edit-prep** (agent-lsp's strength), measured on the same repo corpus, against all three peers?

### Serena head-to-head harness

See `benchmarks/serena/README.md`. We vendor Serena's MIT-licensed agent-self-eval methodology (one-shot 20-task prompt) and adapt it for mache's read-only surface — categories 2–4 (single-file edits, multi-file changes, edit-reliability) are marked **out of scope** per Serena's own taxonomy. The remaining categories (codebase understanding, workflow effects, non-code reads, free-text search) are exactly where mache's projected-FS + pre-baked LSP claims are testable.

`benchmarks/serena/baselines/serena_cc_opus_4.6_on_tianshou.md` is the published Serena baseline (CC-Opus-4.6 on Tianshou, 26K LOC Python). Mache's hypothesis is that on category 1 (codebase understanding) mache will classify as **strong positive vs built-ins** where Serena classified as *moderate positive* — because navigation is `ls` on a projected directory rather than an MCP call, and the LSP-backed answers are pre-baked into the .db so there's no language-server startup cost. That hypothesis is now testable; running it is the next step on bead `mache-7937c5`.
