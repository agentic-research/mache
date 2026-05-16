# Sheaf-driven incremental invalidation: end-to-end audit

**Bead:** [mache-49bf9a](../../../mache/.beads) — *"Audit: is sheaf-driven incremental invalidation live end-to-end (or is the daemon side a sketch)?"*
**Decade:** `0013-refs-defs-canonical-schema` · **Thread:** `consumer-curation`
**Author:** session 2026-05-16 · **Status:** audit complete

## TL;DR

| Half of the moat                                                                       | Status                                                                                                                                                                                                                                                       |
| -------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| **Math + cache** (the precision claim — *"we invalidate only what should invalidate"*) | ✓ Pinned daemon-side by LLO PR #16 (8 falsifiability gates + 1 real-repo bench + 2 UDS e2e tests).                                                                                                                                                           |
| **Operational** (the speed claim — *"fresh in \<500ms"*)                               | ⚠ Wired daemon-side; **sketch consumer-side**. Mache pushes topology once per `find_smells` call but never pushes invalidations, never subscribes to `sheaf.invalidate` events, never flushes its own MCP-tool caches, never exposes `generation` to agents. |

The "is this real?" question splits cleanly along the UDS boundary. LLO is real. Mache is wired up to *send* topology and stalk hashes but does not yet *participate* in the invalidation cascade. The pieces all exist (`SheafClient`, `SheafInvalidator`, `SocketClient.Subscribe`); none of them are connected to the file watcher, the MCP cache layer, or the tool surface.

## Per-step audit (mache + LLO, 7 steps)

The original bead asked seven questions. LLO PR #16 gave a daemon-side answer to each ([comment 974c96a5](../../../mache/.beads) on the bead). This table maps the same seven against the **mache-side** code, so the gap list is explicit.

| #   | Step                                                                                                                         | Daemon-side                                                                                                                   | Mache-side                                                                                                                                                                                                                                                                                                                                                                                                       | Verdict |
| --- | ---------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------- |
| 1   | **fsnotify → daemon notification.** Mache sees a `.go` file change and tells the daemon.                                     | ✗ Not in LLO by design — LLO is passive substrate.                                                                            | ✗ **Missing.** `cmd/serve.go:414-421` wires `onChange` to `store.DeleteFileNodes(path)` + `engine.ReIngestFile(path)` — purely local. No call into `SheafClient.Invalidate` or `SheafInvalidator.InvalidateWithCascade`. The daemon never learns a file changed.                                                                                                                                                 | ✗       |
| 2   | **Daemon re-parse.** LLO is told which files to reparse.                                                                     | ⚠ Caller-driven via `op_reparse` (LLO does what it's told).                                                                   | N/A in mache today. Mache has no relationship with LLO's reparse pipeline; mache reingests via its own `engine.ReIngestFile`.                                                                                                                                                                                                                                                                                    | N/A     |
| 3   | **Restriction recompute on stalk change.** A function body changes; restriction edges across community boundaries recompute. | ⚠ Partial in LLO — `sheaf_invalidate` accepts new f32 stalk `data`; topology edges still need `sheaf_set_topology` to change. | ⚠ **Topology pushed; stalks empty.** `internal/leyline/sheaf.go:73-77` calls `sheaf_set_topology` after every `find_smells` (via `cmd/serve_handlers.go:865-879`). But `sheaf.go:96-101` sends `Hash: ""` with the comment *"daemon computes current hash"* — no real f32 stalk data is pushed, so δ⁰ mode never engages on the response.                                                                        | ⚠       |
| 4   | **Region invalidation.** Daemon marks reachable cache entries stale via bounded BFS through restriction edges.               | ✓ Pinned in LLO by `SheafCache::on_change` + 3 falsifiability tests + 2 e2e UDS tests.                                        | ✗ **Never triggered from mache.** `internal/graph/sheaf_invalidate.go` defines `SheafInvalidator.InvalidateWithCascade(id, membership)` — looks up the region for `id`, calls `sheaf.Invalidate`, fans out to all affected nodes. **Zero production callers.** Only references are `internal/graph/sheaf_invalidate_test.go`. The struct exists, is tested, and is dead code from the prod path's point of view. | ✗       |
| 5   | **Notification path: daemon → mache.** Daemon emits `sheaf.invalidate` events; consumers subscribe.                          | ✓ LLO emits `sheaf.invalidate` + `sheaf.topology` on the ADR-010 event bus.                                                   | ✗ **Subscribe primitive exists, never used.** `internal/leyline/socket.go:371` defines `SocketClient.Subscribe(topics)` returning an event channel. `grep Subscribe\\(` across the repo shows the definition and tests — **no production caller subscribes to `sheaf.invalidate`**. The event bus is live; no one is listening.                                                                                  | ✗       |
| 6   | **MCP cache flush.** When mache learns a region is stale, MCP tool results derived from that region are evicted.             | ✗ Mache-side responsibility (LLO doesn't own mache's caches).                                                                 | ✗ **Not implemented.** Mache's per-tool result caches (e.g. `NodesTableReader.SizeCache`, `SQLiteGraph` FIFO content cache, `MemoryStore` FIFO) have `Invalidate(id)` methods (`internal/graph/sqlite_graph.go:992-996`, `internal/graph/nodes_table_reader.go:281-282`). They are evicted on local mutations only. No path connects `sheaf.invalidate` event → `Graph.Invalidate(...)` calls.                   | ✗       |
| 7   | **Agent visibility (staleness signal).** An agent calling an MCP tool can tell whether the answer is fresh.                  | ✓ LLO exposes `generation: uint64` (monotonic, advances on each `on_change`) via `sheaf_status`.                              | ⚠ **Typed wrapper exists; not surfaced.** `internal/leyline/sheaf.go:30-35,132-160` defines `SheafStatus{Generation, Valid, Total, Defect}` and a `Status()` method. **Not exposed via any MCP tool** — there is no `get_sheaf_status` in `cmd/serve_handlers.go` (only `get_overview`, `get_architecture`, etc.). Agents have no way to read generation.                                                        | ⚠       |

## Mache-side artifacts that *do* exist (and what's missing)

What's wired up:

- **`SocketClient.Subscribe([]string) (<-chan map[string]any, error)`** — `internal/leyline/socket.go:371`. Generic event-bus subscribe over UDS.
- **`SheafClient.PushTopology(*CommunityResult, refs)`** — `internal/leyline/sheaf.go:64-85`. Translates Louvain output → `sheaf_set_topology`.
- **`SheafClient.Invalidate(regionID int) ([]int, error)`** — `internal/leyline/sheaf.go:89-111`. Sends `sheaf_invalidate` with an empty-hash stalk.
- **`SheafClient.Defect() / Status()`** — `internal/leyline/sheaf.go:115-160`. Typed wrappers for `sheaf_defect` / `sheaf_status`.
- **`graph.SheafInvalidator.InvalidateWithCascade(id, membership)`** — `internal/graph/sheaf_invalidate.go:51-104`. Glue between `Graph.Invalidate` and `SheafClient.Invalidate`, with cascade fan-out.
- **`PushTopology` call site** — `cmd/serve_handlers.go:865-879`. Fire-and-forget after the `find_smells` MCP tool runs community detection. Once per call; topology never updated otherwise.

What's missing (the gap list, in dependency order):

1. **Watcher → daemon notification (step 1).** `cmd/serve.go:414-421` `onChange` must call `SheafInvalidator.InvalidateWithCascade(...)` (or a thin wrapper that maps `path` → node ID → community) instead of (or in addition to) the local-only `store.DeleteFileNodes` + `engine.ReIngestFile`. Today the chain is severed at the first link.
1. **Subscribe to `sheaf.invalidate` (steps 5, 6).** A single goroutine somewhere in `cmd/serve.go` setup that calls `sock.Subscribe([]string{"sheaf.invalidate", "sheaf.topology"})` and routes invalidated node IDs to `graph.Invalidate`. This is the consumer side of the cascade.
1. **f32 stalk data (step 3).** `SheafClient.Invalidate` and the topology push need to compute and forward stalk values. Bead [mache-8e59a5](../../../mache/.beads) tracks the design call (recommended shape: 32-dim, first 30 = SHA-256-derived agreement coords, last 2 = private, mirroring LLO's bench). Without this, δ⁰ mode never engages and the response from the daemon is the conservative "everything reachable" fallback rather than the surgical "only what crossed the agreement plane."
1. **Typed bindings adoption (step 3 ergonomics).** Hand-rolled `region` / `restriction` / `stalk` JSON structs in `sheaf.go:38-55` get replaced with v0.4.0 typed Go bindings — bead [mache-8e2e92](../../../mache/.beads).
1. **`get_sheaf_status` MCP tool (step 7).** New MCP tool that returns `SheafStatus` (generation, valid/total, defect). Bead [mache-4a0c05](../../../mache/.beads) — already filed under this audit's thread.
1. **Cross-runtime e2e test (gate the whole chain).** Live LLO daemon ↔ live mache `SheafClient`, edit a file, assert `find_callers` reflects it within budget. Bead [mache-8e7794](../../../mache/.beads).

## Where the "is this real or sketch?" question lands

**Daemon side (LLO):** real. The math is pinned, the cache invalidation is pinned, the events fire, the API exists. The 66% parse-time-reduction bench in `tests/real_repo_sheaf_bench.rs` shows the precision claim holds on a real corpus. Falsifiability gates in `rs/ll-open/sheaf/tests/falsifiability_gates.rs` prevent regression.

**Consumer side (mache):** sketch. There is a SheafClient struct, an Invalidator struct, a Subscribe method — but they are not wired together, and none of them are wired into the watcher or the MCP cache. The only live integration today is `PushTopology` fire-and-forget after `find_smells`. So **today**, when an agent edits a file under a mache mount:

- The watcher reingests locally — mache stays internally consistent.
- The daemon is never told.
- `find_callers` results returned via MCP may already have been cached and **stay cached** — agents see stale data with no signal it's stale.
- The δ⁰ moat math runs in LLO over old topology that mache pushed last `find_smells` call; the cascade output, even if it ran, would never reach an MCP consumer.

The architectural moat is real *in principle*. To make it real *in operation*, the six gap items above need to ship — in roughly the order listed, with item 1 (watcher → daemon) and item 2 (subscribe → flush) as the load-bearing pair. Items 3-5 are quality/observability layers on top of that core wiring.

## Related work (the thread this bead unblocks)

| Bead                                  | Priority | What it does                                                                                              |
| ------------------------------------- | -------- | --------------------------------------------------------------------------------------------------------- |
| [mache-4a0c05](../../../mache/.beads) | P1       | `get_sheaf_status` MCP tool — step 7.                                                                     |
| [mache-8f6abf](../../../mache/.beads) | P1       | Strip `_file_level:` sentinels from consumer-facing aggregations (unrelated to sheaf but in same thread). |
| [mache-8fab5d](../../../mache/.beads) | P1       | Distinguish user-defined symbols from external tokens.                                                    |
| [mache-8e2e92](../../../mache/.beads) | P2       | Adopt v0.4.0 typed Go bindings for sheaf ops.                                                             |
| [mache-8e59a5](../../../mache/.beads) | P2       | Opt SheafClient into δ⁰ mode (push f32 stalks).                                                           |
| [mache-8e7794](../../../mache/.beads) | P3       | Cross-runtime e2e test (steps 1-7 end-to-end).                                                            |
| [mache-33ffb0](../../../mache/.beads) | epic     | Pre-injection + warm-cache (downstream of this audit).                                                    |

This audit deliverable closes [mache-49bf9a](../../../mache/.beads). The work items implied by the gap list are filed as the beads above; no new beads are created by this doc.

## References

- LLO PR #16 — sheaf δ⁰ falsifiability gates + cascade implementation: <https://github.com/agentic-research/ley-line-open/pull/16>
- LLO PR #17 — v0.4.0 release with typed Go bindings: <https://github.com/agentic-research/ley-line-open/pull/17>
- LLO daemon source — `rs/ll-open/cli-lib/src/daemon/sheaf_ops.rs`, `rs/ll-open/sheaf/src/cache.rs`
- LLO e2e test — `e2e_sheaf_ops_drive_delta_zero_mode_over_real_uds`
- Bead [ley-line-open-ae7a35](../../../ley-line-open/.beads) — LLO-side sheaf moat work
