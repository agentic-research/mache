# Measurement contracts for mache's claims — 2026-06-24

Design spec. Defines **what each of the Feb-2026 blog's four claims means as a
measurement** — input, oracle, output, baseline, falsification threshold — so
that when numbers land they either validate the architecture or show where it's
wrong. Implementation is phased; this doc is the contract, not the harness.

> Source claims: "The IDE Solved This Twenty Years Ago" (J. Gardner, 2026-02-22),
> §"What I'm Measuring": (1) wasted CI cycles, (2) context-token efficiency,
> (3) iteration count, (4) time-to-first-valid-patch.

## The one rule everything obeys: an external objective oracle

Code quality is subjective; the field's benchmarks **refuse to measure it**.
HumanEval/MBPP grade by *running the tests*; SWE-bench grades by whether a repo's
*own hidden tests* go FAIL→PASS without breaking PASS→PASS. The oracle is never
"is this good," never the tool judging itself, never the agent self-reporting.

mache's value prop is **not "better code"** — it's *fewer wasted cycles/tokens/
corrupt-file detours to the **same** objectively-correct patch*. That's a process
claim against a fixed outcome, which is exactly what's objectively measurable.

**Every contract here uses an external oracle and forbids three anti-patterns we
already found in our own prior benchmarks (see Appendix A):**

1. **No mache-as-its-own-truth.** "Real references" come from the compiler/gopls,
   never from mache's index (the `symbol_lookup_fp_rate` circularity).
1. **No agent self-eval.** "Did it help me" judgments are not measurements (the
   Serena harness's framing).
1. **No estimated tokens.** Count with a real tokenizer, both arms, same estimator.

Plus: **pin the corpus ex-ante** (the FP-rate doc proved a small clean repo
*understates* the claims — you need MLOC, framework-heavy code), and **report
what each result does NOT prove.**

## The contracts

### M1 — write-validation catch-rate (the strongest claim)

Two distinct measures with very different cost; do not conflate them.

**M1a — catch-rate (cheap, deterministic).**

- **Question.** Of the edits an agent makes, what fraction produce a file that
  fails to tree-sitter-parse — corruption mache rejects at the write boundary
  that a plain filesystem lets land?
- **Input.** Post-edit file contents from real agent trajectories (Write/Edit
  tool calls in Claude Code transcripts via lectio).
- **Oracle.** Objective, mache-independent: a file **parses or it doesn't**
  (`internal/writeback/validate.go`).
- **Measure.** Replay each post-edit file through the validator; count rejects.
- **Output.** catch-rate %.
- **Baseline.** Plain filesystem (every edit lands).
- **Falsification.** catch-rate ≈ 0 ⇒ the "red squiggly before save" claim is
  hollow. Pre-register the threshold.
- **Cost.** *Genuinely small* — extract Write/Edit results, parse each. No CI
  data needed.

**M1b — wasted-cycles-avoided (NOT small; may yield only an estimate).**

- **Question.** Of the edits M1a would have rejected, how many actually *landed*
  in the real trajectory and cost a downstream build/test-failure cycle?
- **Why it's harder.** This needs (1) build/CI signal *in the trajectory* (Bash
  tool outputs showing a failed build), and (2) a *causal link* from a specific
  broken edit to that later failure. Transcript data may not support the causal
  link cleanly — a failure 6 turns later may have many causes.
- **Honest fallback.** If the causal link can't be established from the data,
  M1b degrades to a *modelled estimate* (catch-rate × measured mean
  cycles-per-syntax-error from a separate sample), clearly labelled as an
  estimate, not a direct measurement. Decide which before running.
- **Status.** M1a is the **first dispatchable**. M1b is a follow-on whose
  feasibility is gated on whether the trajectory corpus carries usable CI signal
  — assess that *before* promising "wasted CI cycles" as a measured number.

### M2 — context-token efficiency

- **Question.** For a function-editing task, how many tokens does `source` +
  `context` cost vs reading the full file?
- **Input.** A sample of functions across a pinned corpus.
- **Oracle / measure.** Real tokenizer (same estimator both arms — for a *ratio*
  the estimator need only be consistent). "Visible imports/types" correctness
  checked against gopls, never mache.
- **Output.** token ratio (source+context : full-file), per function + aggregate.
- **Baseline.** Full-file read for the same edit task (like-for-like — NOT grep).
- **Falsification.** ratio ≈ 1 ⇒ no efficiency win.
- **Cross-thread.** Same shape as `mache-7d321e` item 2 (resource-URI
  externalization — "JSON-RPC is the wrong carrier for bulk graph data; MCP
  resources move payload out-of-band"). Both are *shrink the tokens the agent
  pays for the same information*. M2 should measure the lever that work builds;
  don't re-derive the payload-reduction analysis in two places.
- **Status.** **BLOCKED on `mache-b8fe72`.** Verified 2026-06-24: the `context`
  virtual file is absent in the shipped projection (0 of 236 source files). You
  cannot measure the efficiency of a projection that isn't produced. M2 resumes
  once `context` actually exists.

### M3 — iteration count · M4 — time-to-first-valid-patch

- **Question.** Same task, agent-on-files vs agent-in-mache: how many edit-test
  cycles (M3) / how much wall-clock (M4) to a passing patch?
- **Input.** **SWE-bench Verified** (500 human-validated tasks) or a pinned
  subset; SWE-agent runner.
- **Oracle.** The task's hidden tests pass/fail — objective, human-independent.
  Hold the destination fixed; measure the journey.
- **Measure.** Run each task twice; the *only* variable is the file surface
  (plain files vs a mache projection). Instrument cycles + wall-clock (SWE-agent
  logs already capture turns/tokens; M1's parse-replay rides the same edit events).
- **Output.** Δ iterations, Δ time, plus % resolved (the standard SWE-bench
  headline) per arm; report N, controls, significance (Mann-Whitney, like the
  existing micro-benchmarks).
- **Baseline.** Agent-on-files arm.
- **Falsification.** No significant reduction at fixed % resolved ⇒ mache doesn't
  reduce the journey cost.
- **Cost bound (pre-registered kill-switch).** SWE-bench Verified × 2 arms ×
  full agent compute is real money; the run must be staged, not open-ended:
  **Phase 0 — N=50 pilot** (fixed task subset, both arms). Gate: if the Δ on
  iterations/tokens isn't directionally present and powered enough to justify
  scaling, **stop** (a null pilot is itself a publishable finding). **Phase 1 —
  full 500** only after the pilot clears the gate. Pre-decide N, the gate
  criterion, and the per-arm budget ceiling before spending anything.
- **Status.** Real harness build — SWE-agent integration + mache-as-file-surface
  - compute. The expensive tier; do after M1.

### M5 — read-side navigation/understanding efficiency (the half the others miss)

M1/M3/M4 are all **write-side**. But mache's pitch is an *agent-first code-intel
surface* — navigation and understanding (`callers/`, `find_definition`,
`context`, `get_overview`) — which is read-side. The existing Serena harness
covers this axis but by **self-eval** (rejected, §rule 2). This contract is the
objective-oracle *replacement*, so the read-side claim isn't left unmeasured.

- **Question.** For a fixed set of code-intel questions ("where is X defined",
  "who calls Y", "what's the type of Z", "what does this function depend on"),
  how many tokens + tool-calls does an agent spend to reach the **correct**
  answer with mache vs with grep + a stock LSP?
- **Input.** A pinned Q&A set over a pinned corpus, each question with a
  **verifiable correct answer** established by gopls/compiler (the oracle) — NOT
  by mache and NOT by the agent's opinion.
- **Oracle.** The answer is right or wrong against gopls ground truth. Efficiency
  is only counted on questions the agent answered *correctly* (a cheap wrong
  answer doesn't win).
- **Measure.** tokens + tool-calls to a correct answer, per question, per arm.
- **Output.** Δ tokens, Δ calls, and answer-accuracy per arm.
- **Baseline.** grep + stock LSP (gopls).
- **Falsification.** mache no cheaper at equal accuracy ⇒ the navigation claim
  doesn't hold. (And if mache is cheaper but *less accurate* — e.g. the
  type-ref/def-index gaps the FP-rate doc found — that's a finding, not a win.)
- **Status.** Read-side harness; depends on the same `.db` access as the others.
  M2 is a *component* of this (context-token cost is one question-type's payload).

## What this does NOT measure (explicit non-measurements)

Naming these so they're not mistaken for "validated":

- **The FUSE-mount-as-agent-UX path.** The whole blog experience — `cd functions/X/; cat source; ls callers/; write source` as a *filesystem* — is
  **not measured.** The harness reads the `.db`/MCP surface because the mount
  needs interactive `sudo` (`mache-bb3e39`). So these contracts validate mache's
  **structural-data value**, not its **filesystem-projection UX** — which is
  roughly half the value prop. Measuring the mount path is gated on
  `mache-bb3e39` (no-sudo mount) and would need its own contract.
- **Code quality of the resulting patches** (deliberately — see §the-one-rule).
- **Production p99 / scale behaviour** — these are task-completion measures, not
  load tests.

## Cross-cutting constraints (surfaced by actually running, 2026-06-24)

- **Read from the `.db`, not the mount.** The NFS mount needs interactive `sudo`
  (`mache-bb3e39`) and is CI-hostile. Batch measurement uses `mache build --out`
  - direct node access. The mount is for live humans/agents only.
- **The treatment arm is the *writable* surface.** M1/M3/M4 exercise the
  write-back/validate path (`--agent`/`write source`), not the read-only MCP
  framing — the existing Serena harness's read-only scoping does NOT cover them.
- **Pre-register corpus + thresholds** before running, per arm.

## Phasing

1. **M1a** (catch-rate) — replay-through-validator. Cheapest real number; no blockers.
1. **M1b** feasibility check — does the trajectory corpus carry usable CI signal?
   Decide measure-vs-estimate *before* promising "wasted CI cycles."
1. **M5 read-side** + **M2** — M5's Q&A harness (objective oracle); M2 once
   `mache-b8fe72` (context projection) lands.
1. **M3/M4** — SWE-bench: **N=50 pilot → gate → 500**. The headline; the build.

## Appendix A — prior measurement & why it's insufficient

Two benchmarks already exist; both are honest about their flaws but weak as
*evidence*, and both inform the rules above:

- **`benchmarks/quantitative/symbol_lookup_fp_rate.md`** (grep vs `find_callers`).
  Apples-to-oranges: (1) circular — uses mache's own (incomplete) index as the
  FP-rate denominator; (2) different questions — all-text grep vs call-site index;
  (3) token win is *estimated* with favorable constants, not measured; (4) one-
  sided timing; (5) corpus too small/clean to show the claims. → drives rules 1 & 3.
- **`benchmarks/serena/`** (adapted Serena 20-task eval). Three issues: (1) it's
  agent *self-eval* — qualitative judgment, not measurement; (2) it frames mache
  read-only and concedes edits, so it can't touch M1/M3/M4; (3) no neutral oracle.
  → drives rule 2. Keep it as M2-navigation *context*, not as M1/M3/M4 evidence.

## Appendix B — findings from the first run (2026-06-24)

Attempting to run M2 (rather than spec it abstractly) surfaced:

- **`mache-b8fe72`** — `context` virtual file absent in the agent mount (0/236).
  `node.Context` is populated for internal callee resolution (`engine_walk.go:394`)
  but doesn't surface via the VFS `ContextHandler` on the mount. M2's basis is missing.
- **`mache-bb3e39`** — agent mount requires interactive `sudo` (`server.go:82`),
  though the no-sudo userspace path exists (fuse-t / leyline-fuse). Mount is
  unusable for automated measurement → read the `.db`.

These are the "validate the architecture or tell us where it's wrong" outcomes
the blog promised — here, two prerequisite gaps before M2 is measurable.
