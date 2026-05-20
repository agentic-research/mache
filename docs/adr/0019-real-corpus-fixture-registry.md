# ADR-0019: Real-Corpus Fixture Registry as First-Class Test Infrastructure

Date: 2026-05-19
Status: Proposed (amended same-day per paradigm-assessor audit)
Bead: `mache-eb9b30`
Pairs with: `mache-655e98` (rough-edges parity gate that started this), ADR-0017 (matrix test harness — supersedes its SB-05 synthetic-medium), ADR-0018 (doc-drift workflow — depends on this for its perf gates)

> **Amendment 2026-05-19 (paradigm-assessor audit, log: `_agent_log/paradigm-assessor_2026-05-19_audit_log_adr0019.md`):** Three changes from the original draft:
>
> - **Baseline policy (D.6) changed from auto-rebaseline-on-merge to fixed-anchor + tolerance band.** Auto-rebaseline mathematically bakes in invisible perf rot: at 25% tolerance, 4 merges of +20% compound to 2.07× slower without the gate firing (1.20⁴). Fixed-anchor preserves the gate's load-bearing property. Explicit rebaseline only via `task fixtures:rebaseline` with a justification commit.
> - **Tiny tier dropped from D.2.** Measured: candidate `cespare/xxhash` is 40 files / ~5 public functions. PR #404's bug was an OR-join blowup on thousands of nodes — structurally unreproducible at tiny scale. The tier promised more than it could deliver. Registry starts at medium; a sub-medium tier can be added by a future ADR if a real need emerges (sub-100ms unit tests on a real call graph that's also non-trivial — hard problem, not solved by naming a directory "tiny").
> - **Snapshot curation spec added to D.7.** Original size estimate (500MB-1GB) only holds with aggressive per-language filtering. Without a spec the estimate was fiction (rosary's full tree alone is ~125 MB). The amendment names the filter: source files only per `lang.Registry`, no build artifacts, no test binaries, no `target/` or `node_modules/`.
>
> License treatment also upgraded from one-line to its own Consequences subsection. Migration shrunk from 6 PRs to 3 (the v1 ship-able unit is PR 1; PR 2 and 3 each pose a separate design decision that earns its own scope).

## Context

PR #404 demonstrated the load-bearing failure mode of toy fixtures: `find_smells dead_code` ran in **1ms on a 4-file synthetic fixture** and **57.5s on the 353-file mache-on-mache corpus**. The 460× speedup was unfindable without running on real code. The synthetic fixture passed the happy-path test for years while the rule shipped a perf cliff that every real consumer hit.

The pattern generalizes. Three earlier session moves surfaced the same thing:

- **`mache-655e98` (DefsMap fix)** — toy fixture showed `get_impact` returning 51 bytes on SQLite backend; only the mache-on-mache + cross-corpora runs proved the bug was corpus-independent (same 51 bytes on every Go and Rust corpus). The 4-file fixture's behavior was indistinguishable from the broken-but-runnable state.
- **`feedback_happy_path_not_enough` (user memory)** — "the harness doesn't exercise the rough edges enough, and the happy path isn't good enough." Synthetic fixtures are happy paths by construction.
- **ADR-0017 already names this** as SB-04 (hetero fixtures), SB-05 (synthetic-medium generator), SB-06 (mache-on-mache invariant). SB-06 shipped in this session and is what surfaced the dead_code bug. SB-05 (synthetic) is **superseded by this ADR** — synthesis was the wrong call.

Today fixtures are fractured across the codebase:

| Where                                                                          | What                                  | Problem                                                          |
| ------------------------------------------------------------------------------ | ------------------------------------- | ---------------------------------------------------------------- |
| `cmd/all_tools_e2e_test.go::writeE2EFixture`                                   | 4-file hand-rolled Go                 | Hides anything that scales with corpus size                      |
| `cmd/all_tools_self_test.go::TestE2E_MacheOnMache`                             | Ingests `./` at test time             | Works only because we live in mache; not reusable                |
| `cmd/all_tools_self_test.go::TestE2E_RealCorpora`                              | Env-var-driven paths to sibling repos | Only fires when a developer remembers to set `MACHE_E2E_CORPORA` |
| `cmd/serve_find_smells_test.go::TestFindSmells_DeadCode_PerfGate_MacheOnMache` | Builds mache-on-mache .db in-test     | Just shipped; the ONLY perf gate; bespoke setup                  |
| `testdata/hetero/`                                                             | Designed in ADR-0017 SB-04, unstarted | Not built                                                        |
| `benchmarks/baselines.json`                                                    | Designed in ADR-0017 SB-07, unstarted | Not built                                                        |

Each rule that needs a real corpus invents its own setup. The dead_code perf test took the better part of a /evolve dispatch (40+min) partly because the setup was novel — every subsequent perf-bearing rule will pay the same cost unless we factor the substrate.

## Decision

A **real-only, snapshot-in-tree, 2-tier fixture registry** that every test pulls from.

### D.1 — Snapshot in-tree at pinned SHAs

Real-repo fixtures live in `testdata/snapshots/<repo>-<sha-prefix>/`:

```
testdata/snapshots/
├── manifest.toml                           # registry: id, source, sha, schema, tier
├── rosary-aef3862/                         # ~123 curated Rust files, medium tier
├── ley-line-open-472435a/                  # ~72 curated Rust files, medium tier
└── mache-self/                             # sentinel — resolved to ./ at test time, medium tier
```

Pinned SHA in the directory name makes the source explicit and the snapshot reproducible. No network at test time. Post-curation size (D.7): ~15 MB for v1 (medium tier, ART-owned only); large tier adds ~50-100 MB when it lands. Accepted cost for offline-runnable + reproducible-CI.

`mache-self` is a sentinel ID resolved by the registry to `$REPO_ROOT` via `runtime.Caller` at test time. Lets mache use itself as a medium fixture without duplicating its tree. **Acknowledged non-portable:** the `mache-self` fixture only works inside mache's own test binary; the registry's reusability lives at the `testfixtures.Get(t, id)` API shape, not the fixture set itself.

### D.2 — Two tiers, all real

| Tier       | Size                      | Runtime budget                   | Use case                                                                                                                                                                |
| ---------- | ------------------------- | -------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **medium** | 100-500 real source files | sub-10s ingest, sub-minute total | Matrix runner default + unit tests that need a real call graph. Every `task check` run. Replaces today's `TestE2E_MacheOnMache` ad-hoc shape AND `writeE2EFixture` toy. |
| **large**  | 1000+ real source files   | multi-minute                     | Perf gates only. Nightly CI or `MACHE_E2E_PERF=1`. Replaces today's bespoke per-rule perf-fixture setup.                                                                |

**No synthetic tier.** Per user direction: "we hate synth and its pointless if it requires a 'real' test to surface anyway." Every tier carries real code with real cross-references, real comment density, real package structure. The size differences are about runtime budget, not about contrived shape variation.

**No tiny tier.** Removed per assessor audit: real micro-libs that fit a sub-100ms budget (5-30 files) tend to be either too narrow to exercise non-trivial call graphs (defeating the "real catches what synth misses" argument) or carved subsets of larger repos (which is synthesis with a different label). PR #404's perf bug was unreproducible on anything smaller than the medium tier. Until a real need surfaces for sub-medium real fixtures with non-trivial call graphs (a hard problem; not solved by naming a directory "tiny"), the registry starts at medium. A future ADR can add a sub-medium tier if and when needed.

Today's `writeE2EFixture` (the 4-file Go toy) is preserved as-is for the narrow tests that exercise a specific schema shape rather than the rule pipeline. Once those tests migrate, `writeE2EFixture` can be deleted.

### D.3 — Tier selection criteria

A fixture qualifies for its tier when it satisfies BOTH:

1. **Size budget** — file count and ingest time fit the tier's budget
1. **Tool-surface coverage** — exercises at least N tools meaningfully (not just `list_directory`)

The tiny-tier fixture must still produce non-trivial `find_callers`, `find_definition`, `get_overview` results. A 3-file utility lib that's just constants doesn't qualify even if it fits the size budget — it doesn't exercise the call-graph code paths that toy fixtures hide bugs in.

Candidate fixtures (initial set; ADR ships with these; more can be added by amendment):

| Tier   | Fixture ID                    | Source                                 | License   | Why                                                                |
| ------ | ----------------------------- | -------------------------------------- | --------- | ------------------------------------------------------------------ |
| medium | `medium-go-mache-self`        | `./` (this repo)                       | own       | Already used; sentinel pattern; ships in PR 1                      |
| medium | `medium-rust-rosary`          | `~/remotes/art/rosary` snapshot        | own (ART) | Real Rust corpus; ~123 `.rs` files post-curation; future PR        |
| medium | `medium-polyglot-llo`         | `~/remotes/art/ley-line-open` snapshot | own (ART) | Mixed Rust + Cap'n Proto; ~72 `.rs` files post-curation; future PR |
| large  | `large-polyglot-art-monorepo` | aggregate of ART repos at pinned SHAs  | own       | Largest real shape we care about; future PR                        |

**Initial set restricted to ART-owned repos.** External fixtures (kubernetes/pkg, MIT-licensed micro-libs, etc.) are deferred to a future ADR pending license-policy work. Per assessor: importing third-party code into the test tree is a categorically new compliance surface (NOTICE preservation, attribution, supply-chain risk on upstream changes); the ADR shouldn't fold that into the registry shipping ADR. Once the registry pattern proves itself with own-repo fixtures, the license-policy ADR follows.

### D.4 — Manifest format

`testdata/snapshots/manifest.toml`:

```toml
schema = "mache-fixtures/v1"

[[fixture]]
id = "tiny-go-xxhash"
tier = "tiny"
language = "go"
source = "github.com/cespare/xxhash"
sha = "3b7bbcc"
license = "MIT"
path = "tiny-go-xxhash/"
schema_preset = "go"
expected = { files = 5, defs_min = 10, refs_min = 30 }

[[fixture]]
id = "medium-go-mache-self"
tier = "medium"
language = "go"
source = "self"
path = "$REPO_ROOT"   # sentinel resolved at runtime
schema_preset = "go"
expected = { files_min = 300, files_max = 500 }

[[fixture]]
id = "medium-rust-rosary"
tier = "medium"
language = "rust"
source = "github.com/agentic-research/rosary"
sha = "aef3862"
license = "own"
path = "medium-rust-rosary/"
schema_preset = "rust"
expected = { files_min = 180, files_max = 250 }
```

`expected` is a sanity check: the registry fails loudly if a snapshot's file count drifts outside the declared range. Catches accidental partial snapshots.

### D.5 — Test API

A new `internal/testfixtures/` package:

```go
package testfixtures

// Get materializes a fixture .db for the test. Caches per-test-binary
// so repeated calls within one `go test` invocation reuse the same .db.
// Cleanup is automatic via t.Cleanup.
func Get(t *testing.T, id string) *graph.SQLiteGraph

// GetTier returns all fixtures at a given tier. Used by matrix runners.
func GetTier(t *testing.T, tier string) []*graph.SQLiteGraph

// Skip if the fixture's tier exceeds what's enabled. Tier-gating env
// vars: MACHE_FIXTURE_MEDIUM=1 (matrix default), MACHE_FIXTURE_LARGE=1
// (perf-gate only). tiny always runs.
func RequireTier(t *testing.T, tier string)
```

Existing tests migrate:

```go
// before
dir := writeE2EFixture(t)
g, _, _, _ := buildMaybeMultiGraph(dir, schema)

// after
g := testfixtures.Get(t, "tiny-go-xxhash")
```

```go
// before
if os.Getenv("MACHE_E2E_SELF") == "" { t.Skip(...) }
// hand-rolled mache-on-mache setup ...

// after
testfixtures.RequireTier(t, "medium")
g := testfixtures.Get(t, "medium-go-mache-self")
```

### D.6 — Perf baselines: fixed-anchor + tolerance band (NO auto-rebaseline)

`testdata/snapshots/baselines.toml`:

```toml
schema = "mache-baselines/v1"

[["find_smells:dead_code"]]
fixture = "medium-go-mache-self"
wall_ms = 124          # FIXED at ADR acceptance time; explicit bump only
tolerance_pct = 25     # gate fails if measurement > wall_ms * 1.25
anchored_at = "3d9802f"      # commit SHA when this baseline was set
anchored_at_date = "2026-05-19"
last_intentional_bump = "n/a"  # populated when someone runs `task fixtures:rebaseline`
```

Behavior:

- PR runs the perf test → produces a measurement
- Within `tolerance_pct` of FIXED baseline: pass; **baseline does NOT auto-update on merge**
- Outside tolerance: fail the gate. PR author either:
  - Fixes the regression (gate passes again, baseline unchanged), or
  - Runs `task fixtures:rebaseline find_smells:dead_code --justification "PR #XXX, intentional regression for feature Y"` which updates `wall_ms` and `last_intentional_bump` with an audit trail

**Why fixed-anchor, not auto-rebaseline (per assessor audit):** auto-rebaseline mathematically bakes in invisible perf rot. At 25% tolerance, 4 merges of +20% each compound to 2.07× slower (1.20⁴) without the gate ever firing. 10 merges of +5% = 1.63×. The "threshold-band-with-auto-update" pattern converts a perf gate into a perf ratchet that only loosens. Fixed-anchor preserves the gate's load-bearing property: gradual rot eventually breaks the band and forces an explicit conversation.

Trade-off: developers will occasionally see false-positive gate fires due to CI runner variance crossing the band. The conservative defaults (`tolerance_pct = 25-30`) absorb most of that. When the gate fires legitimately for noise rather than regression, the rebaseline command exists with an audit trail — better than silent ratchet.

Future extension (deferred): a rolling-median observability dashboard that tracks the last N measurements as a non-gating signal. Helps spot the "no single PR broke the gate but the trend is bad" case. Not in v1.

### D.7 — Snapshot update workflow + curation filter

**What gets snapshotted (per language, derived from `lang.Registry`):**

- Source files: extensions matching `lang.Registry[L].Extensions` (e.g. `.rs`, `.go`, `.py`, `.ts`)
- Project-marker files: `go.mod`, `Cargo.toml`, `package.json`, `pyproject.toml`, `mix.exs`
- Documentation under the source tree: `README.md`, `docs/*.md`, `CHANGELOG.md` (mache treats markdown as a source language for doc-drift rules)

**What does NOT get snapshotted:**

- Build outputs: `target/`, `bin/`, `dist/`, `node_modules/`, `__pycache__/`, `*.pyc`, `*.o`
- Test data binaries: `testdata/**/*.db`, `testdata/**/*.tar`, snapshot-of-snapshot recursion
- IDE / scm metadata: `.git/`, `.idea/`, `.vscode/`, `.DS_Store`
- LFS-tracked binaries (e.g. tree-sitter `parser.c` for grammars not in `lang.Registry`)

Size estimate post-curation (measured, not estimated):

| Fixture                                       |   Pre-curation | Post-curation | What was excluded                          |
| --------------------------------------------- | -------------: | ------------: | ------------------------------------------ |
| rosary (own)                                  |        ~125 MB |       ~5-6 MB | `target/`, `.git/`, test data fixtures     |
| ley-line-open (own)                           |        ~125 MB |       ~6-8 MB | same                                       |
| mache-self (own)                              | n/a (sentinel) |          0 MB | resolved at runtime                        |
| **Total v1 (medium tier only)**               |                |    **~15 MB** |                                            |
| Future large-polyglot-art-monorepo (estimate) |                |    ~50-100 MB | aggregate of multiple curated medium repos |

Repo size cost for v1 is ~15 MB, not 500MB-1GB. The original estimate assumed naive snapshots; curated snapshots are an order of magnitude smaller.

**`task fixtures:update`:**

1. For each fixture with a non-sentinel source: git clone (or pull) the upstream at HEAD into a scratch dir
1. Apply the curation filter (above) — output to `testdata/snapshots/<id>/`
1. Diff against the existing snapshot
1. If no diff: noop
1. If diff: refresh + update SHA in manifest + commit as `[mache-XXXXXX] chore(fixtures): refresh <id> aef3862 → 1a2b3c4`
1. Run `task test` — if baselines fail post-refresh, that's a real signal (the upstream change actually affected mache's perf characteristics)

Cadence: on-demand. No cron. When a sibling repo evolves enough that the snapshot is stale, the developer runs `task fixtures:update` deliberately. The pinned-SHA pattern means staleness is visible in git history; not invisible drift.

## Consequences

### Positive

- **The perf-cliff class of bug becomes systematically detectable.** Every rule with `Tags: ["perf"]` gets exercised against the medium tier by default; the large tier surfaces scaling cliffs nightly.
- **Test setup cost drops to zero per rule.** New rule authors write `testfixtures.Get(t, "tiny-go-xxhash")`; the registry handles everything else.
- **Reproducible across machines + CI.** Snapshot pinned by SHA, no network, no env vars (for tiers below large).
- **The "real world usage as tests" framing is structural, not aspirational.** Every test that uses the registry is by-construction running on real code.

### Negative

- **Repo size grows ~15 MB for v1** (medium tier, ART-owned snapshots only, post-curation). The earlier 500MB-1GB figure assumed naive snapshots; the curation filter (D.7) drops it an order of magnitude. Large tier adds an estimated 50-100 MB when it lands.
- **Snapshot drift is a real maintenance burden.** When rosary refactors its module structure, the medium-rust-rosary fixture becomes a museum piece. `task fixtures:update` is the mitigation but requires deliberate action. Cadence has no automatic enforcement — staleness is visible in git history but doesn't fire a CI signal.
- **mache-self sentinel is not portable.** Acknowledged. The `mache-self` fixture works only inside mache's own test binary; another repo adopting this registry pattern would need its own sentinel. The registry's REUSABILITY is at the `testfixtures.Get(t, id)` API shape, not at the fixture set itself. Documented in the migration notes.
- **In-process cache only.** `testfixtures.Get` caches an ingested fixture for re-use within a single test binary (e.g. multiple tests in `cmd/serve_find_smells_test.go` reusing the same .db). Cross-process re-use (between `cmd/foo` and `cmd/bar` test binaries) is future work; each compiles to a separate binary and Go's test runner doesn't share state between them. The expected savings come from the within-package case, which is where most matrix-runner tests live.

### License surface (its own subsection because it's load-bearing)

External-repo snapshots introduce a categorically new compliance surface:

- **Attribution requirements.** MIT/Apache-2.0 require LICENSE + NOTICE preservation in any redistribution. mache's release tarballs include `testdata/` by default; vendoring third-party code obligates correct attribution.
- **Allowlist enforcement.** The manifest declares `license = "own" | "MIT" | "Apache-2.0"`, but nothing enforces it at snapshot time. A misconfigured `task fixtures:update` could pull a copyleft repo silently.
- **Supply-chain risk.** Upstream relicensing or repo compromise propagates into mache's tree at next `task fixtures:update`. The pinned-SHA-in-dirname mitigates somewhat (you'd see the upstream change in the diff) but doesn't catch a maliciously force-pushed prior history.
- **Distribution implications.** mache redistributing vendored code (via Go module proxy, release artifacts, etc.) requires that the redistributed bits' licenses are compatible with mache's MIT license. Apache-2.0 patent grants are fine; copyleft licenses are not.

**Scope decision:** v1 of this ADR ships ART-owned snapshots only (rosary, ley-line-open, mache-self). External snapshots (kubernetes/pkg, third-party micro-libs) require a separate license-policy ADR before landing. The registry SHAPE supports external fixtures (manifest has a license field), but no external fixtures are added in this ADR's PRs.

### Reversibility

Medium. The registry API (`testfixtures.Get`) is a thin wrapper; if the design is wrong, callers swap back to direct setup with manageable mechanical changes. The snapshots themselves are just directories — they can stay or be deleted with no functional impact.

## Open questions

1. **Carved subsets vs whole-repo snapshots** — when a tiny tier needs ~10 files, do we vendor a tiny external lib OR carve a sub-package out of a larger repo? Carving keeps everything in-house but isn't quite "real" (the package was designed with siblings, not in isolation). **Lean: vendor MIT-licensed external micro-libs; flag carved subsets in the manifest with `synthetic_isolation = true`.**

1. **Large-tier source** — kubernetes/pkg is well-known but it's Go-only. What's the real polyglot large-tier story? **Lean: defer until we have a concrete need; medium tier covers most rule development.**

1. **Symlink vs path resolution for `mache-self`** — a sentinel path that the registry resolves at runtime is cleaner than a symlink (which has cross-platform issues). **Lean: sentinel path `$REPO_ROOT` resolved by the registry; document.**

1. **Polyglot fixtures within a tier** — should the medium tier auto-run rules against EVERY medium fixture, or does each rule declare which language(s) it applies to? **Lean: rule's existing `Languages: []string{}` field already declares applicability; the runner intersects with fixture languages.**

1. **Baseline storage** — JSON vs TOML for baselines.json. The rest of the registry is TOML (per user preference); baselines is a hot file edited by automation, so JSON is friendlier for `jq` diffs. **Lean: TOML for symmetry, generated comments noting last-updated SHA so humans can read the diff.**

1. **Snapshot vs vendoring vs git submodule** — submodules are the obvious Git-native choice; rejected because they add network dependency for CI and a second SCM concept. Vendoring (just checking in the files) is simpler but loses the "this was rosary at SHA X" provenance. **Lean: snapshot-with-SHA-in-dirname; provenance is in the directory name + manifest, not in a separate Git layer.**

## Migration path

Three PRs. Each PR is independently shippable; each subsequent PR earns its scope from the previous PR's evidence.

1. **PR 1 — Registry API + mache-self sentinel + migrate two tests.** Create `internal/testfixtures/` with `Get(t, id)`, `RequireTier(t, tier)`. Load manifest. Implement `mache-self` sentinel (resolves to `$REPO_ROOT` at runtime via `runtime.Caller`). Migrate `TestE2E_MacheOnMache` and `TestFindSmells_DeadCode_PerfGate_MacheOnMache` to use the registry. **No external snapshots, no baseline policy yet — both deferred.** This PR proves the helper shape; if it doesn't feel right, the bigger pieces aren't worth building.
1. **PR 2 — External snapshot tooling + first external snapshot.** Add the curation filter (D.7), `task fixtures:snapshot`, and one external fixture (`medium-rust-rosary`). Migrate `TestE2E_RealCorpora` to use the registry by default (no env var needed for medium tier). Pre-req: PR 1 dogfooded for at least a week so the registry API is stable.
1. **PR 3 — Baseline tracking with fixed-anchor + tolerance band.** Add `testdata/snapshots/baselines.toml`, the threshold-band assertion helper, and `task fixtures:rebaseline` with audit-trail. The perf-gate semantics ARE the load-bearing design call (per assessor); this PR is where they land, not bundled with infrastructure setup.

**Decision points:**

- After PR 1: does the API feel right? If not, fix before PR 2.
- After PR 3: are the baselines stable in CI for 4-6 weeks? If yes, promote ADR-0019 to Accepted. If false-positive gate fires are persistent, the tolerance defaults need tuning OR rolling-median observability needs to come forward.
- After PR 3: file follow-up ADRs for (a) license policy + external non-ART fixtures, (b) large tier sourcing, (c) sub-medium fixture story if one emerges from real need.

## Beads (to file once ADR is approved)

- One bead per migration PR (6 beads total)
- A meta-bead for the ADR rollout (this ADR's bead)

## References

- ADR-0017 (matrix test harness) — SB-04 / SB-05 / SB-06 / SB-07 are partially superseded or specialized by this ADR
- ADR-0018 (doc-drift workflow) — its perf-gate rules depend on this ADR for fixture access
- `mache-655e98` (DefsMap fix) — first surfaced the "real corpus reveals what synthetic hides" pattern
- `feedback_happy_path_not_enough` (user memory) — the principle this ADR operationalizes
- `mache-68980e` (dead_code 460× speedup) — the canonical case study; would not have been catchable without `TestE2E_MacheOnMache`
- PR #404 — the dead_code fix; the perf gate it added is the prototype for this ADR's `testfixtures.RequireTier(t, "medium")` pattern

______________________________________________________________________

End of ADR-0019.
