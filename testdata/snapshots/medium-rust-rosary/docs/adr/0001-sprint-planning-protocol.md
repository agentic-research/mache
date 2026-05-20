# ADR-001: Sprint Planning Protocol

## Status
Proposed

## Context

Rosary's sprint planning is currently manual: a human + Claude Code session reviews beads, reads ADRs, checks INVESTIGATION_LOGs, decomposes work into parallelizable tasks, dispatches agents, and verifies results. This takes 2-4 hours per session and requires full ecosystem context.

The ART ecosystem has 14+ repos, each with their own planning artifacts:
- `ARCHITECTURE.md` — what it is, feature matrix
- `INVESTIGATION_LOG.md` — what was learned
- `CLAUDE.md` — how to build/test
- `docs/adr/` — decisions made and their status
- `.beads/` — open work items (Dolt-backed)

Sprint planning follows a pattern discovered through practice:

1. **Explore** — parallel scan of ecosystem repos for planning artifacts
2. **Synthesize** — amalgamate findings into unified understanding
3. **Derive** — extract actionable work for next sprint
4. **Decompose** — break into parallelizable, file-per-agent tasks
5. **Dispatch** — launch agents with appropriate permissions
6. **Verify** — check work, close beads, update docs

## Decision

Codify the sprint planning loop as `rsry plan`, which:

### Inputs (read automatically)
- Open beads across all registered repos (`~/.rsry/repos.toml`)
- Phase gate mapping (which beads belong to which phase)
- ADRs from `docs/adr/` across repos (architectural constraints)
- `INVESTIGATION_LOG.md` across repos (recent learnings)
- `ARCHITECTURE.md` across repos (feature matrices, current state)

### Processing
1. **Phase assessment**: which phase are we in? what's blocking the gate?
2. **Dependency analysis**: which beads are blocked by others?
3. **Composition detection**: do any new beads subsume existing ones?
4. **Dedup check**: are any beads saying the same thing?
5. **Parallelization**: group beads by primary file to maximize agent independence
6. **Permission derivation**: map issue_type → PermissionProfile for each task

### Output
A sprint bead (issue_type=epic) with:
- Tiered task list (T1 parallel, T2 depends-on-T1, T3 integration)
- File assignments per task
- Permission profiles per task
- Estimated agent count and concurrency

### Enforcement (from ART methodology)
- Every sprint has a HYPOTHESIS (what we expect to accomplish)
- Every sprint ends with a FALSIFICATION check (did we actually accomplish it?)
- Phase gates are enforced: don't start Phase N+1 until Phase N's e2e test passes
- The e2e test IS the gate (not documentation, not bead count)

## Consequences

- Sprint planning becomes reproducible and delegatable
- New sessions can bootstrap by running `rsry plan` instead of manual review
- The planning process itself is testable (does `rsry plan` produce valid sprints?)
- Agents dispatched by rosary get context from the planning artifacts, not just bead titles

## References

- loom-45n: Codify sprint planning loop
- ART methodology: HYPOTHESIS → FALSIFICATION → v2 (art-meta-project)
- art-hooks: lifecycle completion, sub-agent control, CRUMB integration
- mache ADRs 0001-0009: architectural decision patterns
- ley-line ADRs 001-010: design document patterns
