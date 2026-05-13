# Mache benchmarks

This directory anchors mache's benchmarking work. Two categories live here:

1. **Micro-benchmarks** — Go `testing.B` benchmarks that live next to the code they measure (in `internal/...`). This README cross-links the ones that matter for cross-PR comparison.
1. **Comparative bench harness** *(planned, scoped under bead `mache-7937c5`)* — runs mache against peer code-intelligence MCPs (codebase-memory-mcp, Serena, agent-lsp) over a fixed repo corpus + question set. Not yet implemented; lives here once the methodology lock-in lands.

## Wire-decode microbench (`internal/leyline/socket_bench_test.go`)

Paired benches that exercise both daemon-response decode paths through a stub UDS daemon: the legacy `SocketClient.SendOp` (decodes into `map[string]any`) and the typed `SocketClient.SendOpInto` (decodes into the structs in `internal/leyline/wire.go`).

Payloads match the v0.3.0 daemon wire shape — Int64 fields as quoted JSON strings (the capnp-json codec adopted in ley-line-open's b0ea2e migration). See PR #372 for the decode-correctness story; this bench is the regression baseline.

### Coverage

| Op            | Payload shape                      | Bench prefix                             |
| ------------- | ---------------------------------- | ---------------------------------------- |
| list_children | 64 child entries (Int64 size each) | `BenchmarkSend{Op,OpInto}…_*Decode`      |
| get_node      | single Node + 30 B record          | `BenchmarkSend{Op,OpInto}_GetNode_*`     |
| read_content  | 8 KiB content string               | `BenchmarkSend{Op,OpInto}_ReadContent_*` |
| find_callers  | 32 Ref entries                     | `BenchmarkSend{Op,OpInto}_FindCallers_*` |
| find_callees  | 16 Ref entries                     | `BenchmarkSend{Op,OpInto}_FindCallees_*` |

### Running

```sh
go test -bench='BenchmarkSendOp' -benchmem -run='^$' -benchtime=2s -count=3 ./internal/leyline/
```

For a single op:

```sh
go test -bench='BenchmarkSendOp.*_FindCallers' -benchmem -run='^$' ./internal/leyline/
```

`-count=3` smooths out scheduler noise; the deltas reported below were stable across runs.

### Baseline results (Apple M3 Max, darwin/arm64, Go 1.25.5)

Two runs at `-benchtime=2s`, results averaged. `Δ` is typed-vs-map relative to map baseline.

| Op            | Map ns/op | Typed ns/op | Δ time | Map B/op | Typed B/op | Δ alloc B | Map allocs | Typed allocs | Δ allocs |
| ------------- | --------: | ----------: | -----: | -------: | ---------: | --------: | ---------: | -----------: | -------: |
| list_children |   101,700 |     106,818 |  +5.0% |   82,152 |     64,382 |    −21.6% |      1,716 |        1,455 |   −15.2% |
| get_node      |     6,835 |       6,881 |  +0.7% |    2,997 |      2,420 |    −19.3% |         72 |           65 |    −9.7% |
| read_content  |    49,197 |      49,316 |  +0.2% |   47,412 |     47,093 |     −0.7% |         44 |           40 |    −9.1% |
| find_callers  |    34,557 |      33,561 |  −2.9% |   29,627 |     17,958 |    −39.4% |        464 |          332 |   −28.4% |
| find_callees  |    20,648 |      18,844 |  −8.7% |   14,762 |      8,816 |    −40.3% |        255 |          187 |   −26.7% |

### Reading the table

- **Throughput (`ns/op`)**: typed-decode is equal or faster on every op except the largest one (`list_children` with 64 children × 6 fields = 384 fields decoded). Reflection cost scales with field count; map decode beats it only when the payload is wide.
- **Memory (`B/op`)**: typed-decode allocates uniformly less, with the biggest wins on Ref-list responses (`find_callers`, `find_callees`). The `map[string]any` path materializes every JSON value as an `any` box; the typed path stuffs them into struct fields directly.
- **Allocations (`allocs/op`)**: typed-decode does 9–28% fewer allocations across the board. The wins compound under sustained traffic (GC pressure, scheduler tail latency) — exactly the thing the wall-clock microbench understates.

### Net read

The typed-decode path that PR #372 lands is a net win across realistic workloads. The only marginally-slower op is `list_children`, and even there the GC-pressure trade (−21% bytes, −15% allocs) is the right call: GC tail latency hurts mache's MCP-tool flow more than 5 µs per call.

The `list_children` reflection overhead is bounded — children fanout is what it is, and most callers walk only a slice of the result. If it ever matters in production, the fix is field-level cleanup (drop unused `*string` pointers, replace with `string` where capnp's nullability isn't load-bearing) rather than reverting to map decode.

## Comparative bench (planned)

Tracked under bead `mache-7937c5`. Three peer MCPs in scope:

- **codebase-memory-mcp** (DeusData) — tree-sitter + Cypher; published a 12-question × 64-repo PASS/PARTIAL/FAIL grid we can adopt as the navigation-axis methodology.
- **Serena** (Oraios) — live LSP, no published bench; serves as the "live-data" comparator.
- **agent-lsp** (Blackwell Systems) — LSP + workflow-enforcing skills; published per-codebase ratio benchmarks (rename, FP rate, speculative-edit) we can adopt as the edit-axis methodology.

The 2-axis question is the meaningful one: how does mache's projected-FS structural view compare on **navigation** (codebase-memory-mcp's strength) AND on **edit-prep** (agent-lsp's strength), measured on the same repo corpus, against all three peers? See bead comments for the methodology breakdown.

Until that work lands, this README is just the wire-decode regression baseline.
