# Smell-debt baseline — 2026-06-24

First measured snapshot of structural smell debt across the ART repos, the
input the advisory→enforced **ratchet** will eventually grandfather. Produced
with `mache find-smells` (commit `dac8ac5`) over leyline-parsed `.db`s.

> **Headline:** the baseline is **Go-only**. The non-Go repos are not parseable
> by the current tooling, so they are invisible to the smell engine — see
> [the blocker](#the-cross-repo-blocker) (`mache-048fb6`). "Standardize across
> repos" has an unmet prerequisite.

## Method

- `mache build <repo> <db>` (leyline backend → full `_ast` + `node_defs`/`node_refs`).
- `mache find-smells --db <db> --rule '*' --limit 10000 --fail-on=none --format=ci`
  — every rule, uncapped (the default 200 cap silently understates; raised here).
- Counts **denoised** of generated / vendored / test-snapshot paths
  (`testdata/`, `/target/`, `node_modules/`, `*_generated.go`, `*.pb.go`,
  `*.capnp.go`, `.gen.`, `_agent_log`, `/dist/`).
- Locations are real (`file:line`) as of `dac8ac5` — earlier runs reported
  `:1:1` on the `node_defs` rules.

## Go repos (accurate)

| Repo  | Files | Denoised findings |
| ----- | ----- | ----------------- |
| mache | 896   | 4568              |
| assay | 27    | 172               |

### Per-rule

| Rule                      | mache | assay | Read                                                                                                                                              |
| ------------------------- | ----: | ----: | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| `cyclomatic_complexity`   |  2929 |   111 | **noisy** — one per control-flow node; meaningless without `min_metric` (≥10 noteworthy, ≥20 review). Not 2929 problems.                          |
| `magic_int_in_comparison` |  1098 |    36 | **noisy** — every int-literal comparison; inherently high, needs curation.                                                                        |
| `duplicate_definitions`   |   439 |    24 | **FP-inflated** — dominated by interface methods implemented by N types colliding on the bare token (known; see `mache-409569` / `mache-40ae5c`). |
| `long_function`           |    99 |     1 | **actionable** — bodies >80 lines. mache's real sprawl signal.                                                                                    |
| `long_file`               |     3 |     0 | **actionable** — all three are *test* files (`serve_test.go` 3076, `serve_find_smells_test.go` 3365, `socket_test.go` 1578).                      |
| `god_file`                |     0 |     0 | clean — no file ≥10 defs AND >3× mean.                                                                                                            |
| `dead_code`               |     0 |     0 | clean.                                                                                                                                            |
| `fan_out_skew`            |     0 |     0 | clean.                                                                                                                                            |
| `untested_function`       |     0 |     0 | clean (static proxy).                                                                                                                             |
| `sleep_in_test`           |     0 |     0 | clean.                                                                                                                                            |

### Truth vs. assumption

The "god files everywhere" intuition is **falsified** for the Go repos:
`god_file` / `fan_out_skew` = 0. The sprawl that exists wears the mask of **long
functions** (99 in mache — hotspots: `engine_walk.go` 325-line method,
`engine_languages.go` 253, `engine_treesitter.go` 232) and **giant test files**,
not god-files-by-def-count. mache is also the *best*-tended repo (it hosts the
engine, just finished the S-slice refactors).

### Top files by finding count (denoised)

- **mache**: `cmd/serve_test.go` (182), `tools/coverage-gate/main_test.go` (88),
  `cmd/serve_find_smells_test.go` (84), `internal/leyline/socket_test.go` (69),
  `cmd/serve_registry.go` (64) — test files dominate; the lone non-test in the
  top band is `serve_registry.go`.
- **assay**: `internal/embeddings/dense.go` (19), `internal/coverage/tokenize.go`
  (18), `internal/docs/extract.go` (14), `internal/code/extract.go` (13).

## The cross-repo blocker

`mache-048fb6` (P1). Verified 2026-06-24:

- **leyline 0.4.5 does not parse Rust.** `mache build` on a 7-file Rust crate
  ingested **0** `.rs` files (only markdown). rosary (9 crates),
  ley-line-open (105 crates), cloister (4 crates + TS) are unmeasured.
- **`--backend tree-sitter` ingests Rust files but extracts 0 defs** — the
  28-language registry has the grammar, but the default FCA build produces no
  symbol defs/refs without a per-language def/ref schema, so every `node_defs`
  rule returns nothing.

Until one of these is closed, cross-repo standardization is Go-only:

- (a) leyline gains Rust (+JS/TS) parsing → emits `_ast`/`node_defs` like Go (preferred — unifies on the LLO substrate);
- (b) author tree-sitter def/ref schemas (`examples/rust-schema.json`, `ts-schema.json`) so the standalone backend extracts defs;
- (c) both — schema as the standalone floor, leyline as the enriched tier (the fidelity-ladder pattern).

## Next steps

1. **Unblock non-Go parsing** (`mache-048fb6`) — prerequisite for any cross-repo claim.
1. **Tune the noisy rules** — `cyclomatic`/`magic_int` need default `min_metric`s or
   they drown the digest; `duplicate_definitions` FP class is `mache-409569`/`mache-40ae5c`.
1. **Build the ratchet** — once counts are trustworthy and stable, snapshot a
   per-repo baseline file and gate on *new* findings (advisory→enforced).
1. **Burn down** the actionable Go set — 99 long functions, 3 long test files.

_Generated from `mache find-smells`. Re-run `task install && /tmp/baseline_go.sh` to refresh._
