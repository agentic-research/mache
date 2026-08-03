#!/usr/bin/env python3
"""Cost + quality bench: mache MCP vs vanilla Claude Code on the mache repo.

Lifts the methodology from GrapeRoot's `Codex-CLI-Compact/benchmark/
run_preinjection_benchmark.py` (MIT, Copyright 2025 Kunal). Two modes:

    normal  -> Claude Code with no MCP server, baseline
    mache   -> Claude Code with mache MCP server providing structural tools

Per prompt, captures: input/output tokens, cache hit/miss, wall time,
total cost (USD, from Claude's own accounting), turn count, and a 50-point
quality score on the response. Results stream to a JSONL file (resume-safe).

Drops GrapeRoot's pre-injection mode — mache doesn't pre-inject; its
projected filesystem IS the pre-injection equivalent if the agent reads it,
but that's a separate axis we measure later.

Usage:
    python bench.py --corpus /Users/jamesgardner/remotes/art/mache \\
                    --db /tmp/mache_fixed.db \\
                    --out results/run_$(date +%Y%m%d).jsonl
    python bench.py --prompts 1,3,5 ...
    python bench.py --modes normal ...
    python bench.py --resume ...
"""
from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
import time
from pathlib import Path
from statistics import mean, median

# Per-prompt timeout. Claude can spin for a while on complex prompts when
# the agent decides to read 10+ files; the GrapeRoot reference uses 300s.
TIMEOUT_S = 300
COOLDOWN_S = 5

ROOT = Path(__file__).resolve().parent
PROMPTS_FILE = ROOT / "prompts.json"
MCP_CONFIG = ROOT / "mcp.json"


def log(msg: str) -> None:
    print(f"[bench] {msg}", file=sys.stderr, flush=True)


def parse_claude_json(stdout: str) -> dict:
    """Claude Code emits JSON on stdout under --output-format json. Be
    tolerant of trailing log lines — the JSON object is the first
    line that starts with '{'."""
    for line in stdout.splitlines():
        line = line.strip()
        if line.startswith("{"):
            try:
                return json.loads(line)
            except json.JSONDecodeError:
                continue
    return {}


def run_claude(prompt: str, corpus: Path, db: Path, mode: str) -> dict:
    """Invoke `claude -p` once. Returns metrics dict.

    Mirrors GrapeRoot's run_claude shape so per-mode results are comparable.
    """
    cmd = [
        "claude", "-p", prompt,
        "--output-format", "json",
        "--model", "claude-sonnet-4-6",
        "--dangerously-skip-permissions",  # bench runs need to read files
    ]

    env = os.environ.copy()
    if mode == "mache":
        env["MACHE_BIN"] = str(Path(os.environ.get("MACHE_BIN", ROOT.parent.parent / "bin" / "mache")).resolve())
        env["MACHE_DB"] = str(db.resolve())
        cmd.extend(["--mcp-config", str(MCP_CONFIG)])

    wall_start = time.time()
    try:
        result = subprocess.run(
            cmd,
            cwd=str(corpus),
            env=env,
            capture_output=True,
            text=True,
            timeout=TIMEOUT_S,
        )
    except subprocess.TimeoutExpired:
        return _err_result(time.time() - wall_start, "timeout", f"Timeout after {TIMEOUT_S}s")
    except Exception as exc:  # subprocess setup error, not Claude itself
        return _err_result(time.time() - wall_start, "error", str(exc))

    wall_time = time.time() - wall_start

    if result.returncode != 0:
        return _err_result(
            wall_time, "error",
            f"claude exited {result.returncode}: {result.stderr[-300:]}",
        )

    data = parse_claude_json(result.stdout)
    usage = data.get("usage", {})
    raw_input = usage.get("input_tokens", 0)
    cache_create = usage.get("cache_creation_input_tokens", 0)
    cache_read = usage.get("cache_read_input_tokens", 0)
    output_tokens = usage.get("output_tokens", 0)
    total_input = raw_input + cache_create + cache_read

    return {
        "wall_time_s": round(wall_time, 2),
        "duration_ms": data.get("duration_ms", 0),
        "duration_api_ms": data.get("duration_api_ms", 0),
        "input_tokens": total_input,
        "input_tokens_raw": raw_input,
        "output_tokens": output_tokens,
        "cache_creation_tokens": cache_create,
        "cache_read_tokens": cache_read,
        "total_cost_usd": data.get("total_cost_usd", 0.0),
        "response_text": data.get("result", ""),
        "num_turns": data.get("num_turns", 1),
        "stop_reason": data.get("stop_reason", ""),
    }


def _err_result(wall: float, reason: str, msg: str) -> dict:
    return {
        "wall_time_s": round(wall, 2),
        "duration_ms": 0,
        "duration_api_ms": 0,
        "input_tokens": 0,
        "input_tokens_raw": 0,
        "output_tokens": 0,
        "cache_creation_tokens": 0,
        "cache_read_tokens": 0,
        "total_cost_usd": 0.0,
        "response_text": "",
        "num_turns": 0,
        "stop_reason": reason,
        "error": msg,
    }


def score_quality(response: str, category: str) -> dict:
    """50-point heuristic quality score. Ported from GrapeRoot with the
    file-extension regex updated for Go-flavored corpora (.go, .md, .json,
    .yaml, .yml, .toml, .py for cross-comparison). Everything else is
    language-agnostic.

    Dimensions:
        word_count      0-8
        file_mentions   0-10
        code_blocks     0-8
        specificity     0-10  (concrete identifiers in backticks)
        structure       0-6   (headings, bullets, numbered lists)
        category_bonus  0-8   (category-specific signals)
    """
    if not response or "error" in response.lower()[:50]:
        return {"total": 0, "word_count": 0, "file_mentions": 0, "code_blocks": 0,
                "specificity": 0, "structure": 0, "category_bonus": 0}

    word_count = len(response.split())
    wc = 8 if word_count >= 500 else 6 if word_count >= 300 else 4 if word_count >= 150 else 2 if word_count >= 50 else 0

    # File paths — mache repo is Go-centric but we keep multi-language regex
    # so Python/JS prompts in the corpus also score consistently.
    file_pattern = re.compile(
        r'`[a-zA-Z0-9_/\-.]+\.(?:go|py|ts|tsx|js|jsx|json|yaml|yml|toml|sql|md|sh)`'
        r'|[a-zA-Z0-9_/\-]+\.(?:go|py|ts|tsx|js|jsx|json|yaml|yml|toml|sql|md|sh)'
    )
    file_count = len(set(file_pattern.findall(response)))
    fm = 10 if file_count >= 8 else 8 if file_count >= 5 else 6 if file_count >= 3 else 3 if file_count >= 1 else 0

    cb_pairs = response.count("```") // 2
    cb = 8 if cb_pairs >= 3 else 6 if cb_pairs >= 2 else 4 if cb_pairs >= 1 else 0

    identifiers = re.findall(r'`[a-zA-Z_][a-zA-Z0-9_]*(?:\([^)]*\))?`', response)
    ident_count = len(set(identifiers))
    spec = 10 if ident_count >= 15 else 8 if ident_count >= 10 else 5 if ident_count >= 5 else 3 if ident_count >= 2 else 0

    headings = len(re.findall(r'^#{1,4}\s', response, re.MULTILINE))
    bullets = len(re.findall(r'^[\s]*[-*]\s', response, re.MULTILINE))
    numbered = len(re.findall(r'^[\s]*\d+\.\s', response, re.MULTILINE))
    struct_items = headings + min(bullets, 10) + min(numbered, 10)
    struct = 6 if struct_items >= 10 else 4 if struct_items >= 5 else 2 if struct_items >= 2 else 0

    # Category bonus: rewards signals appropriate to the prompt type.
    # Lifted from GrapeRoot's heuristics, expanded for Go-codebase prompts.
    rl = response.lower()
    cat = 0
    if category == "code_explanation":
        if any(w in rl for w in ("flow", "step", "first", "then", "finally", "phase")):
            cat += 3
        if any(w in rl for w in ("endpoint", "route", "handler", "package", "function", "method")):
            cat += 3
        if file_count >= 3:
            cat += 2
    elif category == "bug_fix":
        if any(w in rl for w in ("cause", "bug", "issue", "fix", "problem", "root cause")):
            cat += 3
        if any(w in rl for w in ("solution", "patch", "change", "modify", "fix:")):
            cat += 3
        if cb_pairs >= 1:
            cat += 2
    elif category == "feature_add":
        if any(w in rl for w in ("implement", "add", "extend", "new")):
            cat += 3
        if cb_pairs >= 1:
            cat += 3
        if file_count >= 2:
            cat += 2
    elif category == "refactoring":
        if any(w in rl for w in ("refactor", "extract", "consolidate", "duplicate", "common")):
            cat += 4
        if cb_pairs >= 1:
            cat += 4
    elif category == "architecture":
        if any(w in rl for w in ("tradeoff", "design", "compare", "axis", "boundary")):
            cat += 4
        if any(w in rl for w in ("layer", "component", "module", "boundary")):
            cat += 4
    elif category == "debugging":
        if any(w in rl for w in ("bottleneck", "instrument", "profile", "trace", "candidate")):
            cat += 4
        if any(w in rl for w in ("hypothesis", "verify", "measure", "evidence")):
            cat += 4

    total = wc + fm + cb + spec + struct + cat
    return {"total": total, "word_count": wc, "file_mentions": fm,
            "code_blocks": cb, "specificity": spec, "structure": struct,
            "category_bonus": cat}


def load_completed_ids(out_file: Path) -> set:
    """Resume support — re-read the JSONL and skip prompts already done
    for each (prompt_id, mode) tuple."""
    done = set()
    if not out_file.exists():
        return done
    with out_file.open() as f:
        for line in f:
            try:
                row = json.loads(line)
                done.add((row["prompt_id"], row["mode"]))
            except (json.JSONDecodeError, KeyError):
                continue
    return done


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--corpus", required=True, type=Path, help="Path to the codebase to run prompts against")
    parser.add_argument("--db", required=True, type=Path, help="Path to the mache .db file (for mache mode)")
    parser.add_argument("--out", required=True, type=Path, help="JSONL output file")
    parser.add_argument("--prompts", help="Comma-separated prompt IDs (default: all)")
    parser.add_argument("--modes", default="normal,mache", help="Comma-separated mode list (default: normal,mache)")
    parser.add_argument("--resume", action="store_true", help="Skip prompt-mode pairs already in the JSONL")
    args = parser.parse_args()

    with PROMPTS_FILE.open() as f:
        all_prompts = json.load(f)

    if args.prompts:
        wanted = {int(x) for x in args.prompts.split(",")}
        prompts = [p for p in all_prompts if p["id"] in wanted]
    else:
        prompts = all_prompts

    modes = [m.strip() for m in args.modes.split(",")]
    for m in modes:
        if m not in ("normal", "mache"):
            log(f"unknown mode {m!r}; must be 'normal' or 'mache'")
            return 2

    args.out.parent.mkdir(parents=True, exist_ok=True)
    done = load_completed_ids(args.out) if args.resume else set()

    log(f"corpus={args.corpus} db={args.db} out={args.out}")
    log(f"prompts={len(prompts)} modes={modes} resume_skip={len(done)}")

    with args.out.open("a") as out_f:
        for p in prompts:
            for mode in modes:
                key = (p["id"], mode)
                if key in done:
                    log(f"skip prompt={p['id']} mode={mode} (already done)")
                    continue

                log(f"prompt={p['id']} category={p['category']} mode={mode} -> running")
                metrics = run_claude(p["prompt"], args.corpus, args.db, mode)
                quality = score_quality(metrics["response_text"], p["category"])

                row = {
                    "prompt_id": p["id"],
                    "category": p["category"],
                    "mode": mode,
                    "ts": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                    **metrics,
                    "quality": quality,
                }
                out_f.write(json.dumps(row) + "\n")
                out_f.flush()

                err = metrics.get("error", "")
                cost = metrics.get("total_cost_usd", 0.0)
                turns = metrics.get("num_turns", 0)
                qtotal = quality["total"]
                log(f"  done cost=${cost:.4f} turns={turns} quality={qtotal}/50 wall={metrics['wall_time_s']:.1f}s {err}")

                time.sleep(COOLDOWN_S)

    log("aggregating...")
    aggregate(args.out)
    return 0


def aggregate(jsonl: Path):
    """Read the JSONL and print a per-mode summary table to stderr."""
    by_mode: dict[str, list[dict]] = {}
    with jsonl.open() as f:
        for line in f:
            try:
                row = json.loads(line)
            except json.JSONDecodeError:
                continue
            by_mode.setdefault(row["mode"], []).append(row)

    print("\n=== Summary ===", file=sys.stderr)
    for mode, rows in sorted(by_mode.items()):
        costs = [r["total_cost_usd"] for r in rows if r["total_cost_usd"]]
        turns = [r["num_turns"] for r in rows if r["num_turns"]]
        walls = [r["wall_time_s"] for r in rows if r["wall_time_s"]]
        quals = [r["quality"]["total"] for r in rows]
        if not rows:
            continue
        print(f"\n[{mode}]  n={len(rows)}", file=sys.stderr)
        if costs:
            print(f"  cost  mean=${mean(costs):.4f}  median=${median(costs):.4f}  sum=${sum(costs):.4f}", file=sys.stderr)
        if turns:
            print(f"  turns mean={mean(turns):.1f}  median={median(turns):.1f}", file=sys.stderr)
        if walls:
            print(f"  wall  mean={mean(walls):.1f}s median={median(walls):.1f}s", file=sys.stderr)
        if quals:
            print(f"  qual  mean={mean(quals):.1f}/50 median={median(quals):.1f}/50", file=sys.stderr)


if __name__ == "__main__":
    sys.exit(main())
