---
title: "ADR-0027: One Startup Path — Resolve → Plan → Open, With Capabilities as Ablatable Axes"
status: Proposed
date: 2026-08-18
tags: [architecture, startup, cli, mcp, performance, ablation, observability]
---

**Relates to:**

- ADR-0012 (CGO Removal — why the in-process walker path exists at all)
- ADR-0026 (Arena as Substrate — per-project arenas; proposed, unmerged)
- Bead `mache-40faff` (this ADR's tracking bead)
- Bead `mache-6c9e1d` (nil `SheafInvalidator` on the default serve path)
- Bead `mache-4ae285` (no health surface — becomes tractable only after this)
- Bead `mache-be8090` (the corpus the ablation matrix requires)

## Context

Mache acquires a graph through **seven distinct mechanisms** spread across the CLI and
MCP surface. Measured 2026-08-17:

| Entry                            | Mechanism                                                                | Branches |
| -------------------------------- | ------------------------------------------------------------------------ | -------- |
| `serve` + all MCP                | `buildServeGraph` → `openDBGraph` / `buildControlGraph`                  | 4        |
| `mount`                          | inline in a ~450-line `RunE` closure; reuses nothing from serve          | 4        |
| `mount --control`                | `graph.OpenWritableGraph` (`cmd/mount_control.go:131`)                   | 1        |
| `pack`                           | `graph.OpenSQLiteGraph` (`cmd/pack.go:71`)                               | 1        |
| `find-smells` CLI                | raw `sql.Open` (`cmd/find_smells_cli.go:186`) — bypasses the graph layer | 1        |
| `cache`, `build_meta`, `leyline` | raw `sql.Open`                                                           | 3        |

`buildServeGraph`, `openDBGraph`, and `autoInvokeLeylineParse` appear **only** in
`serve.go`. `mount.go` reimplements the same decisions independently.

### The defect is not duplication

Duplication is the symptom. The defect is that **a capability is a side effect of
dispatch rather than a request**. `buildServeGraph`'s four branches differ in semantics,
not merely in code:

1. control-block / arena → hot-swap, live
1. `.db` → frozen
1. **directory + leyline — the default** → frozen, **nil `SheafInvalidator`, cascade
   disabled** (`mache-6c9e1d`)
1. MemoryStore + Engine + watcher → **the only cascade-capable path**, reachable only via
   `MACHE_NO_LEYLINE=1`, a flag that *also* disables leyline auto-download

Live-edit propagation therefore exists on exactly one path, is not the default, and
enabling it carries an unrelated second effect. Nobody chose that bundle; it fell out of
branch selection.

Mount is not simply "serve plus a mountpoint" either. It needs three things serve does not
express: `EagerScan()` (fuse-t NFS timeouts), `writable` → `OpenWritableGraph` with a
retained `*ingest.Engine` for write-back, and `--out` materialization that terminates
without mounting. Any attempt to make mount call serve's builders as they stand would
either drop those capabilities or smuggle mount-specific flags into serve — deepening the
entanglement this ADR exists to remove.

### Why this is a measurement problem

Four entangled paths mean four **incomparable** benchmarks. "What does the watcher cost?"
is unanswerable today, because the watcher only exists on the path that also swaps the
backend, also skips leyline, and also changes download behavior. Every number is a bundle;
no delta is attributable to a cause.

Contrast the restriction-first measurements (`mache-b37cff`): global cold 44.97 s /
8.37 GB versus scoped cold 0.447 s / 166 MB. That is a clean causal claim precisely
because it was the **same database and same code path with one variable flipped**. No
startup axis can currently be measured that way.

## Decision

Collapse all graph acquisition to a single path:

```
Resolve(source) → Plan{backend, freshness, capabilities} → Open(plan) → Graph
```

- **Resolve** inspects the source and produces a *default* Plan. It decides nothing that
  the caller cannot override.
- **Plan** is a plain struct whose fields are the capability axes below. It is inspectable
  and loggable — this is what `mache doctor` will report (`mache-4ae285`).
- **Open** is the only constructor. Every entry point goes through it.

Expensive behavior is a **declared input**, never an implicit consequence of which branch
matched. A caller that wants the cheap thing asks for it; a caller that wants the watcher
asks for that.

### The axes

Each must be independently settable. The right column is what it is welded to today.

| Axis        | Values                                                      | Currently welded to              |
| ----------- | ----------------------------------------------------------- | -------------------------------- |
| `Backend`   | SQLiteGraph (zero-copy, lazy) \| MemoryStore (materialized) | source type                      |
| `Freshness` | Frozen \| Watched \| ArenaHotSwap                           | whether leyline was auto-invoked |
| `Cascade`   | on \| off (`SheafInvalidator` wired vs nil)                 | backend choice                   |
| `EagerScan` | on \| off                                                   | mount only                       |
| `Writable`  | read-only \| write-back                                     | `mount --control` only           |
| `Embedding` | eager \| lazy \| off                                        | hardcoded lazy-on-first-access   |

`Freshness: ArenaHotSwap` is the **only** value that requires a running ley-line daemon.
Per LLO's `docs/TABLE_CONTRACT.md`, the contract between mache and LLO is the **SQL
projection ABI** — not the `.db` file and not the arena. Making the daemon dependency one
value of one axis states that contract in code rather than in prose.

## Consequences

**Ablation becomes possible.** One corpus, one code path, N axes → a matrix that reports a
per-axis cost delta. Regressions become attributable to an axis instead of "the mount path
got slower."

**A health surface becomes expressible.** Today `mache doctor` would have to enumerate
seven mechanisms to know what to check. With one path, "what is this session configured to
do" is a single readable structure: the doctor reports the Plan.

**`mache-6c9e1d` becomes fixable.** Cascade stops being a consequence of backend selection,
so it can be requested without `MACHE_NO_LEYLINE`'s unrelated side effect.

**Cost: a real migration.** Six call sites move. The utility commands (`cache`,
`build_meta`, `leyline`) may be explicitly exempted — they poke metadata rather than
consuming a graph — but the exemption must be **listed in code**, not assumed, or it
becomes the crack the next bypass grows in.

**Risk: Plan becomes a god-object.** Mitigated by keeping it a dumb struct with no methods
beyond validation, and by requiring every new field to come with an ablation entry. An axis
whose cost nobody measures is a field nobody needed.

## Alternatives considered

**Leave it.** Rejected: the entanglement is already producing wrong defaults
(`mache-6c9e1d`) and unmeasurable performance.

**Fold `mount` into `serve` entirely.** Rejected: they have genuinely different lifecycles
(a mount outlives a request; `--out` terminates without serving). Unifying *acquisition* is
the win; unifying the commands is not.

**Shared helpers, per-command builders.** Rejected: this is what `serve.go` already does
internally, and it does not deliver ablation — helpers still get composed differently per
caller, so the axes stay welded.

## Falsifiability

This ADR is satisfied only when an ablation harness runs one corpus through the axis matrix
on a single code path and reports per-axis deltas in time and allocation. If a capability's
cost cannot be isolated, the refactor did not achieve its purpose, however much code it
deleted. A test must additionally assert the cascade-capable configuration is selectable
**without** setting `MACHE_NO_LEYLINE`.
