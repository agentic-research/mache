# Design docs

Working design material — the thinking behind a change, kept separate from the
decisions (`../adr/`) and the current-state docs (`../ARCHITECTURE.md`, `../ROADMAP.md`).

| Directory  | Holds                                                                               |
| ---------- | ----------------------------------------------------------------------------------- |
| `specs/`   | Design specs — the problem, the shape of the solution, the trade-offs. Active only. |
| `plans/`   | Task-by-task implementation plans derived from a spec. Active only.                 |
| `archive/` | Specs and plans whose subject has shipped. Kept for provenance, not for reading.    |

## Convention

A spec/plan stays in `specs/` or `plans/` only while it is still driving work. Once the
thing it describes has shipped, move it to `archive/` — a shipped plan left in `plans/`
reads as pending work and is the main way this tree goes stale.

An ADR records a decision that stays in force; a spec/plan records how a specific piece of
work was designed and executed. When a design hardens into a durable decision, write an ADR
and archive the spec.

## Active

- [`specs/2026-07-22-node-properties-props-column-design.md`](specs/2026-07-22-node-properties-props-column-design.md) — de-overload the `record` column: node Properties move to a queryable `props` column (bead `mache-90b89b`). On-disk format change with a hard cutover.
- [`specs/2026-07-02-analysis-substrate-consolidation-design.md`](specs/2026-07-02-analysis-substrate-consolidation-design.md) — the analysis-substrate consolidation thread.
- [`specs/2026-06-24-mache-measurement-contracts.md`](specs/2026-06-24-mache-measurement-contracts.md) — what mache's four public claims mean as measurements. Contract, not harness; implementation is phased.
- [`specs/2026-03-24-art-platform-release-infrastructure-design.md`](specs/2026-03-24-art-platform-release-infrastructure-design.md) — cross-repo ART release infrastructure. Still a draft, and mostly scoped to sibling repos.

`plans/` is currently empty: every plan written so far has shipped and moved to `archive/`.
