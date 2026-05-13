# mache vs Serena — head-to-head evaluation harness

Adapts Serena's published evaluation methodology (their one-shot agent-self-eval prompt) to measure mache against the same task battery, on the same codebases Serena has already published baselines for. Goal: a direct, reproducible comparison with neutral methodology — not a marketing exercise.

## Why this exists, not a from-scratch design

The user question — *"why wouldn't we pull Serena's existing benches and run them with our tool?"* — is the right one. Serena (Oraios AI, MIT) shipped a complete, agent-friendly evaluation framework: a single prompt the agent runs end-to-end across ~20 hands-on tasks spanning 5 task categories, with published baseline results across multiple agent × codebase combinations. Reusing it gives us:

1. **Methodology we don't have to defend** — it's neutral, was authored by the competitor, and is already in production use
1. **Pre-existing baselines** to diff against (see `baselines/`)
1. **A reproducible single-prompt harness** — no scripts to write, no scoring rubric to invent

## Methodology source

Vendored under `upstream/` (MIT-licensed, Copyright 2025 Oraios AI):

- `upstream/010_methodology.md` — design goals + measurement framework
- `upstream/010_evaluation-prompt.md` — the actual one-shot prompt (332 lines)
- `upstream/020_summary-prompt.md` — the report-summarization prompt

These are verbatim copies from `oraios/serena@70d93973` (`docs/04-evaluation/`). Re-vendor against upstream when running a fresh comparison to pick up any methodology improvements.

## What changes for mache

Serena evaluates **20 tasks across 5 categories**: codebase understanding, single-file edits, multi-file changes, reliability/correctness, workflow effects. Mache is intentionally a **read-only structural projection** — it has no rename/move/insert/replace tool surface. So:

| Category                      | Tasks | Applies to mache?                                                              |
| ----------------------------- | ----- | ------------------------------------------------------------------------------ |
| 1. Codebase understanding     | 1–6   | **Yes** — this is mache's core surface                                         |
| 2. Single-file edits          | 7a–9  | **No** — read-only; mark as "out of scope"                                     |
| 3. Multi-file changes         | 10–13 | **No** — read-only; mark as "out of scope"                                     |
| 4. Reliability & correctness  | 14–16 | **Partial** — scope-precision applies; atomicity/success-signals are edit-side |
| 5. Workflow effects           | 17–18 | **Partial** — multi-step exploration (18) applies; chained edits (17) does not |
| 6. "Shouldn't be interesting" | 19–20 | **Yes** — non-code reads, free-text search                                     |

The mache-vs-Serena delta therefore lives in categories 1, partial 4, partial 5, 6. The honest read: mache positions itself in the **structural-projection** axis (Serena's category 1 plus richer offerings like community detection, change impact, mermaid diagrams) and concedes the **edit** axis to Serena and agent-lsp (categories 2–4).

Categories 2–4 in the report should be classified `(c) out of scope` per Serena's own taxonomy (see `upstream/010_methodology.md` §Method) — not `(b) neutral or negative finding`. A neutral/negative finding requires that the tool targets the task and performs worse; mache deliberately doesn't target editing.

## What the evaluation IS measuring on mache

For each in-scope task, the agent records:

- **Call count** — tool invocations needed end-to-end
- **Input payload** — tokens sent to the tool
- **Output payload** — tokens returned from the tool
- **Prerequisite steps** — reads/grep/listings needed before the productive call
- **Stability under edits** — does the addressing still work after a file changes?

Then: order findings by **frequency × value-per-hit** (Serena's required framing). Headline finding goes first.

The mache-side claims to test:

1. **Pre-baked LSP indices**: ley-line's `_lsp*` tables in the .db mean `find_callers`, `find_definition`, `get_type_info`, `get_diagnostics` have zero LSP-daemon startup cost. Serena pays LSP startup on first call to each language.
1. **Projected FS as zero-call browse**: walking `/mnt/mache/pkg/foo.go/Foo/callers/` is `ls`, not an MCP call at all. Serena's `find_referencing_symbols` is always at least one tool invocation.
1. **Token cost**: mache's `list_directory` on a projected construct dir returns short subdir names (`Topology/`, `NewMemoryStore/`, …) vs Serena's `get_symbols_overview` returning structured JSON with kinds, ranges, etc. Direct token-count comparison per task is the metric.

## How to run

Mache must be mounted (or `serve` with `--control`) on the target repo before the agent starts:

```sh
# Set up the corpus repo (use Tianshou for parity with the Serena baseline)
git clone https://github.com/thu-ml/tianshou /tmp/tianshou-bench

# Start the LLO daemon + mache against it (or mount directly)
leyline daemon --source /tmp/tianshou-bench --arena /tmp/tianshou.arena --control /tmp/tianshou.ctrl &
mache serve --control /tmp/tianshou.ctrl --http :7532 &

# Wire mache into Claude Code as an MCP server
claude mcp add --transport http mache http://localhost:7532/mcp

# Start a fresh CC session and paste the adapted prompt below
```

Adapted prompt skeleton (see `upstream/010_evaluation-prompt.md` for the full text):

> Evaluate mache's tools against built-ins for codebase understanding tasks (Serena prompt §"Codebase understanding" 1–6, §"Workflow effects" 18, §"Things where comparison shouldn't be interesting" 19–20). For categories 2–4 (editing, multi-file changes, reliability of edits), classify as **out of scope** — mache is intentionally a read-only structural projection; live editing is provided by separate tools.

Write the report to `serena-style-evaluation-MACHE.md` for parity with Serena's filenames.

## Baseline to compare against

`baselines/serena_cc_opus_4.6_on_tianshou.md` — vendored from `oraios/serena@70d93973` `docs/04-evaluation/030_results/010_cc_on_tianshou.md`. This is **Serena's own result**, evaluated by Claude Opus 4.6 (Claude Code CLI), 20 tasks on Tianshou (~26K LOC Python, 43 source files), 2026-04-13.

Their headline:

> Serena's IDE-backed semantic tools are the single most impactful addition to my toolkit — cross-file renames, moves, and reference lookups that would cost me 8–12 careful, error-prone steps collapse into one atomic call

Their measured wins are all in categories 2–3 (cross-file rename, move, atomicity) — exactly the categories mache concedes. Their measured neutrals/losses are in category 1 (`get_symbols_overview` is "slightly less efficient" than Edit for small known-line changes).

Mache's hypothesis is that on category 1 (codebase understanding, where Serena's baseline classified results as `moderate positive`), mache should classify as **strong positive vs built-ins** because:

- Projected FS = `ls` instead of an MCP call
- Pre-baked refs index = no LSP daemon startup
- `find_callers` returns directly addressable node IDs that re-key into the projection without re-resolution

That hypothesis is testable by running the adapted prompt on the same corpus and diffing reports.

## Next steps (sequenced)

1. **Run the adapted prompt** — Claude Code session, mache mounted on tianshou, write to `results/mache_cc_opus_4.7_on_tianshou.md`
1. **Side-by-side report** — `analysis/mache_vs_serena_tianshou.md` enumerating each task's call count + payload diff
1. **Expand corpus** — repeat on the other Serena baselines (`020_codex_on_jbplugin.md`, `030_copilot_cli_on_ente.md`) to get cross-language coverage
1. **Add agent-lsp** when the same harness can be pointed at it — Serena's prompt is server-agnostic by design, so this is just running it three times

Tracked under bead `mache-7937c5`.

## Attribution

Methodology and prompts © 2025 Oraios AI, MIT-licensed. Source: github.com/oraios/serena, commit `70d93973`. Vendored under `upstream/` per the MIT license terms; copyright notice preserved.
