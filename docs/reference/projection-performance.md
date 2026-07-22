---
status: current
covers-version: v0.18.0
last-verified: 2026-07-21
sources-of-truth:
  - internal/ingest/ast_walker_index.go
  - internal/ingest/ast_walker_bench_test.go
audience: [contributors, evaluators]
supersedes: []
---

# Projection performance: the O(n²)→O(n) ASTWalker fix

v0.18.0 removed a per-file quadratic in source projection (bead `mache-4f3840`).
This doc records what was measured, how, and — importantly — what the number is
**not**.

## Result: a growth-class change, not a fixed speedup

The pre-v0.18.0 `ASTWalker` re-scanned every same-kind node in a file for each
node it projected (`ExtractCalls` / `fileAddrRefs`), which is O(n²) in the
per-file node count. v0.18.0 builds a per-file index once and partitions by
node-id prefix — O(n). `internal/ingest/ast_walker_index.go` is the fix;
`ast_walker.go`'s `TuneReadConnForBuild` is a secondary constant-factor win.

Measured on a single Go file of *N* functions × 4 call sites each, projected
with `examples/go-schema.json`, 3 warm runs per point, comparing a binary built
at `3e13737` (pure-Go, pre-index) against v0.18.0 — same pinned leyline both
sides, so this isolates the projection change, **not** the CGO cutover:

| N funcs | nodes/file | before (median) | after (median) | ratio |
| ------- | ---------- | --------------- | -------------- | ----- |
| 125     | 259        | 2.345 s         | 0.115 s        | 20×   |
| 250     | 509        | 9.486 s         | 0.187 s        | 51×   |
| 500     | 1009       | 42.882 s        | 0.353 s        | 121×  |
| 1000    | 2009       | 152.626 s       | 0.774 s        | 197×  |

Node counts are identical before/after at every size — the fix is pure speed,
with zero change to what gets projected.

Power-law fit (log-log least squares, 4 points):

- **before**: `t ∝ n^2.02` — 4.07× time per input doubling. Textbook quadratic.
- **after**: `t ∝ n^0.92` — ~1.9× per doubling. Linear.

## What the number is not

**There is no single "N× faster" figure**, and quoting one is misleading. The
before/after ratio is itself `n²/n = n`, so it **doubles when the file doubles**:
20× at 259 nodes, ~200× at 2009, and unbounded beyond. The widely-quoted "~50×"
(and the ad-hoc "78×" from the original fix session) are just where a particular
file landed on that line — for the repo's `cmd/` package, dominated by a 169 KB
and a 110 KB test file. State it as:

> Projection went from O(n²) to O(n) in per-file node count; observed speedup
> ranged 20×–197× across 259–2009 nodes/file and grows linearly with file size.

**The driver is per-*file* node count, not repo size.** The quadratic was
per-file. A repo of many small files sees little benefit; a repo with one huge
generated/vendored file sees a lot.

## Measurement traps (both cost real time to discover)

1. **Schema matters.** The quadratic only runs under a schema that extracts
   calls/references (`go-schema.json`). The default `_ast` projection does not
   touch `ExtractCalls`/`fileAddrRefs`, so benchmarked against it the fix looks
   like a ~1.1× no-op. The first measurement of this fix reported exactly that
   and nearly concluded the fix did nothing.
1. **Cold vs warm.** The first run of any size pays leyline's cold parse (e.g.
   0.60 s vs 0.06 s warm on a 6 KB dir). Warm both sides or the small-N points
   are dominated by parse, not projection.

## Reproducing

```bash
# before: last pure-Go commit before the index landed
git worktree add /tmp/wt-before 3e13737
CGO_ENABLED=0 go build -C /tmp/wt-before -o /tmp/mache-before .
# after: v0.18.0 (or later main)
CGO_ENABLED=0 go build -o /tmp/mache-after .

# one Go file, N funcs each calling 4 others by name; sweep N; time warm runs:
#   mache build -s examples/go-schema.json <dir> <db>
# compare medians; node counts must match between the two binaries.
```

The maintained in-tree proxy is `internal/ingest/ast_walker_bench_test.go`
(`BenchmarkASTWalker_ExtractCalls` / `ExtractQualifiedCalls`), which sweeps call
counts on the current code; run it with `-benchmem` to watch the per-call cost
stay flat as N grows.

## Not covered here

Agent-facing end-to-end latency (the 6-level capability arena) is a separate
measurement — see [arena.md](arena.md). Its recorded numbers still predate
v0.18.0; a re-run is tracked in `mache-b7ec42`.
