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
