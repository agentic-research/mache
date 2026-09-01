# Cost + quality bench: mache MCP vs vanilla Claude Code

Measures the **practical impact** of plugging mache's MCP surface into a Claude Code session — cost, turn count, response time, and a 50-point quality score on the response — across 15 representative prompts about the mache codebase itself.

Lifts methodology from GrapeRoot's `Codex-CLI-Compact/benchmark/run_preinjection_benchmark.py` (MIT, Copyright 2025 Kunal). Drops their pre-injection mode; runs two modes:

| Mode     | What it is                                                                    |
| -------- | ----------------------------------------------------------------------------- |
| `normal` | `claude -p <prompt>` with no MCP servers — baseline                           |
| `mache`  | `claude -p <prompt> --mcp-config mcp.json` — mache MCP server serving the .db |

## Why this exists, why this corpus

PR #373's correctness audit caught 6 bugs but produced zero perf numbers. Bead `mache-15f15a` covers the broader perf-instrumentation plan; this bench is its first concrete deliverable: a single, reproducible run that emits real numbers comparable to GrapeRoot's published $0.17 / $0.25 / $0.27 figures.

We use the **mache repo as the corpus** because (a) it's free of "go clone first" friction, (b) we know it intimately so quality scores are spot-checkable, (c) Go gives mache's tree-sitter ingestion a workout. A follow-up run on a larger public corpus (Kubernetes, Consul) is straightforward via the same harness.

## What gets measured (per prompt, per mode)

Direct from `claude -p --output-format json`:

- `input_tokens` / `output_tokens` / `cache_creation_tokens` / `cache_read_tokens`
- `total_cost_usd` — Claude's own cost accounting
- `duration_ms` / `duration_api_ms` — Claude's internal duration
- `num_turns` — how many model-tool round trips
- `wall_time_s` — our subprocess wall-clock

Computed on the response text:

- 50-point quality score (`word_count` + `file_mentions` + `code_blocks` + `specificity` + `structure` + `category_bonus`). Ported from GrapeRoot's scorer with file-extension regex updated for Go-flavored corpora. See `bench.py` for the per-dimension breakdown.

## Prompt battery (15 prompts × 6 categories)

Mirrors GrapeRoot's category split so any cross-tool comparison stays apples-to-apples:

| Category           | Count | Prompts focus on...                                                                            |
| ------------------ | ----: | ---------------------------------------------------------------------------------------------- |
| `code_explanation` |     3 | Ingestion pipeline, find_callers resolution, projected-FS vs MCP-tool                          |
| `bug_fix`          |     3 | Real bugs PR #373 surfaced (Topology empty, find_callees empty, search role=definition broken) |
| `feature_add`      |     3 | Kotlin language support, find_implementers MCP tool, LSP-backed get_call_signature             |
| `refactoring`      |     2 | lazyGraph passthroughs, Go ref-query growth                                                    |
| `architecture`     |     2 | Schema projection vs LSP-live, .db lifecycle                                                   |
| `debugging`        |     2 | Cold-start bottleneck, ingest OOM                                                              |

Prompts live in `prompts.json` — JSON shape matches GrapeRoot's so the same scorer/runner can ingest both batteries.

## Running

Prerequisites:

- `claude` CLI installed and authenticated
- `mache` binary built (`task build` at repo root)
- A `.db` file built for the corpus (`./bin/mache build --backend tree-sitter --schema examples/go-schema.json . /tmp/mache_corpus.db`)

```sh
# Default: both modes, all 15 prompts, fresh run
python3 benchmarks/cost-quality/bench.py \
    --corpus /Users/jamesgardner/remotes/art/mache \
    --db /tmp/mache_corpus.db \
    --out benchmarks/cost-quality/results/run_$(date +%Y%m%d).jsonl

# Subset: just bug_fix prompts (4,5,6), just mache mode
python3 benchmarks/cost-quality/bench.py \
    --corpus /Users/jamesgardner/remotes/art/mache \
    --db /tmp/mache_corpus.db \
    --out benchmarks/cost-quality/results/quick.jsonl \
    --prompts 4,5,6 --modes mache

# Resume after interruption
python3 benchmarks/cost-quality/bench.py \
    --corpus /Users/jamesgardner/remotes/art/mache \
    --db /tmp/mache_corpus.db \
    --out benchmarks/cost-quality/results/run_$(date +%Y%m%d).jsonl \
    --resume
```

The runner appends one JSONL row per `(prompt, mode)` pair as it goes. The aggregate summary prints to stderr at the end with `n`, mean/median cost, turns, wall time, and quality.

### Cost estimate

15 prompts × 2 modes × roughly 50K input tokens average × ~$3/M (Claude Sonnet 4.6) ≈ **~$5 per full run**. Output and cache differences will shift this. Subsets are cheap for iteration.

## Reading the results

The JSONL is the source of truth. Each row:

```json
{
  "prompt_id": 4,
  "category": "bug_fix",
  "mode": "mache",
  "ts": "2026-05-13T20:00:00Z",
  "wall_time_s": 18.3,
  "input_tokens": 47823,
  "output_tokens": 1124,
  "total_cost_usd": 0.1842,
  "num_turns": 7,
  "stop_reason": "end_turn",
  "response_text": "...",
  "quality": {
    "total": 38,
    "word_count": 6,
    "file_mentions": 8,
    "code_blocks": 4,
    "specificity": 10,
    "structure": 4,
    "category_bonus": 6
  }
}
```

The aggregate at the end produces per-mode mean/median tables for cost / turns / wall / quality.

## Comparing across tools

The JSONL shape is intentionally compatible with GrapeRoot's `run_preinjection_benchmark.py` output. To cross-compare with their published $0.17 / $0.25 / $0.27 numbers, you'd need to re-run their bench against their corpus — which isn't checked in, so direct apples-to-apples isn't possible from this harness alone. What IS comparable: the per-mode deltas (normal vs mache) on this corpus, and the absolute magnitudes ($/prompt, turns/prompt) against their reported magnitudes for sanity.

A follow-up that fully closes the comparison is an entry on bead `mache-15f15a`.

## Attribution

Methodology + scorer + runner shape © 2025 Kunal (GrapeRoot project), MIT-licensed. Source: `github.com/kunal12203/Codex-CLI-Compact`. The Go-flavored prompt battery, mache-MCP wiring, and the scorer's expanded file-extension regex are mache-side adaptations.
