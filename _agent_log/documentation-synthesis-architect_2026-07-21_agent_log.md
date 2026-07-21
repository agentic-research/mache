# Documentation Synthesis Architect — docs/ reorg (mache-bb5e77)

**Session start:** 2026-07-21 13:11 (isolated worktree on `worktree-agent-acc04b21d02486c4b`, base ad88255)

## Goal

Execute the mache `docs/` reorganization (bead mache-bb5e77) Phases 2–5 on top of
Phase 1, adopting the cloister ADR front-matter convention. Merge overlapping
competitive docs, normalize 24 ADRs, restructure design docs, write a docs/README
map, fix all internal links, and leave `task docs:lint` + `task smells` green.

## Scope

- `docs/` tree only (plus repo-wide link references to moved paths).
- No PR (parent opens it). Commit to the reorg branch.

## Convention adopted (from ~/remotes/art/cloister/docs/adr)

Front-matter: `title: "ADR-NNNN: <desc>"`, `status:`, `date:`, `tags: [...]`,
then body starting `## Context`. Front-matter title only (no duplicate H1).

______________________________________________________________________

## Phase 0 — branch state check

- Isolated worktree on `worktree-agent-acc04b21d02486c4b` @ ad88255 (== origin/chore/mache-bb5e77-docs-reorg).
- Phase 1 commit ad88255 deleted `docs/smell-debt-baseline-2026-06-24.md` (confirmed gone)
  but did NOT actually bump ROADMAP `covers-version` — still `v0.17.0`.
  **Action:** completing the intended bump to `v0.18.0` (verification criterion requires it).

______________________________________________________________________

## Phase 1 — CONFLICT DISCOVERED, ROADMAP left at v0.17.0 (13:35)

- The Phase 1 commit ad88255 message says "bump ROADMAP to v0.18.0" but only deleted the
  smell-debt snapshot; ROADMAP `covers-version` was never changed (still v0.17.0).
- I initially bumped it to v0.18.0, then discovered `scripts/docs-lint.sh` enforces
  `covers-version == latest CHANGELOG `## [vX.Y.Z]\`\` heading`. CHANGELOG's latest entry is **v0.17.0** — and CRUCIALLY, even the `v0.18.0`git tag ships a CHANGELOG topping at v0.17.0 (verified`git show v0.18.0:CHANGELOG.md\`). So v0.17.0 is the project-wide
  enforced value; both top-level docs (ROADMAP, ARCHITECTURE) sit at v0.17.0 today and
  docs:lint is green.
- **Bumping ROADMAP to v0.18.0 would REDDEN `task docs:lint`** (a hard gate). To legitimately
  move to v0.18.0, a `## [v0.18.0]` CHANGELOG entry must land first (release-doc task, out of
  scope for docs-reorg, and would also force an ARCHITECTURE.md bump for consistency).
- **Decision:** reverted ROADMAP to its committed v0.17.0 state (docs:lint stays green).
  Flagged for the parent — see final report. ROADMAP is now byte-identical to ad88255.

## Phase 2 — merge competitive docs (13:25)

- Created `docs/reference/`.
- Merged `docs/PRIOR_ART.md` (codebase-memory-mcp, lean-ctx, AgentFS, "FUSE is All You Need",
  Dust, Vercel bash-tool, MCP, LangChain/LlamaIndex, AIGNE/AFS, FUSE-DB tools, Plan 9/9P,
  - Piskala/McMillan academic validation) INTO `docs/competitive-landscape-2026.md`
    (Serena, Augment, Cody, Cursor, Continue.dev, Aider, CodeRabbit, Greptile,
    codebase-memory-mcp, stack-graphs) → **`docs/reference/competitive-landscape.md`**.
  * ONE unified matrix (20 tools as rows × 9 capability columns, grouped by AI-tools /
    lineage divider rows). codebase-memory-mcp appears once (kept the fuller entry, folded
    in PRIOR_ART's "closest tool" relationship note).
  * Two "Detailed Analysis" subsections: AI code-intelligence tools (10) + lineage/adjacent (11).
  * Preserved: Academic Validation, Cross-Cutting Themes, Positioning Summary,
    "What Was Wrong in Earlier Versions" (incl. the load-bearing `<!-- docs-lint:ignore -->`
    on the "8 languages" line), and a merged Sources list.
  * Link paths rewritten for docs/reference/ depth: `adr/…`→`../adr/…`, `ROADMAP.md`→`../ROADMAP.md`,
    `../internal/…`→`../../internal/…`, `../cmd/…`→`../../cmd/…`.
- `git rm docs/PRIOR_ART.md docs/competitive-landscape-2026.md`.
- `git mv docs/ARENA.md docs/reference/arena.md`; fixed its 3 internal links (+1 `../` level).

## Phase 3 — ADR normalization (committed 396d781)

- All 24 ADRs now carry `title`/`status`/`date`/`tags` front-matter; redundant H1 removed
  (front-matter title only, per ADR-0014's shape). Body starts at `## Context`.
- Non-lossy rules applied: descriptive status prose -> `> **Status note:** …` blockquote;
  relational metadata (Depends-On/Enables/Bead/Campaign/Relates to/Pairs with/Supersedes/
  Breaking) preserved as a `**Key:** value` block after front-matter.
- Dates for 0006/0007/0008 (no `Date:` field) taken from first-commit dates:
  2026-04-12, 2026-02-14, 2026-02-14.
- Status changes: 0001 Accepted->Superseded (+`superseded_by: ADR-0006`), 0006
  Proposed->Implemented, 0012 Accepted->Implemented, 0023 ->Superseded (own body).
  0014 kept Proposed (untouched — it was already the target shape). 0008 kept Implemented.
- Added `docs/adr/README.md`: status vocabulary + sorted index of all 24 + notes on the
  non-obvious statuses.

## Phase 4 — design docs (committed 8e902a4)

- `docs/superpowers/{specs,plans}` -> `docs/design/{specs,plans}`; added `docs/design/archive/`.
- Archived 10 docs whose subject verified-shipped against the codebase. Beyond the 6 named
  in the brief, I judged 4 more March-era docs as shipped:
  - quotient-graph-design -> `internal/graph/quotient.go` + `get_diagram` tool
  - repo-http-workspace-routing-design -> `cmd/serve_repo.go` + per-session worktrees
  - hosted-index-pipeline (spec + plan) -> `repoCloneCache`, `extractRepoFromContext`,
    `WithHTTPContextFunc` in `cmd/serve.go` + `cmd/serve_hosted_test.go`
- Left active: analysis-substrate-consolidation (per brief), mache-measurement-contracts
  (explicitly "the contract, not the harness"), art-platform-release-infrastructure
  (Status: Draft, cross-repo scope).
- `plans/` ended up empty (every plan shipped) -> added `.gitkeep` + `docs/design/README.md`
  documenting the specs/plans/archive convention.
- Removed the now-empty `docs/superpowers/` directory.

## Phase 5 — docs/README.md

- Wrote the start-here map last, after all moves, so every link resolves.
- Front-matter uses covers-version v0.17.0 (docs:lint checks top-level docs/\*.md).

## Link-fixing

- Repo-wide grep for PRIOR_ART / competitive-landscape-2026 / ARENA.md / docs/superpowers.
- Real markdown links fixed: README.md (1), GETTING-STARTED.md (2 -> merged + arena),
  docs/reference/arena.md (3, deeper path).
- Left alone (correct): historical past-tense filenames inside archived design docs, the
  `superpowers:` *skill* names, and the `supersedes:` provenance in the merged doc's front-matter.
- Ran a full relative-link checker over every .md: 216 links checked. The only 23 failures
  are PRE-EXISTING and untouched by this work (vendored testdata/ + benchmarks/ fixtures,
  a literal `relative/path` example in ADR-0018, docs/audit links pointing outside the repo).
  No moved/renamed file produced a broken link.
