# ADR-0018: Doc-Drift Detection as Executable Specs (find_smells First-Class Workflow)

Date: 2026-05-19
Status: Accepted (vocabulary amended post-merge per `mache-ec1a06`)
Bead: `mache-e1b6c8`
Pairs with: `mache-96341d` (rule categorization), `mache-966e22` (external rule pack distribution), `mache-9e03df` (viper+TOML config — *bead, not yet shipped*), `mache-ec1a06` (severity+tags vocabulary, prior-art research)

> **Amendment 2026-05-19 (`mache-ec1a06`):** Prior-art research across ruff/pylint/eslint/clippy/semgrep (full report: `/tmp/prior-art-rule-classification.md`) refined the schema before implementation. Two changes from the original draft:
>
> - **Severity vocabulary:** `block | warn | info` → **`off | warn | error`** (ESLint precedent, three-tier near-universal). `block` was bespoke; `error` aligns with every existing 2026 linter. `info` was non-actionable; `off` matches clippy's `allow` and lets a rule ship-disabled in a pack.
> - **`Stages []string` field dropped.** None of the 5 surveyed tools puts stages on the rule. Replaced with **`Tags []string`** (free-form, capped at 3-5) — stages emerge as CLI profiles from `(--tags × --fail-on)` combinations at invocation, not from a frozen enum baked into the schema. Same agent-native local/CI symmetry, more flexible.
> - **`--fail-on` default** is `error` (not the original `never`). Observability contract is preserved by construction because new rules default to `Severity = "warn"` — no rule ships at error severity unless the author opts in. CI escalates via `--fail-on=warn` (clippy `-D warnings` idiom).
>
> The rest of the ADR (PR roadmap, scope, deferred federation, SDD framing) stands as-is. The implementation PRs ship the amended vocabulary.

## Context

13 repos in the ART ecosystem, all maintained with AI assistance. Documentation drift from code reality is endemic. Reactive "HEY UPDATE DOCS" patterns are at their scaling ceiling. The pattern the user is naming — and that exists in production in cloister's narrow lint scripts — is **spec-driven development applied to maintenance**.

Mainstream SDD ([Spec Kit][1], [Kiro][2], [OpenSpec][3], [Tessl][4], 2025-2026 wave) flows *forward*: spec → generated code → tests derived from spec. The inverse direction is also load-bearing and currently under-tooled: prose docs already exist, code already exists, and the system needs **executable claims** that detect when they've drifted apart.

Cloister demonstrates the pattern in production:

- `scripts/lint-doc-counts.mjs` — README claims "N ADRs"; ground truth is `ls docs/adr/*.md | wc -l`; verify
- `scripts/lint-paths.mjs` — three config files claim the same `/data/do` path; verify they agree
- `scripts/lint-mermaid.mjs` — mermaid blocks reference declared node IDs; verify edges resolve

Each script is a small executable spec. Each fires in pre-commit / CI. Each catches one drift class deterministically. The pattern works.

What mache adds is the **substrate**: cross-language graph projection (markdown sections, code definitions, refs) + a declarative SQL rule engine (`find_smells`) + external rule loading via `MACHE_SMELL_RULES_DIR`. The substrate already exists; what's missing is the *workflow* that treats `find_smells` as a doc-drift gate rather than just observability.

A survey of 4 representative repos surfaced three rule paradigms in production (mache SQL, cloister TS, rosary semgrep) and the related research project `assay` (coverage-based, fuzzy, hard to falsify per the user). The full survey + paradigm analysis is captured in this session's design brief at `/tmp/drift-rules-design.md`; the federation / manifest / scope-lattice / sheaf story belongs in a future ADR when a second consumer demands it. This ADR is the **single-repo, single-paradigm shipping unit**.

## Decision

Promote `find_smells` from observability tool to first-class doc-drift workflow on mache. Concretely:

### D.1 — Treat docs as executable specs

Add 3 starter doc-drift rules to `cmd/smell_rules.go` registry. Each rule encodes one claim shape that, when violated, indicates documentation has drifted from code reality:

| Rule ID                           | Spec the rule encodes                                                                              | Ground truth source                                          |
| --------------------------------- | -------------------------------------------------------------------------------------------------- | ------------------------------------------------------------ |
| `drift_doc_broken_internal_link`  | Every markdown link `[text](relative/path)` resolves to a real file.                               | filesystem (`stat`)                                          |
| `drift_doc_dead_symbol_reference` | Every backtick-fenced token `\`Foo\``in markdown exists in`node_defs\`.                            | `node_defs` table (or repo-local allowlist for stdlib names) |
| `drift_doc_outdated_count`        | Numeric claims in tables (`N MCP tools`) match a SQL ground-truth query the repo author specifies. | per-repo claim→query map in `.mache/drift-counts.toml`       |

Each rule:

- Lives in the existing `SmellRule` struct + registry. Zero new infrastructure.
- Produces (file, line, message, rule_id) findings via the existing SQL→`smellFinding` pipeline.
- Composes with the existing `MACHE_SMELL_RULES_DIR` loader for repo-specific extensions.

### D.2 — Gate-mode for the existing CLI

Add a `--fail-on=<level>` flag to `cmd/find_smells_cli.go`:

- `--fail-on=never` (default; preserves the existing observability contract from `cmd/find_smells_cli.go:59`: *"This tool is observability, not a gate. It never exits non-zero on findings."*)
- `--fail-on=warn` → exit 1 on any finding at severity `warn` or higher
- `--fail-on=error` → exit 1 on any finding at severity `error` (introduce severity field on `SmellRule`; default `warn`)

This makes the same binary usable in both observability mode (status quo) and gate mode (pre-commit / CI) without forking the codebase or changing default behavior. MCP consumers are unaffected (the MCP handler always uses observability mode).

### D.2a — Local-CI equivalence

The non-negotiable invariant: **the exact same rule set, severity, and exit semantics run locally (pre-commit) and in CI**. Agents must not have to reason about "passes locally but fails CI" or vice-versa. Achieved by both invocations using the same binary + same flags:

```bash
# Local pre-commit hook (in .pre-commit-config.yaml)
mache find_smells --rule 'drift_doc_*' --stage=pre-commit --fail-on=block --format=ci

# CI workflow step
mache find_smells --rule 'drift_doc_*' --stage=ci --fail-on=block --format=ci
```

Two orthogonal rule dimensions support this without inventing new vocabulary:

- **`Severity`** — `block | warn | info`. What to do when the rule fires. `block` = exit non-zero with `--fail-on=block`.
- **`Stages`** — `[pre-commit, ci, nightly, on-demand]`. When the rule runs. Same binary; the `--stage=` flag filters the registry to the appropriate slice.

A rule that should always block both locally and in CI declares `Severity = "block"` and `Stages = ["pre-commit", "ci"]`. Any divergence between local and CI verdicts is structurally impossible because both invocations consume the same rule definitions.

Agent-native interface follows from this:

- **Discovery**: `mache find_smells --list --json` returns the full rule registry with severity / stages / requires / description
- **Filtered run**: `mache find_smells --stage=ci --rule 'drift_doc_*' --format=json` returns findings as parseable JSON
- **Re-run failing only** (cheap, on-demand): `mache find_smells --rerun-failed --since=<run-id>` (deferred; not in PR 1)

### D.3 — Wire one pre-commit hook in mache

Add to mache's `.pre-commit-config.yaml`:

```yaml
- repo: local
  hooks:
    - id: mache-doc-drift
      name: mache doc-drift rules
      entry: mache find_smells --rule 'drift_doc_*' --fail-on=warn --format=ci
      language: system
      pass_filenames: false   # rules scan the whole repo's graph
      stages: [pre-commit]
      always_run: true
```

This requires two small companion CLI features:

- `--rule` accepts glob patterns (`drift_doc_*`) — today it's exact match only
- `--format=ci` emits `file:line:col: severity: message [rule-id]` (gh / ripgrep / vale convention)

Both are mache CLI work, file as small follow-up beads.

### D.4 — Dogfood on mache itself for 4-6 weeks

Run the rules in CI. Surface false-positive patterns. Refine rule descriptions, severity defaults, and the `.mache/drift-counts.toml` schema based on real findings. Track findings as beads when they reveal real drift.

### D.5 — Defer

The following are intentionally **out of scope** for this ADR and tracked for a possible future ADR-0019:

- Federated rule registry / paradigm-tagged manifest
- Cross-repo rule packs (`agentic-research/drift-rules` external repo)
- Scope lattice with formal precedence (Repo > User for invariants, etc.)
- Sheaf / presheaf formalization
- Multi-paradigm dispatch (semgrep, shell-runner, cloister-script, etc.)
- `mache config show --resolved --by-source`

Each of these earns its place when a *concrete need* arrives — typically: a second repo trying to consume mache's rules, or a rule that cannot be expressed as `find_smells` SQL. Until then they are premature abstractions. The paradigm-assessor's audit of the original ADR-0018 draft is appended below (`Appendix B`).

## Consequences

### Positive

- Ships in days, not weeks. Existing infrastructure (`SmellRule`, `find_smells`, `MACHE_SMELL_RULES_DIR`) does 90% of the work.
- Dogfoodable on the first repo immediately. Validates the rule shape against real drift.
- Earns the right to federation: when a second repo wants to consume the rules, the design will be informed by 4-6 weeks of actual usage, not speculation.
- The SDD framing gives the work a clean conceptual hook that connects to the broader 2026 industry direction.
- **Local-CI equivalence is structural, not aspirational.** Same binary, same flags, same registry — divergence is impossible by construction. Agents don't have to reason about it.

### Negative

- The user's stated cognitive-load problem covers 13 repos. This ADR addresses 1. The federation story is deferred, which means cross-repo doc-drift remains a manual problem for now.
- Adding `--fail-on` to a tool whose documented contract is "never exits non-zero on findings" is a contract change. Mitigated by `--fail-on=never` being the default (status quo preserved), but worth flagging in the release notes for any external mache consumer.
- The 3 starter rules might not be the highest-value rules. The dogfood phase will surface this; expect rule additions / removals before any external pack ships.

### Reversibility

High. The 3 rules are 3 entries in a Go slice. The `--fail-on` flag is one boolean check at the end of `find_smells_cli.go`. The pre-commit hook is one entry in `.pre-commit-config.yaml`. Rolling back is `git revert` of one commit.

## Migration path

1. **PR 1** — Add `drift_doc_broken_internal_link` rule + test. Smallest meaningful slice.
1. **PR 2** — Add `--fail-on` flag + glob support for `--rule` + `--format=ci`. The CLI plumbing.
1. **PR 3** — Add `drift_doc_dead_symbol_reference` + `drift_doc_outdated_count` rules + tests.
1. **PR 4** — Wire the pre-commit hook in mache. Activates the gate.
1. **PR 5+** — Iterate based on dogfood findings. Add / remove / refine rules.
1. **Decision point** (4-6 weeks in) — if a second repo (cloister? rosary?) wants to consume the rules, write ADR-0019 covering federation. If not, the workflow stays mache-local.

## Open questions

1. **Severity + Stages schema.** Today `SmellRule` has neither. Adding `Severity string` (`block|warn|info`) and `Stages []string` (`pre-commit|ci|nightly|on-demand`) is a small schema change but introduces precedence questions (per-rule default vs CLI flag vs `.mache.toml` override). **Lean: per-rule default + CLI flag (`--fail-on`, `--stage`); skip `.mache.toml` override until viper+TOML lands per `mache-9e03df`. Use `block` / `warn` / `info` and the existing English stage names — no new vocabulary; "tier" and "class" stay un-introduced.**
1. **The `drift-counts.toml` schema** for `drift_doc_outdated_count`. Format:
   ```toml
   ["MCP tools"]
   query = "SELECT COUNT(*) FROM nodes WHERE id LIKE 'cmd/serve_handler_%/source'"
   ["languages"]
   query = "SELECT COUNT(*) FROM nodes WHERE id LIKE 'lang/%/source'"
   ```
   File at `.mache/drift-counts.toml`. Per-repo, checked-in, team-contract.
1. **The `dead_symbol_reference` rule** will fire on stdlib references (`time.Sleep`, `fmt.Println`, etc.) that don't exist in mache's `node_defs`. Need an allowlist mechanism. **Lean: per-repo `.mache/drift-allow.toml` with rule-keyed token allowlist. Defer the inline `// mache:nolint` syntax until a real false-positive cluster appears.**
1. **Cross-repo claim verification** ("see ADR-0042 in repo B"). Out of scope here. Future ADR-0019 territory.
1. **Naming.** `drift_doc_*` prefix is fine. Open to revision once 4-6 weeks of usage reveals the right taxonomy.

## References

- Spec-driven development landscape (2026): [Spec Kit][1], [Kiro][2], [OpenSpec][3], [Tessl][4]
- Cloister narrow-lint precedent: `~/remotes/art/cloister/scripts/lint-*.mjs`, `~/remotes/art/cloister/done-rules/00-lint-passes.json`
- mache existing substrate: `cmd/smell_rules.go`, `cmd/serve_find_smells.go`, `cmd/find_smells_cli.go`, `cmd/smell_rules_external.go`, `MACHE_SMELL_RULES_DIR`
- Paradigm survey (other repos): `~/remotes/art/rosary/.semgrep/rules.yml`, `~/remotes/art/ley-line-open` (no custom rules)
- Complementary research: `~/remotes/art/assay` (coverage-based, fuzzy, hard to falsify per user assessment)
- Related beads: `mache-96341d`, `mache-966e22`, `mache-9e03df`
- This session's design brief (federation / manifest / lattice / sheaf): `/tmp/drift-rules-design.md`
- This session's paradigm-assessor audit (the find-the-lie review that triggered the pivot to this ADR): `_agent_log/paradigm-assessor_2026-05-19_audit_log.md` (see Appendix B)

______________________________________________________________________

## Appendix A — On assay

drift-rules ≠ assay. Both are valid, complementary directions:

| Tool        | Asks                                     | Falsifiability                                                        |
| ----------- | ---------------------------------------- | --------------------------------------------------------------------- |
| assay       | "Did we document this entity?" (recall)  | Fuzzy: "sufficient docs" undefined; mention ≠ correct                 |
| drift-rules | "Is the doc claim accurate?" (precision) | Sharper, but depends on quality of user-authored ground-truth queries |

The paradigm-assessor noted (correctly) that drift-rules' "sharpness" is **narrower scope**, not categorically higher falsifiability — both tools' precision depends on rule quality. drift-rules ships independently. If assay matures, integration is: assay's "undocumented entities" list feeds drift-rules as a per-repo `drift_doc_missing_for_entity` rule when the repo opts in.

______________________________________________________________________

## Appendix B — Why this ADR is smaller than the original draft

An initial draft (preserved at `/tmp/drift-rules-design.md`) proposed a "federated rule registry" with TOML manifests, paradigm-tagged rules, multi-executor dispatch, formal scope lattice, presheaf framing, and cross-repo packs. A paradigm-assessor audit identified 3 fatal issues with that draft (`_agent_log/paradigm-assessor_2026-05-19_audit_log.md`):

1. **The federation bridge did not exist** — step 1 of the migration was "add 3 rules to mache and rename the subcommand." Zero federation surface got exercised. The whole pitch was sketching v3 of a tool whose v1 is "ship some rules."
1. **Pre-commit composition contradicted reality** — `find_smells_cli.go:59` explicitly says "never exits non-zero on findings." The draft's pre-commit story required non-zero exit.
1. **Cited substrate (`mache-9e03df` viper+TOML) did not exist** — that's a bead filed this same session, not established infrastructure. Draft pretended otherwise.

Plus 4 significant issues (Appendix A of the original draft was decoration; pre-commit was over-orchestrated; "Repo > User" lattice was convention without enforcement; the precision/recall framing oversold).

This ADR scopes down to the version that actually holds up: ship 3 rules + a gate flag + a pre-commit hook on mache itself; defer the federation, manifest, lattice, and sheaf framing to a future ADR when a second consumer earns the right to push back on the design. The original brief is preserved at `/tmp/drift-rules-design.md` as future reference.

______________________________________________________________________

End of ADR-0018.

[1]: https://github.com/github/spec-kit
[2]: https://kiro.dev/
[3]: https://openspec.dev/
[4]: https://tessl.io/
