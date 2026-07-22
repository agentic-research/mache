# Agent-Native Mount — No-Sudo Mounting & Source-Write Consolidation — Design

**Status:** Draft (brainstorming output), 2026-07-22. An epic-scoping artifact, not
yet decomposed into beads. Cross-repo: mache, ley-line-open (LLO). Gated on a
Phase-0 probe that can kill or reshape the epic before the expensive work.

**Companion context:** ADR-0006 (pure-Go, CGO+FUSE removed — a **hard invariant**,
reaffirmed 2026-07-22: *"we do NOT want cgo back in mache"*), ADR-0012 (CGO
removal / leyline is the sole parser), roadmap epic `mache-33dc5f` (bundle leyline
in mache release). Supersedes nothing; a new ADR will record the no-sudo-mount
decision once Phase 0 validates it.

______________________________________________________________________

## 1. Problem

mache's mount shells `sudo mount -t nfs` (`internal/nfsmount/server.go:82`, no
fallback, `cmd.Stdin=nil`). sudo tickets are per-tty, so an interactive `sudo -v`
never reaches a non-interactive/agent shell — **an agent literally cannot mount
mache.** That undercuts the agent-substrate premise and is why the arena
benchmark rotted (its sudo mount isn't CI-runnable, so nothing exercised it and
the harness silently drifted to a stale flat-topology assumption).

Two coupled wants emerged while scoping the fix:

1. **No-sudo mount.** Achievable only via fuse-t, which requires linking
   `libfuse-t` (C). mache is pure-Go by ADR-0006, so mache cannot mount via
   fuse-t itself — it must **delegate mounting to leyline** (whose Rust `fuser`
   crate links libfuse-t).
1. **Don't confuse responsibilities.** ADR-0006 removed FUSE from mache and made
   leyline "the thing" for mounting; mache is the graph engine. Today mache still
   violates that — it runs its own userspace NFS server *and* owns write-back.

### North star (the real motivation)

The mount fix is a means to an end: **the CAS/arena as a worktree-free agent
workspace substrate.** rosary today dispatches each agent into a **git worktree —
a full disk checkout (N agents = N× disk)**. The goal: each agent gets a **mount
backed by the content-addressed arena**, edits "files" normally (**none the
wiser**), and its writes land as **CAS deltas on an isolated branch** — N agents
share one CAS with dedup + COW, no worktree duplication.

This is **already the stated design of `leyline-vcs`** (`ll-open/vcs`): a jj
(Jujutsu) sidecar that snapshots the arena, whose own doc says *"jj never touches
the hot path; the agent never needs to know it exists; the `.leyline/` virtual
directory in the mount exposes time-travel."* So the substrate is **half-built in
LLO**. Under this lens the epic's center of gravity is **LLO** (leyline-vcs +
arena + fuse), mache is a consumer, and **rosary is the payoff** (swap
worktree-dispatch for branch-dispatch). Related roadmap: ADR-0003 (layered
overlays over CAS), ADR-0004 (MVCC ledger).

**Gap vs. today:** leyline-vcs does *linear* snapshot history over a *single*
`current_root` (`ll-core/core/control.rs:250`). Worktree replacement needs **N
concurrent *isolated* branches** (each agent COW-forked over the shared arena) +
per-branch mount roots. jj supports concurrent branches natively, so this extends
leyline-vcs rather than inventing anything.

## 2. Decisions of record (forks already resolved)

| Fork                              | Decision                                                  | Rationale                                                                                                                                                                                                                                                                                         |
| --------------------------------- | --------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| CGO-FUSE back in mache (Option B) | **REJECTED**                                              | ADR-0006:90 already named+rejected "keep FUSE behind a build tag"; re-adds cross-compile/binary-size/libfuse-t/CI-flake costs. Pure-Go is a hard invariant.                                                                                                                                       |
| No-sudo transport (Option A)      | **Delegate to `leyline serve --backend fuse`**            | Only ADR-consistent path; leyline links libfuse-t, mache never calls a mount syscall.                                                                                                                                                                                                             |
| Write-back ownership              | **Move to leyline** (retire mache's `internal/writeback`) | Symmetry: leyline already owns source *read* (`parse`) and is ~70% of the way to *write* (retains `_source.path`, reads real files, validates via tree-sitter, splices to db). The arena is the IPC substrate. Completes the split rather than confusing it.                                      |
| Format on the write path          | **Removed from the substrate write path**                 | Format is polish, not safety; enforced anyway via the pre-push `task fmt` hook + CI. Write path = `validate → splice → write-file`.                                                                                                                                                               |
| Unified formatting mechanism      | **WASM canonical formatters** (own bead, not this spec)   | "topiary but byte-for-byte" is contradictory — topiary is a *different* formatter and can't match gofumpt without reimplementing it (brittle, drifts on version bumps). Only WASM runs the *real* formatter hermetically in-LLO. Topiary is a fallback for languages with no canonical formatter. |

## 3. Target architecture (for the code-projection path)

| Responsibility                                                         | Owner       | Notes                                                                                          |
| ---------------------------------------------------------------------- | ----------- | ---------------------------------------------------------------------------------------------- |
| Mount + transport (fuse-t no-sudo; NFS)                                | **leyline** | data plane; "leyline is the thing"                                                             |
| Source read (parse → arena db)                                         | **leyline** | `leyline parse` (already)                                                                      |
| Source write (validate → splice → write real file)                     | **leyline** | gap today: leyline splices to arena, not `_source.path`'s real file                            |
| Draft mode + write-status feedback                                     | **leyline** | gap today: leyline rejects invalid at the syscall (EROFS/EIO), no draft-keep                   |
| What the tree *is* (schema, topology, `callers/`/`callees/`/`context`) | **mache**   | graph engine; materialized into `nodes` for leyline's schema-blind renderer, or served via MCP |
| MCP query tools                                                        | **mache**   | primary agent surface — needs no mount, no sudo                                                |
| Re-project on write                                                    | **mache**   | polls `current_root`; consumes an arena write-event                                            |

Arena = shared IPC substrate. mache does **zero** source-file IO and **zero**
mount syscalls on the code path.

## 4. Constraints & blind spots (must be designed for, not assumed away)

1. **The clean split only holds for *code* projections.** mache's non-code
   projections (NVD, KEV, Notion, Trivy, JSON, SQLite — the "one engine, many
   projections" core) flow through `SQLiteGraph`/`Engine`, **not** the leyline
   arena. A data-projection mount has no leyline arena, so `leyline serve --backend fuse` cannot serve it. **Consequence:** mache still needs its own
   mount path (today's sudo-NFS) for data projections, unless those also get a
   no-sudo transport — which fuse-t can't give without CGO. So the honest end
   state is **two mount paths**: leyline-fuse (code, no-sudo) and mache-go-nfs
   (data, sudo). "mache never mounts again" is false for data.
1. **Draft mode + `_diagnostics/last-write-status` don't move for free.** These
   are first-class agent-experience features (invalid write kept as a visible
   draft; readable write feedback). leyline currently *rejects* invalid writes at
   the syscall — reject, not draft-keep. Porting draft semantics + the feedback
   surface into leyline is real work, not a detail.
1. **Critical path is entirely in LLO, blocked at packaging.** Nothing in mache
   can start until LLO ships an **opt-in mount-capable leyline**. That's gated by
   the libfuse-t hard-runtime-dep problem: a mount-enabled leyline won't launch
   *at all* without libfuse-t, which is why the default install skips it
   (`fix/install-full-skip-mount`). Step 1 is a Rust/packaging problem, not a
   mache change.
1. **CI-testability — low risk, but prove the *mount*, not just the build.** The
   arena rotted because sudo-NFS isn't CI-friendly. The no-sudo fuse mount is
   almost certainly CI-testable **on Linux**: `fuser` uses `libfuse` +
   `fusermount` (setuid, genuinely no-sudo), and LLO CI **already installs
   `libfuse-dev` on `ubuntu-latest`** — but note LLO currently *builds* the fuse
   crate there, it does not *mount*. So P0.2 must run an actual `leyline serve --backend fuse` mount on a Linux runner, not just install the lib. macOS fuse-t
   in CI is then unnecessary (Linux covers the CI-rot fix); fuse-t stays the
   local-dev no-sudo path. **Correction:** there is no "arduino fuse action" — the
   pinned `arduino/setup-task@b91d5d2c96a56797b48ac1e0e89220bf64044611` is the
   *Taskfile* runner; FUSE is plain `apt-get install libfuse-dev`.
1. **Packaging assumption to flip.** LLO's `release.yml` states the `mount`
   feature is gated off *"so the release binary doesn't depend on libfuse at the
   consumer end — mache uses the daemon for MCP/UDS, not for FUSE mounting."* This
   epic **contradicts** that sentence. Phase 3 must change LLO release packaging to
   offer an opt-in mount build **without** making libfuse a hard dep of the default
   binary.
1. **Write→re-project loses the surgical update.** mache today does
   `ShiftOrigins` (surgical node-position update, no re-ingest). If write moves to
   leyline and mache re-projects on `current_root` advance, mache likely pays a
   **full re-projection per write** plus a staleness window (MCP view lags the
   file) unless the arena write-event carries enough delta to update surgically.
   Depends on the sheaf-watcher / arena-event payload (flagged possibly-unreleased
   in LLO).

## 5. Epic decomposition

This is an epic, not a task. It must not be built straight-through — Phase 0
exists to **kill or reshape it cheaply** before the expensive migrations.

### Phase 0 — MVP probe (validate the premise; ~days, mostly LLO)

Goal: cheaply validate the premise and surface what the reduced view costs, before any migration.

- **P0.1** LLO: produce a mount-capable leyline binary (`cargo build -p leyline-cli --features full`) and confirm `leyline serve --backend fuse --mount <dir>` mounts **without sudo** on this dev machine (fuse-t installed).
- **P0.2** Prove a `leyline serve --backend fuse` mount **actually runs, no-sudo,
  on `ubuntu-latest`** (Linux `libfuse` + `fusermount`) — not just that the crate
  builds. Extend LLO's existing `libfuse-dev` install step to a mount smoke-test.
  Any new GitHub Action **must be SHA-pinned** (mache's `scripts/actions-pin-lint.sh`
  / `task lint:actions` enforces it; `arduino/setup-task@b91d5d2c96a56797b48ac1e0e89220bf64044611`
  is the golden pinned reference). macOS/fuse-t in CI is out of scope for P0.2.
- **P0.3** mache: add an experimental `--backend fuse` to the code-mount path that
  shells the mount-capable leyline against a source dir, accepting **leyline's
  existing (reduced) projection and arena-only write** for now. Measure: does an
  agent get a usable read/navigate mount with no sudo? What exactly is missing
  vs mache's GraphFS (callers/, write-to-real-file, diagnostics)?
- **P0.4 (north-star probe)** LLO: can `leyline-vcs` give **two concurrent,
  isolated agent branches** over **one** arena — agent A's writes invisible to
  agent B, no per-agent disk copy? This is the load-bearing capability for the
  worktree-replacement payoff. If jj concurrent changes map cleanly to per-agent
  arena roots, the north star is reachable; if the single-`current_root` model is
  a hard wall, the isolation story needs rethinking before rosary can adopt it.

**Exit criteria / decision gate:** P0.2 is expected to pass (Linux fuse is
no-sudo); if it does *not*, the CI-rot motivation is unfixed and we re-scope. If
P0.3 shows the reduced view is acceptable for the mount's (secondary) use, most of
Phases 1–2 shrink. Re-decide scope here before proceeding.

### Phase 1 — Read-side parity (code mounts, no-sudo, full view)

Make leyline's schema-blind fuse renderer show mache's full read-side view by
**materializing** mache's virtual surfaces (`callers/`, `callees/`, `context`,
`_schema.json`) into the `nodes` table leyline renders. mache stays the projector;
leyline stays a dumb renderer. No write-back changes yet.

### Phase 2 — Write-side consolidation into leyline

Add to leyline: write spliced bytes back to `_source.path`'s real file (it already
has the path + validate + splice-to-db); port **draft mode** + a write-status
signal. Retire mache's `internal/writeback`; route mache's MCP `write_file`
through leyline. Solve the re-project story (arena write-event → surgical update if
possible, else full re-project + document the staleness window).

### Phase 3 — Packaging (rides `mache-33dc5f`)

Ship the mount capability as an **opt-in installable** (not default, per the
libfuse hard-dep). Bundle base (mount-less) leyline with mache as the required
backend; the mount feature is a separate installable layer that mache detects
(libfuse/fuse-t present + leyline reports `mount`). Concretely, this **flips LLO's
`release.yml`** which today gates `mount` off and documents *"mache uses the
daemon for MCP/UDS, not for FUSE mounting"* — Phase 3 adds an opt-in mount artifact
without making libfuse a hard dep of the default binary. All CI/release action
refs stay **SHA-pinned** (enforced by mache's `actions-pin-lint`).

### Phase 4 — Formatting substrate (separate strategic bead)

WASM canonical formatters in LLO (`gofumpt`/`black`/`prettier` → WASM, run
in-process via LLO's per-language registry). Delivers hermetic + unified +
byte-for-byte. Topiary only as a no-canonical-formatter fallback. **Independent of
Phases 0–3** and decided on its own merits.

## 6. Open questions (resolve in Phase 0 / early)

- Does the arena write-event payload exist + carry enough delta for surgical
  re-projection, or is a full re-project per write the near-term reality?
- Is fuse-t installable on CI runners (P0.2) — the load-bearing unknown for the
  CI-rot motivation.
- Data-projection mounts: keep sudo-NFS, drop mounting for data, or invest in a
  separate no-sudo data transport? (Probably: keep sudo-NFS for data, documented.)

## 7. Non-goals

- Reversing ADR-0006 / any CGO in mache. Hard no.
- A no-sudo mount for **data** projections (out of scope; they keep sudo-NFS).
- Making topiary match gofumpt byte-for-byte (rejected as contradictory).
- Windows mount support.

## 8. Related

- Beads: `mache-b7ec42` (arena re-run — this epic subsumes the harness rewrite),
  `mache-33dc5f` (bundle leyline), plus new beads to file per phase.
- LLO branches: `feat/fuse-backend-rs-graph-adapter` (merged),
  `fix/install-full-skip-mount` (unmerged — the packaging tension),
  `fix/nfs-feature-libc` (unmerged).
- ADRs: 0006 (pure-Go/no-FUSE, hard invariant), 0012 (CGO removal). A new ADR
  records the no-sudo-mount + write-consolidation decision after Phase 0.
