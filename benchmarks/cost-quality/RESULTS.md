# What this bench actually told us

**Short version**: the bench is not currently useful for evaluating mache. It measured mache in the wrong configuration, against the wrong baseline, on a workload that doesn't exercise the layer mache is supposed to add. The raw JSONL is in `results/run_20260513.jsonl`; the headline numbers are committed for posterity but should not be cited.

This document explains why, and what needs to exist before the same bench produces a result that matters.

## What we ran

15 prompts × 2 modes (vanilla Claude vs `mache serve --stdio /tmp/mache_bench.db`), Sonnet 4.6, 300s subprocess timeout, GrapeRoot's 50-point quality scorer ported. See `bench.py` and `prompts.json` for the methodology lift from `Codex-CLI-Compact`.

## What we got

Of 30 calls:

- 17 completed cleanly
- 6 timed out (all `bug_fix` × both modes — the hardest category)
- 1 asymmetric finish (p9: mache completed, normal timed out)
- 1 scorer false-negative (p11 mache: 0/50 because the response started with the substring "error")
- 5 paired-completed prompts where both modes finished

**Headline numbers on the 7 valid paired prompts**:

|               | Normal | Mache |    Δ |
| ------------- | -----: | ----: | ---: |
| $/prompt      |  $0.43 | $0.40 |  −7% |
| Turns         |   10.0 |  11.6 | +16% |
| Quality (/50) |   41.4 |  38.3 | −3.1 |

Mache mode is roughly **cost-neutral, slightly worse on turns and quality**. That contradicts the architectural pitch (structural tools = fewer turns = lower cost). It also doesn't approach GrapeRoot's published 30-45% cost reduction.

## Why these numbers are not useful

Three things make this bench an unfair measurement of what mache is supposed to be.

### 1. We tested the wrong mache

`mache serve --stdio <db>` reads a static, pre-built .db. No daemon, no file watcher, no incremental re-parse, no LSP enrichment kept current. That's a deployment scenario (ship a frozen .db with a release), not the development scenario mache exists to be.

The intended configuration is:

```
leyline daemon --source /path/to/repo --control /tmp/leyline.ctrl  # warm arena, file watcher
mache serve --control /tmp/leyline.ctrl --http :7532                # served from live arena
```

The daemon-backed path was untested. **Same source code in the MCP handlers, but the data shape and freshness story is different**.

### 2. We tested the wrong interface

Mache has three ways an agent can use its structural intelligence:

1. **MCP tools** — agent calls `find_callers(token)` etc. ← what the bench tested
1. **Projected FS overlay** — agent uses `Read`/`Grep` on `/mnt/mache/pkg/Foo/callers/Bar`, structure-as-API ← untested
1. **Pre-injection** — mache produces a context bundle (overview + key abstractions + entry points + community summary) that gets injected once into the agent's session ← **doesn't exist yet**

The pre-injection path is what GrapeRoot does. It's the entire mechanism by which GrapeRoot beats vanilla Claude on cost: pay the structural work cost ONCE per session, then every prompt is fast because the relevant context is already in the prompt. Mache doesn't have this. The MCP-tools-only path forces the agent to discover at query time.

GrapeRoot's published comparison is illustrative: their MCP-DGC mode (`$0.27/prompt`) is actually MORE expensive than vanilla (`$0.25`) — same trap we may have been falling into. Their pre-injection mode (`$0.17`) is what produces the win.

### 3. Mache is objectively better at the structural work — but doesn't ship it to the agent

Mache's projection captures more than a dual-graph:

- Full schema-driven projection (constructs as directories, files as templated text)
- Cross-language ref index (`node_refs` with bare + qualified tokens)
- Cross-language def index (`node_defs` with package-qualified names)
- Community detection on the refs graph
- Optional LSP enrichment via `ley-line` (`_lsp_hover`, `_lsp_defs`, `_lsp_refs`)
- `find_smells` rules over the AST
- Mermaid diagram generation
- Schema-driven `find_callers` / `find_callees` / `find_definition`

This is genuinely richer than GrapeRoot's dual-graph. **But the agent never sees most of it** because the only delivery channel is on-demand MCP calls, which the model decides whether to invoke. And the model often doesn't — it falls back to its trained-on `Read` / `Grep` reflex on a Go corpus it's already pretty fluent with.

## What needs to exist for the bench to be meaningful

Listed in priority order. Items 1–3 are required; 4–5 are stretch:

### 1. Pre-injection pipeline (the missing layer)

A new command (`mache context-pack <repo> --query <q>`, or as an option to `serve`) that produces a context bundle suitable for injection into an agent prompt. Composition:

- Top-level structure overview (`get_overview` output)
- Key abstractions (`get_architecture` top-N entry points + key types)
- Community summary (top-N clusters with member names)
- Optional: query-specific narrowing (which files / constructs are relevant to `<q>`)

This is what GrapeRoot's `context_packer.pack_for_query()` does. We have all the inputs — we just don't have the packer. **This is the highest-leverage missing piece.**

### 2. Daemon-backed warm cache, end-to-end

`leyline daemon` already exists and watches source. What's missing from the developer experience:

- **Readiness signal**: a daemon op `status` that returns `{phase: "warming" | "ready" | "reingesting", staleness_ms: int, last_change_at: timestamp}`. Mache passes this through to the agent via `get_overview` so the agent knows whether to wait or proceed.
- **Verified incremental re-parse**: today the daemon claims to re-parse on file change but I haven't audited the granularity. If it re-parses the whole file or the whole arena on any change, that breaks the "always ready" promise for large repos. Needs a focused empirical test.
- **Persistent LSP enrichment**: `ley-line enrich-lsp` is currently a one-shot. The daemon should re-enrich incrementally as files change so `find_definition` / `get_type_info` don't go stale.

### 3. The bench rerun, against the correct configuration

Once 1 + 2 land, the bench gets three modes instead of two:

| Mode              | Description                                                                                                                                           |
| ----------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------- |
| `normal`          | Plain claude, no MCP. Baseline.                                                                                                                       |
| `mache-mcp`       | claude + `mache serve --control` (daemon-backed MCP tools). What we tried to test.                                                                    |
| `mache-preinject` | claude with mache's context bundle pre-injected via `--append-system-prompt` or equivalent. The "structural projection IS pre-injection" theory test. |

If `mache-preinject` doesn't substantially beat `mache-mcp`, that's a real result and we know the pre-injection isn't worth the bytes. If it does (GrapeRoot's pattern), we have the comparison story.

### 4. Tool-description audit

If `mache-mcp` mode underperforms even with the warm cache, the next thing to look at is *why the agent doesn't use mache tools*. Possibilities:

- Tool descriptions too generic ("find callers" sounds like grep)
- Tool inputs unclear (does `path` want a node ID or a file path?)
- Response shapes don't compose well (next-step ambiguity)

Solvable via prompt engineering and tool-description rewrites. Cheap experiment.

### 5. FS-overlay measurement

The third axis (projected FS via Read/Grep on `/mnt/mache/...`) is a separate UX. If an agent reads `/mnt/mache/pkg/Foo/callers/` via plain `Read`, does that work better than the MCP `find_callers` tool? Different question, different harness. Defer until 1–3 land.

## What this bench DID establish (the only honest takeaways)

- **Mache MCP responds fast in isolation** — 4 RPC calls in 5.5s, `find_callers(Topology)` returned 117 correct caller paths. No hang, no slow parsing.
- **Claude + mache MCP works on simple prompts** — 3 turns, 10s wall, correct answer.
- **`mache serve --stdio <db>` is a viable deployment but is not the dev configuration** — the bench accidentally validated this.
- **Cost-quality bench HARNESS is reusable** once the missing pieces above are built. The 15-prompt battery, the scorer port, the JSONL output schema are all sound (modulo the "error" substring false-negative — needs a tweak).

## What this bench did NOT establish (despite what the JSONL numbers suggest)

- **It did NOT show mache is slow** — measurement was confounded by subprocess timeouts and the wrong configuration. The mache MCP server itself is fast.
- **It did NOT show mache is a wash** — 12 of 30 calls had reliability issues (timeouts, scorer false-negatives, asymmetric finishes). 7 valid paired prompts isn't enough to call a winner.
- **It did NOT exercise the pre-injection axis at all** — which is GrapeRoot's actual win surface and which mache doesn't have today.

## What the next concrete step is

File a bead capturing items 1–3 above. The pre-injection pipeline (item 1) is independently valuable and probably ~1-2 days of work since we have all the inputs. Then re-run THIS bench against the proper configuration. If the result is still flat, we know mache's structural superiority doesn't translate to agent productivity on this corpus and that's a real strategic finding. If it spikes, the win was real and the original bench was just measuring the wrong thing.

Until then: **don't quote these numbers as a mache evaluation.** They're a measurement-methodology bug report, not a product evaluation.
