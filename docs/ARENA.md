---
status: current
covers-version: v0.12.0
last-verified: 2026-07-03
sources-of-truth:
  - scripts/arena.sh
audience: [contributors, evaluators, agent developers]
supersedes: []
---

# Mache Arena: 6-Level Agent Benchmark

The Arena is a custom benchmark that tests whether an LLM-driven agent can operate on real code **only through mache's filesystem abstraction** — no direct file access. The target code is intentionally bespoke so memorized patterns don't help.

The driver is [`scripts/arena.sh`](../scripts/arena.sh); this doc explains what the levels measure and how to run them. For the agent-facing briefing, see the `PROMPT.txt` that the script writes into the sandbox.

## Running the Arena

```bash
# Interactive (script mounts the sandbox, you bring your own agent):
bash scripts/arena.sh

# Same flow with automated verification when you press ENTER:
bash scripts/arena.sh --verify

# Pre-canned demo (no agent, no verification — just shows the mount):
bash scripts/arena.sh --demo
```

The script builds mache, creates a disposable sandbox at `/tmp/mache-arena/`, ingests a hand-crafted Go codebase, and NFS-mounts it at `/tmp/mache-arena/mnt/`. Drop your agent into the sandbox with the generated `PROMPT.txt` and let it work through the levels.

## The Six Levels

| #   | Name                          | What it measures                                                                                                    |
| --- | ----------------------------- | ------------------------------------------------------------------------------------------------------------------- |
| 1   | Orientation                   | Can the agent discover the codebase by listing virtual directories and writing a summary?                           |
| 2   | Bug Hunt                      | Can the agent locate and fix a logic bug (off-by-one) by reading and rewriting `source` files?                      |
| 3   | Needle in a Haystack          | Can the agent find a subtle empty-input edge case by inspecting multiple functions?                                 |
| 4   | Cross-Function Refactor       | Can the agent change two related functions consistently (checksum: XOR → addition mod 256) without breaking syntax? |
| 5   | Adversarial Write             | Does the agent correctly use `_diagnostics/last-write-status` to recover from rejected writes (Draft mode)?         |
| 6   | Reverse Call-Chain Navigation | Can the agent use `callers/` virtual directories to trace dependencies and find zero-caller constructs?             |

Levels 1–4 exercise the read + write-back loop. Level 5 verifies the agent understands the validate → format → splice pipeline and Draft mode. Level 6 specifically tests the `callers/` self-gating virtual-dir model.

## Scoring

After each level the agent appends notes to `/tmp/mache-arena/agent-notes.md`. `--verify` runs automated checks against the sandbox state (file contents, presence/absence of constructs, notes content) and prints a per-level pass/fail summary.

## What This Doesn't Test

- Multi-file source-of-truth files outside the Arena's hand-crafted codebase.
- `find_smells`, `get_impact`, `get_architecture`, `semantic_search` MCP tools (Arena is filesystem-only; these are MCP-only surfaces).
- Hot-swap behavior (`current_root` substrate identity from v0.8.0).
- The `--mount` cross-repo serve path.

For end-to-end MCP-tool coverage see [`cmd/all_tools_e2e_test.go`](../cmd/all_tools_e2e_test.go) and the `task profile-tools` / `task flamegraphs` harness described in [ARCHITECTURE.md § E2E Tool Harness](ARCHITECTURE.md#e2e-tool-harness--profiling).
