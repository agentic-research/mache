# ADR-0021: Semantic file-suggester for Claude Code's `fileSuggestion`

Date: 2026-06-17
Status: Proposed
Bead: TBD (file when rsry reconnects — sibling beads in mache + cloister)
Pairs with: ADR-0014 (mache-in-constellation), ADR-0020 (portable cache lockfile)

## Context

Claude Code 0.2.x exposes a `fileSuggestion` hook in `settings.json` that lets a project override how `@`-completion ranks files. The protocol is small ([`code.claude.com/docs/en/settings`](https://code.claude.com/docs/en/settings), [Anderegg](https://ricardoanderegg.com/posts/claude-code-file-suggestion-hook/)):

```json
{ "fileSuggestion": { "type": "command", "command": "<path-to-binary>" } }
```

The spawned process receives `{"query": "src/comp"}` on stdin, returns newline-separated repo-relative paths on stdout, and Claude Code keeps the **first 15**. Default ranker is filename fuzzy; users report ~1000ms on large repos vs ~62ms for a `ripgrep | fzf` shim ([martinemde.com](https://martinemde.com/blog/fast-claude-file-suggestion-in-big-repos)).

**Load-bearing constraint:** the hook fires on **every keystroke after `@`**. Not debounced. Budget is sub-100ms or it feels broken.

**Market gap (validated):** Cursor, Sourcegraph Cody, Continue.dev, and every fzf-style picker either do filename fuzzy at the picker tier, or do semantic ranking at the **agent context-selection** tier (seconds, batched, no keystroke pressure). Nobody ships a semantic picker under a 100ms-per-keystroke budget. The constraint dominates the design.

The ART stack already has what a semantic-keystroke picker needs:

| System        | What it provides                                                                                                             | Status                                             |
| ------------- | ---------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------- |
| mache         | Structural projection in SQLite, daemon-backed via `mache serve --http`, knows callgraphs / communities / definitions / refs | Hot                                                |
| ley-line-open | Pre-baked `_ast`, `_lsp_*`, `_imports` tables in `.db`                                                                       | Hot                                                |
| lectio        | FS + claude-session ingestion across 20 ART repos                                                                            | Ingestion wired, projection empty as of 2026-06-17 |
| rosary        | Bead file-scopes — "which files this work item touches"                                                                      | Hot when rsry up                                   |

Combining them at picker time without blowing the budget is the actual engineering problem.

## Decision

**Split the work across mache and cloister.** Each repo does what it already does:

- **mache** owns the `mache suggest` subcommand + the `/suggest` daemon endpoint + the per-repo rank table. It already runs the daemon, owns the projection, owns the structural intelligence; this is a thin query layer on top.
- **cloister** owns the Claude Code plugin — `.claude-plugin.json`, the `settings.json` snippet, the binary install. Distribution is cloister's job, not mache's.

### D.1 — Architecture: daemon-backed, precomputed rank

The hot path is a single SQLite query against a precomputed `mache_suggest_rank` table (lives in the existing per-repo `.refs.db` sidecar — see D.4):

```sql
SELECT path FROM mache_suggest_rank
WHERE name GLOB ? OR path GLOB ?
ORDER BY score DESC, recency DESC
LIMIT 15;
```

The `score` column is updated **offline** by a background refresh tick. The hot path never recomputes — it just reads.

```
                    ┌───────────────────────────────┐
                    │      mache serve --http       │
                    │  (already running per ADR-0010)│
                    └──────────────┬────────────────┘
   GET /suggest?q=… (~30ms)        │
        ┌───────────────────┐      │
        │ claude code @     │──────┘
        │ → mache suggest   │     ┌─────────────────────────────────┐
        │   (tiny client)   │     │ .refs.db (per-repo)             │
        └───────────────────┘     │   mache_suggest_rank:            │
                                  │     path, name, score, recency  │
                                  │   refreshed on FS/chat events   │
                                  └─────────────────────────────────┘
                                         ▲
              ┌──────────────────────────┼──────────────────────────┐
              │             refresh tick (debounced, async)         │
              │   mache get_communities + node_defs                  │
              │   lectio claude-session: files mentioned recently    │
              │   lectio fs: mtimes                                  │
              │   rosary active bead: file-scope                     │
              │   ley-line-open imports: inbound count               │
              └──────────────────────────────────────────────────────┘
```

The suggester binary is ~50 lines: read stdin, GET `localhost:7532/suggest?q=…`, print stdout. Sub-30ms when the daemon is warm. GET (not POST) because the query is short, the call is read-only, and a query string keeps the client trivial.

### D.2 — Signal fusion (offline, per refresh tick)

`score` is computed offline from a weighted sum:

| Signal                 | Source                                     | Weight    | Why                                                                         |
| ---------------------- | ------------------------------------------ | --------- | --------------------------------------------------------------------------- |
| filename trigram match | fd-style index                             | base      | universal fallback when nothing else fires                                  |
| edit recency           | lectio fs mtime                            | high      | "what was I just editing" — strongest behavioral signal                     |
| conversation mention   | lectio claude_session, last N turns        | high      | "what was I just talking about" — picker is invoked in conversation context |
| active-bead scope      | rosary bead file-scope, status=in_progress | very high | strong intent signal when present                                           |
| community centrality   | mache `get_communities`                    | medium    | structural backbone — high-degree files matter more                         |
| inbound import count   | LLO `_imports`                             | medium    | structural — many-callers files matter more                                 |
| symbol density         | LLO `_lsp_defs` count                      | low       | nudge in favor of "files with real API surface"                             |

Weights start as priors. v3 graduates to adaptive weights (track which suggestions the user actually picks, regress on outcomes).

### D.3 — Graceful degradation order

The picker MUST never harder-fail than the default ranker. Failure modes in priority order:

1. **mache daemon down** → suggester binary falls back to direct SQLite read of `.refs.db`'s `mache_suggest_rank` table (mmap'd). No-network path.
1. **`mache_suggest_rank` missing or stale** → fall back to fd-style filename glob. Beats default ranker on big repos because we still skip `node_modules` / `target` / `.git` per `lang.Registry` exclusions.
1. **lectio empty** (projection not yet populated for this repo) → drop recency + conversation signals from `score`. Other signals carry it.
1. **rosary disconnected** → drop bead-scope boost. Other signals carry it.
1. **Query is empty** → return top-N by `score` alone (the user hit `@` and is browsing).
1. **Hard error** → exit 0, print nothing. Claude Code falls back to default.

The hot path must NEVER block on a degraded subsystem.

### D.4 — Repo split (the actual question that motivated this ADR)

**mache** ships:

- `cmd/suggest.go` — the `mache suggest` subcommand. Stdin/stdout protocol per Claude Code.
- `internal/suggester/` — rank-table schema, refresh loop, signal-fusion code, daemon endpoint handler.
- New SQL table: `mache_suggest_rank` in the per-repo `.refs.db` (no new file).
- `mache build` learns `--with-rank` to seed the table at build time so the first-keystroke is fast.

**cloister** ships:

- A Claude Code plugin: `.claude-plugin.json` + `settings.json` snippet pointing `fileSuggestion` at the installed `mache` binary in `serve` mode.
- The install path: `cloister install mache-suggester` writes the snippet, ensures `mache serve --http` is running, registers the plugin.
- Versioning: the cloister plugin tracks mache's minor version so a breaking protocol change in either side surfaces at install time.

Neither repo grows a feature it isn't already shaped for: mache stays "code intelligence," cloister stays "ART tooling distribution." Anything else (a separate suggester repo, or burying both in cloister) muddies one or both.

## Consequences

### Positive

- First semantic file picker at keystroke latency anywhere (validated market gap).
- Reuses every existing investment: mache daemon already runs, LLO `.db` already exists, lectio FS sweep already wired, rosary bead scopes already exist.
- Graceful degradation by design — picker never harder-fails than today's default.
- Adaptive weights (v3) feed back observed pick behavior — the picker gets better as you use it without any user config.

### Negative

- Couples the picker's freshness to the refresh tick. A user editing a brand-new file may not see it ranked for ~30s. Mitigated by piggybacking on lectio's FS sweep cadence + adding the file at the bottom of the next pick result regardless of score.
- Adds a `rank` table to every `.refs.db`. Storage cost is small (one row per source file, ~50 bytes) but lockfile churn is real — `mache push` either excludes the rank table from the portable cache (per ADR-0020) or accepts the churn. Lean: exclude.
- Requires cloister to learn one more plugin shape (Claude Code's). Net-new surface area for cloister.

### Neutral

- The protocol is small enough that a different host (Cursor, Cody, Continue) can adopt the same backend with a different shim. The split between "rank engine in mache" and "host-specific plugin in cloister" generalizes.

## Open questions

**1. Where does conversation mention come from?**

Lectio currently has `claude_session` ingestion configured but the projection is empty as of this ADR. Either (a) the suggester drives lectio to populate on first run, or (b) lectio adds a streaming sink that emits "file mentioned" events the refresh tick subscribes to. Lean (b) — the refresh tick already has to be event-driven, why not let lectio push.

**2. Active-bead resolution when rsry's transport is HTTP.**

The session-stale-on-rebuild problem we hit in this very session (rsry MCP fell over after `task install`) means "active bead" can be ambiguous. The refresh tick should cache the last successfully observed active bead rather than fail open.

**3. Multi-repo workspaces.**

If the user has a Claude Code workspace spanning rosary + mache + ley-line, does the suggester rank across all three repos or only the one the picker is invoked in? Lean: invoke in cwd, rank in cwd. Cross-repo rank is out of scope for v1.

**4. Plugin install ergonomics.**

`cloister install mache-suggester` is aspirational — cloister doesn't have an `install` verb yet. Alternative: ship the snippet + binary path as a `cloister generate fileSuggestion --to ~/.claude/settings.json` one-shot. Defer the cloister-side ergonomics to a sibling ADR in cloister.

## Migration path

Three PRs, in order:

1. **mache — daemon + rank table + suggester subcommand.** New SQL table, refresh tick that runs on FS events + mache build. Stdin/stdout protocol implemented. No external dependencies on lectio/rosary; v1 ranks on filename trigram + mtime alone. End-to-end working at `task install && mache serve --http && mache suggest <<< '{"query":"foo"}'`.

1. **mache — fuse structural signals.** Wire community centrality, import count, symbol density into the refresh tick. Validates the signal-fusion math.

1. **cloister — plugin packaging.** `.claude-plugin.json` + settings.json snippet + install verb. Wires lectio + rosary signals at this layer because the plugin owns the host-side context (it knows which conversation, which bead).

Each PR is independently shippable. PR 1 alone beats fd/fzf on real repos because mtime + filename is already better than default ranker.

## What this ADR does NOT decide

- The cloister plugin install command shape (`install` vs `generate` — sibling ADR in cloister).
- Adaptive-weight learning algorithm (v3 concern, separate bead).
- Cross-repo workspace ranking (deferred).
- Whether to expose the same backend to other hosts (Cursor, Cody) — out of scope until v1 ships and is validated.
- Privacy posture for conversation mentions — lectio's `claude_session` is local-only today; if that changes, this ADR needs an amendment.
