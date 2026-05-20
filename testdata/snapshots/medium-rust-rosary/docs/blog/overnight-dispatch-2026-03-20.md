# I don't write Rust. Last night my agents merged 10 PRs.

*March 20, 2026 — James Gardner*

______________________________________________________________________

I didn't write a line of Rust today. I've never written Rust before. But by the end of the session, autonomous AI agents had:

- Merged **10 PRs** to rosary's main branch
- Split a 2,716-line god file into clean modules
- Fixed 5 dispatch-blocking bugs and 2 conductor P0s
- Organized 371 open beads into 4 decades and 13 threads
- Written an ADR, designed a 4-stage enrichment pipeline, and codified Golden Rules as pre-commit hooks
- Dispatched themselves, reviewed their own work, and opened their own PRs

And this was only 1/4 terminal windows.

This is the story of rosary's first real overnight dispatch — what worked, what broke, and what it feels like when the recursion starts to bite.

## The setup

Rosary is an autonomous work orchestrator. It tracks work as "beads" (think issues, but stored in Dolt databases per repo), dispatches AI agents to work on them in isolated worktrees, verifies the results, and syncs to Linear for human review.

The human manages work, not agents. The agents manage code.

I'm a platform engineer. I designed the architecture, wrote the ADRs, defined the product tiers. I've never written Rust — every line of rosary's 21,000 lines was written by AI agents, guided by me.

## The session

It started with a broken `git stash`. The jj+git colocated repo had a desync — a file Claude Code created got stuck in git's index, blocking pull and stash. Running `jj status` fixed it in 2 seconds (jj re-snapshots and reconciles the index).

From that bug, we unraveled a chain of infrastructure issues:

- **Symlink split-brain**: `~/github` → `~/remotes` symlink caused beads created via one path to be invisible from the other. Same Dolt DB, different path strings. Fixed by canonicalizing paths in the connection pool.
- **Stale Dolt PIDs**: Dead Dolt servers left PID/port files behind, causing 10-second timeouts on every startup. Fixed by checking if PIDs are alive before trusting port files.
- **Hooks blocking their own fix**: We added a pre-commit hook to enforce bead references in commit messages. Then we couldn't commit the hook update because the branch had the *old* hook that didn't understand the new format. The fix for the hook was blocked by the hook. We ended up using `--no-verify` to bootstrap, then moved hooks to `~/.rsry/hooks/` — central, not per-branch.

## The dispatch loop

Once the infrastructure was stable, we started dispatching:

**Level 1** — A trivial test: add a line to CLAUDE.md. Agent completed in 30 seconds, committed, closed its own bead, and we merged to main. Proof of life.

**Level 3** — Split serve.rs (2,716 lines) into 4 modules. Agent ran for 10 minutes, created `serve/mod.rs`, `serve/tools.rs`, `serve/handlers.rs`, `serve/webhook.rs`. All 385 tests passed. Merged.

**Parallel dispatch** — Three agents simultaneously refactoring god files in isolated worktrees. Non-overlapping file scopes, no collisions. But — two of them branched from old main and independently re-did the serve.rs split that was already merged. Duplicate work. We caught it in review, closed the duped PRs, and re-dispatched from current main. This is exactly why the feature agent layer matters (more on that below).

**Conductor** — The Elixir/OTP conductor manages agent lifecycles. We started it and it immediately dispatched 3 agents with zero stdin warnings (after fixing the stdio issue). But it picked P0 beads that were too ambitious for dev-agents — the triage needs work. And then it got stuck.

## What broke

**The conductor timeout handler hung forever.** After an agent timed out, `Port.close` was called to kill it. But `Port.close` invalidates the Erlang port handle *before* the signal propagates — the `{port, {:exit_status, code}}` message never arrives. The GenServer sat in `:noreply` state forever, the DynamicSupervisor still counted it as an active child, the slot was never freed. The fix: SIGTERM the OS process directly, poll for death (up to 5s), SIGKILL if stubborn, then call `on_failure` inline. No waiting for a message that will never come.

**PTY was categorically wrong.** Our first attempt at fixing the stdin issue used `script` to allocate a PTY. Bad idea — PTYs do CR/LF conversion (corrupts JSON), echo input back as output, and `script` stays alive after the child exits (masking exit status). The correct fix was embarrassingly simple: `exec "$@" < /dev/null` in a wrapper script. `exec` replaces the shell so the Port tracks the real PID. Stdin from `/dev/null` gives immediate EOF (no "no stdin data" warning). For ACP mode (bidirectional JSON-RPC), use a standard Port with no wrapper.

**Agents that branch from old main duplicate work.** When we dispatched three god file splits in parallel, two of them independently re-did the serve.rs split because they branched before it merged. This is the core problem ADR-0008 addresses: dev agents don't see each other's work. The feature agent layer would coordinate — "serve.rs is already split, just do dolt.rs."

**Branch protection fought ff-merge.** Agents committed to worktrees and we ff-merged to main. GitHub's branch protection requires PRs. The system fought itself. Now agents push branches and the human (or feature agent) opens PRs.

**Beads piled up faster than dispatch could clear them.** Every conversation generated 10-20 beads. Dispatch handled maybe 10. Without admission control, the backlog grows unbounded. The answer isn't "stop filing beads" — it's triage harder, cap per type, and let stale beads age out.

## The architecture that emerged

```
Human (release manager)
  └── Orchestrator (conductor)
        └── Feature agent (manages a thread, opens PRs)
              └── Dev agents (scoped to files, parallel)
```

Each tier has a **scope** (what it sees) and a **reign** (what it can do). As you go up: scope broadens, detail decreases, reign shifts from "edit code" to "dispatch agents" to "approve releases."

Dev agents don't know about each other. Feature agents compose their work. The orchestrator decides what to work on. The human reviews PRs.

## The numbers

*Updated March 21, 2026 — includes the pipeline engine session.*

| Metric             | March 20             | March 21                                  | Total      |
| ------------------ | -------------------- | ----------------------------------------- | ---------- |
| PRs merged         | 10 (#29-40)          | 15 (#59-77)                               | 25         |
| Bugs found + fixed | 7                    | 10                                        | 17         |
| God files split    | 2 (serve, workspace) | 1 (reconcile: 2145→551 lines, 8 modules)  | 3          |
| New modules        | 4                    | 11 (pipeline.rs, 8 reconcile/*, 2 dolt/*) | 15         |
| Tests passing      | 385 + 110 + 106      | 429 (Rust, conductor removed)             | 429        |
| Beads filed        | ~95                  | ~30                                       | ~460 total |
| Beads closed       | ~40                  | ~20 (8 dupes + fixes)                     | ~60        |
| Agent dispatches   | 20+                  | 10+ (including first 4-phase pipeline)    | 30+        |

### March 21 highlights

- **Pipeline engine**: config-driven agent sequences, persistent state in Dolt, unified completion handler
- **Scoping agent**: pre-dispatch enrichment (search → analyze → plan)
- **First 4-phase pipeline**: scoping → dev → staging → prod (all phases advanced automatically)
- **Multi-tenant chain**: identity extraction, handler scoping, schema migration, repo registration (PRs #67-71)
- **Storage architecture**: math friend proved Dolt features unused → D1/R2/Fly migration validated
- **Conductor moved to rig**: rosary is pure Rust, conductor is product layer
- **Human direction: 1.3% of session tokens** → 98.7% autonomous execution

## What it feels like

There's a moment around hour 6 where you stop thinking about the code and start thinking about the system. You're watching an agent split a 2,700-line file into modules — code you've never read, in a language you've never written — and the tests pass. You didn't tell it which functions go where. You told it "this file has too many responsibilities" and it figured out the rest.

Then you dispatch three more agents in parallel on three different files. They can't collide because the file scopes don't overlap. That's not magic — that's the system you designed working as intended. The beads have file scopes, the reconciler checks overlap, the worktrees isolate. You designed the rules. The agents follow them.

The recursive moment: the pre-commit hook that enforces bead references in commits was blocking its own commit because the branch had the old hook. The fix for the hook was blocked by the hook. You laugh, you `--no-verify`, you move hooks to a central directory so they're never per-branch again. The system found a bug in itself and you fixed the system, not the bug.

By midnight, you're not coding. You're reviewing PRs that agents wrote, filed, and tested. You're a release manager for a codebase you've never typed into. That's the feeling: you designed the factory, the factory runs, you inspect the output.

## What's next

~~The gap is the feature agent.~~ **Update:** The scoping agent is built and working (March 21). It runs as pipeline phase 0 before dev-agent, researching docs and producing a structured plan. The 4-phase pipeline (scoping → dev → staging → prod) executed end-to-end for the first time.

~~The hosted endpoint at `mcp.rosary.bot` is next.~~ **Update:** Multi-tenant identity, handler scoping, schema migrations, and repo registration are all merged. The deploy to Fly is the last step.

The remaining gaps:

- **Feature agent** (thread-scoped orchestration of multiple dev agents) — the scoping agent is a step toward this
- **D1 migration** — storage architecture validated (Dolt features provably unused), D1/R2/Fly is the target
- **Channel plugin** — push-based CI/PR notifications into Claude Code sessions, replacing polling
- **Bead consolidation** — 460 beads, 8.7% completion rate, PM sweep identified 8 dupes and 6 decades

Designed by a human. Built by Claude. Managed by Claude. Reviewed by a human.

**1.3% human direction. 98.7% machine execution. 25 PRs merged in 2 days.**

______________________________________________________________________

*Rosary is open source (AGPL-3.0): [github.com/agentic-research/rosary](https://github.com/agentic-research/rosary)*
