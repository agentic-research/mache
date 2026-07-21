---
status: current
covers-version: v0.17.0
last-verified: 2026-07-21
sources-of-truth:
  - CHANGELOG.md
audience: [contributors, maintainers, users]
supersedes: []
---

# mache docs

Start here. This page is the map — what each document is for, and which one answers
your question. For install and first run, see [GETTING-STARTED.md](../GETTING-STARTED.md)
at the repo root.

## The four questions

| If you want to know…                    | Read                                                                     |
| --------------------------------------- | ------------------------------------------------------------------------ |
| **How does mache work?**                | [ARCHITECTURE.md](ARCHITECTURE.md)                                       |
| **What's landed, what's next?**         | [ROADMAP.md](ROADMAP.md)                                                 |
| **Why is it built this way?**           | [adr/](adr/README.md) — Architectural Decision Records                   |
| **How does it compare to other tools?** | [reference/competitive-landscape.md](reference/competitive-landscape.md) |

## Directories

| Path                          | Purpose                                                                                                                             |
| ----------------------------- | ----------------------------------------------------------------------------------------------------------------------------------- |
| [`adr/`](adr/README.md)       | Architectural Decision Records — 24 of them, each with `title`/`status`/`date`/`tags` front-matter. The index lists status per ADR. |
| [`reference/`](reference/)    | Durable external-facing reference material.                                                                                         |
| [`design/`](design/README.md) | Working design material: active `specs/` and `plans/`, plus `archive/` for shipped work.                                            |
| [`schemas/`](schemas/)        | Per-language schema notes for the projection topologies.                                                                            |
| [`cache/`](cache/)            | Portable-cache (`mache push` / `mache pull`) status and wire-shape notes.                                                           |
| [`audit/`](audit/)            | Point-in-time audits — findings as of a date, not current-state docs.                                                               |

### reference/

- [`competitive-landscape.md`](reference/competitive-landscape.md) — head-to-head comparison against AI code-intelligence tools plus the intellectual lineage (Plan 9, FUSE-DB, RAG frameworks). One matrix, per-tool analysis, and the open gaps.
- [`arena.md`](reference/arena.md) — the capability-arena harness: what each level measures and how to run it.

### design/

- [`specs/`](design/specs/) — active design specs (problem, solution shape, trade-offs).
- [`plans/`](design/plans/) — active task-by-task implementation plans. Currently empty; every plan written so far has shipped.
- [`archive/`](design/archive/) — specs and plans whose subject has shipped. Kept for provenance.

See [design/README.md](design/README.md) for the convention that keeps this tree from going stale.

### Thin subdirectories

- [`schemas/go.md`](schemas/go.md) — the Go source-projection schema.
- [`cache/STATUS.md`](cache/STATUS.md) — portable-cache consumer-side status · [`cache/phase-4-chunk-shape.md`](cache/phase-4-chunk-shape.md) — chunk wire shape.
- [`audit/sheaf-incremental.md`](audit/sheaf-incremental.md) — audit of sheaf-driven incremental invalidation.

## Other files

- `smell-baseline.json` — the committed baseline for the `find_smells` PR gate (`task smells`). Generated, not hand-edited.

## Conventions

- **Top-level `docs/*.md`** carry front-matter with `covers-version` and `last-verified`.
  `task docs:lint` enforces that `covers-version` matches the newest `## [vX.Y.Z]` heading
  in `CHANGELOG.md`, and flags stale mache language-count claims. Subdirectories are not linted.
- **ADRs** record decisions that stay in force; **design specs/plans** record how a specific
  piece of work was designed and executed. When a design hardens into a durable decision,
  write an ADR and archive the spec.
- **Internal links are checked.** `find_smells`' `drift_doc_broken_internal_link` rule runs
  over these docs in `task smells`, so a moved file with a stale link fails the gate.
