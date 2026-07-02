---
status: current
covers-version: v0.8.0
last-verified: 2026-06-24
---

# Smell-debt baseline — 2026-06-24

First measured snapshot of structural smell debt across the ART repos, the
input the advisory→enforced **ratchet** will eventually grandfather. Produced
with `mache find-smells` (commit `dac8ac5`) over leyline-parsed `.db`s.

> **Headline:** this snapshot is **Go-only** — but the cross-repo gap is
> narrower than it looks. A *current* leyline parses Rust into a full `_ast`
> (the installed binary is just stale); the genuine missing piece is **Rust
> def/ref extraction** (`node_defs`), which is Go-only in LLO today. So the
> structural rules can't yet run on Rust/TS — see
> [the blocker](#the-cross-repo-blocker). One LLO function (`extract_rust`) plus
> cross-lang rule generalization unlocks it.

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

The gap is narrower than a first pass suggested. Corrected against **current LLO
source** (not the stale installed binary), 2026-06-24:

- **leyline _does_ parse Rust.** The `rust` cargo feature (added 2026-06-22,
  pulled transitively by the default `hdc` feature) wires `tree-sitter-rust`. A
  binary built from current source parses a 7-file crate into a full `_ast`
  (743 identifiers, 181 call-expressions, function nodes). The earlier "0 files"
  result was the **stale `~/.local/bin/leyline` (May-23 build, pre-`rust`-feature)** —
  an install-currency problem, not a capability gap. **Action: rebuild + install
  current leyline.**
- **The real gap is def/ref extraction, which is Go-only.** On a current build,
  Rust `node_defs = 0` while Go = 49. `rs/ll-open/ts/src/refs.rs` matches on
  language with a single `TsLanguage::Go => extract_go(...)` arm; the file's own
  doc says *"add new languages by adding a match arm + an `extract_<lang>`
  function."* So Rust gets AST but no symbols → the `node_defs` rules
  (`god_file`/`dead_code`/`duplicate_definitions`/`fan_out_skew`/`untested_function`)
  return nothing. **This is an LLO bead: write `extract_rust`** (`ley-line-open-…`).
- **Several rules are Go-language-gated.** `cyclomatic_complexity` /
  `long_function` / `magic_int_in_comparison` carry `languages: ["go"]`, so they
  won't fire on Rust even though the AST has the nodes (`function_item`,
  `if_expression`, …). Cross-lang generalization is a mache concern (`mache-048fb6`).

What **already works for Rust** on a current leyline: cross-lang `_ast` rules —
`long_file` correctly lists `round_trip.rs`, `projection.rs`, etc. So the moment
`extract_rust` lands (and the metric rules generalize), rosary (9 crates),
ley-line-open (105), and cloister (4 + TS) become measurable.

## Next steps

1. **Install current leyline** — the May-23 binary predates Rust support.
1. **`extract_rust` in LLO** (`ts/src/refs.rs`) → Rust `node_defs`/`node_refs`; unblocks the structural rules cross-repo.
1. **Generalize the Go-gated metric rules** cross-lang (`mache-048fb6`) — the Rust/JS AST nodes exist; the rules just need non-Go arms.
1. **Tune the noisy rules** — `cyclomatic`/`magic_int` need default `min_metric`s; `duplicate_definitions` FP class is `mache-409569`/`mache-40ae5c`.
1. **Build the ratchet** — once counts are trustworthy + cross-repo, snapshot a per-repo baseline and gate on *new* findings (advisory→enforced).
1. **Burn down** the actionable Go set — 99 long functions, 3 long test files.

_Generated from `mache find-smells`. Re-run `task install && /tmp/baseline_go.sh` to refresh._
