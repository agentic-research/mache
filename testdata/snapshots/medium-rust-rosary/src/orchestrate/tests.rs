//! Invariant tests for FeatureOrchestrator state machine.
//!
//! These tests express "Must Fix" findings from the 2026-04-07 architecture
//! review (Mark Esler). Each test is expected to FAIL until the underlying
//! bug is resolved. Do not delete these tests — fix the bugs instead.
//!
//! ## Must Fix findings
//!
//! ### Finding 1: Phase deadlock when synthesis=false
//!
//! After `on_worker_completed(passed=true)` with `synthesis=false`, the
//! orchestrator sets state to `AwaitingWorker` for the next phase. On the
//! following `tick()`, `AwaitingWorker` with no handle returns `Idle`.
//! The reconciler never learns it must spawn the next agent — the pipeline
//! is permanently stuck.
//!
//! Fix: `on_worker_completed` must produce a state that causes the next
//! `tick()` to emit `NeedsSpawn`, not `Idle`.
//!
//! ### Finding 2: Synthesizing bypass emits Idle instead of NeedsSpawn
//!
//! When `synthesis=true` but the synthesis step is not yet wired, the
//! `Synthesizing` match arm in `tick()` transitions to `AwaitingWorker` and
//! calls `self.tick()` recursively. That recursive call returns `Idle`
//! (no handle), so the outer `tick()` also returns `Idle`. The reconciler
//! never spawns the next agent.
//!
//! Fix: the `Synthesizing` bypass must emit `NeedsSpawn` directly (not via
//! a recursive `tick()` call on a state that can't produce `NeedsSpawn`).
//!
//! ### Finding 3: Session decision off-by-one in on_worker_completed
//!
//! `session_decision()` reads `self.current_phase` to derive the "current"
//! agent. But `on_worker_completed` increments `self.current_phase` to the
//! *next* phase **before** calling `session_decision`. The result: every
//! non-retry, non-verification advancement sees `current_agent == next_agent`
//! and matches the "same-agent retry → Continue" arm — even when the two
//! agents are different and a fresh session is warranted.
//!
//! Fix: capture the previous agent name before incrementing `current_phase`,
//! then pass it into the session decision logic.

use std::path::PathBuf;

use crate::orchestrate::{FeatureOrchestrator, OrchestratorBehavior, SessionDecision, TickOutcome};
use crate::store::WorkRef;

fn test_bead() -> WorkRef {
    WorkRef {
        repo: "test-repo".into(),
        scope: String::new(),
        bead_id: "test-bead".into(),
    }
}

fn make_orchestrator(pipeline: Vec<String>, synthesis: bool) -> FeatureOrchestrator {
    let config = OrchestratorBehavior {
        synthesis,
        fan_out: false,
        plan_gate: false,
        max_research_workers: 2,
        fork_context: false,
    };
    FeatureOrchestrator::new(
        test_bead(),
        "feature".into(),
        pipeline,
        PathBuf::from("/tmp/test-orch"),
        config,
    )
}

// ── Finding 1 ──────────────────────────────────────────────────────────────

/// After a successful phase with synthesis disabled, the NEXT tick MUST emit
/// NeedsSpawn for the following pipeline agent.
///
/// Currently FAILS: `on_worker_completed` sets state to `AwaitingWorker`
/// (no handle), and `tick()` on that state returns `Idle`.
#[test]
fn phase_advance_no_synthesis_emits_needs_spawn() {
    let mut orch = make_orchestrator(vec!["dev-agent".into(), "staging-agent".into()], false);

    // First tick — request phase 0.
    let first = orch.tick();
    assert!(
        matches!(first, TickOutcome::NeedsSpawn { phase: 0, .. }),
        "expected NeedsSpawn phase=0, got {first:?}"
    );

    // Worker completes successfully — advance to phase 1.
    orch.on_worker_completed(true, 5);

    // INVARIANT: next tick MUST request phase 1 (staging-agent).
    // Bug: returns Idle because AwaitingWorker with no handle → Idle.
    let second = orch.tick();
    assert!(
        matches!(second, TickOutcome::NeedsSpawn { phase: 1, .. }),
        "phase 1 was never requested — pipeline deadlocked; got {second:?}"
    );

    // And the requested agent must be the next pipeline agent.
    if let TickOutcome::NeedsSpawn { ref agent, .. } = second {
        assert_eq!(agent, "staging-agent", "wrong agent requested for phase 1");
    }
}

/// A 3-phase pipeline must advance through all phases.
///
/// Currently FAILS for the same reason as the 2-phase test: each
/// `on_worker_completed` call leaves the orchestrator in `AwaitingWorker`
/// state with no handle, so subsequent ticks return `Idle`.
#[test]
fn three_phase_pipeline_advances_all_phases_no_synthesis() {
    let mut orch = make_orchestrator(
        vec![
            "scoping-agent".into(),
            "dev-agent".into(),
            "staging-agent".into(),
        ],
        false,
    );

    // Phase 0.
    let t0 = orch.tick();
    assert!(
        matches!(t0, TickOutcome::NeedsSpawn { phase: 0, .. }),
        "expected phase 0, got {t0:?}"
    );

    orch.on_worker_completed(true, 5);

    // Phase 1 — currently deadlocks here.
    let t1 = orch.tick();
    assert!(
        matches!(t1, TickOutcome::NeedsSpawn { phase: 1, .. }),
        "expected phase 1, got {t1:?}"
    );

    orch.on_worker_completed(true, 5);

    // Phase 2.
    let t2 = orch.tick();
    assert!(
        matches!(t2, TickOutcome::NeedsSpawn { phase: 2, .. }),
        "expected phase 2, got {t2:?}"
    );

    orch.on_worker_completed(true, 5);

    // All phases done → Terminal.
    let t3 = orch.tick();
    assert!(
        matches!(t3, TickOutcome::Terminal),
        "expected Terminal after 3-phase pipeline, got {t3:?}"
    );
}

// ── Finding 2 ──────────────────────────────────────────────────────────────

/// When synthesis is enabled but not yet wired, `tick()` in `Synthesizing`
/// state must emit `NeedsSpawn` for the next pipeline phase.
///
/// Currently FAILS: the `Synthesizing` arm transitions to `AwaitingWorker`
/// then calls `self.tick()` recursively, which returns `Idle`.
#[test]
fn synthesizing_bypass_emits_needs_spawn() {
    let mut orch = make_orchestrator(vec!["dev-agent".into(), "staging-agent".into()], true);

    // Phase 0.
    let t0 = orch.tick();
    assert!(
        matches!(t0, TickOutcome::NeedsSpawn { phase: 0, .. }),
        "expected NeedsSpawn phase=0, got {t0:?}"
    );

    // Worker completes → enters Synthesizing (synthesis=true).
    orch.on_worker_completed(true, 5);

    // INVARIANT: tick() while in Synthesizing (not-yet-wired path) MUST emit
    // NeedsSpawn, not Idle. The bypass skips synthesis but must still hand off
    // work to the next pipeline agent.
    // Bug: returns Idle because Synthesizing sets AwaitingWorker then calls
    // tick() recursively → no handle → Idle.
    let t1 = orch.tick();
    assert!(
        matches!(t1, TickOutcome::NeedsSpawn { .. }),
        "Synthesizing bypass must emit NeedsSpawn; got {t1:?}"
    );
}

// ── Finding 3 ──────────────────────────────────────────────────────────────

/// `session_decision` must return SpawnFresh when dev-agent advances to
/// prod-agent (non-retry, non-verification advancement).
///
/// Currently FAILS: `on_worker_completed` increments `current_phase` before
/// calling `session_decision`, so `current_agent` evaluates to `next_agent`.
/// The `(current, next) if current == next` arm then matches and returns
/// `Continue` — even though this is not a retry.
///
/// NOTE: this test only surfaces the bug when Finding 1 is also fixed (i.e.,
/// the NeedsSpawn is actually emitted). Mark it accordingly.
#[test]
fn session_decision_fresh_for_non_retry_advancement() {
    // Pipeline: dev-agent → prod-agent (neither is a verification agent by
    // the current allow-list, but they are different agents — not a retry).
    let mut orch = make_orchestrator(vec!["dev-agent".into(), "prod-agent".into()], false);

    // Provide a session ID so the decision isn't forced to SpawnFresh by the
    // "no previous session" guard.
    orch.last_session_id = Some("session-abc123".into());

    // First tick — phase 0.
    orch.tick();

    // Advance: dev-agent → prod-agent (different agents, not a retry).
    orch.on_worker_completed(true, 5);

    // INVARIANT: dev-agent advancing to prod-agent is NOT a retry. The session
    // decision for distinct agents should be SpawnFresh unless explicitly
    // whitelisted (e.g. scoping → dev).
    //
    // Bug: session_decision is called AFTER current_phase is incremented, so
    // current_agent == next_agent == "prod-agent" → "same agent retry" arm
    // fires → returns Continue instead of SpawnFresh.
    let outcome = orch.tick();
    if let TickOutcome::NeedsSpawn {
        session_decision, ..
    } = outcome
    {
        assert!(
            matches!(session_decision, SessionDecision::SpawnFresh),
            "dev-agent → prod-agent must be SpawnFresh, not Continue"
        );
    } else {
        // Finding 1 not yet fixed — document that both bugs must be resolved.
        panic!(
            "expected NeedsSpawn (Finding 1 must also be fixed for this assertion to run); got {outcome:?}"
        );
    }
}

/// `session_decision` must return Continue when the SAME agent is retried
/// (failure path in `on_worker_completed`).
///
/// Ensures the retry-Continue logic still works once Finding 3 is fixed.
/// This test should pass both before and after the fix.
#[test]
fn session_decision_continue_for_retry() {
    let mut orch = make_orchestrator(vec!["dev-agent".into(), "staging-agent".into()], false);

    orch.last_session_id = Some("session-xyz".into());

    // First tick.
    orch.tick();

    // Worker FAILS → retry same phase.
    orch.on_worker_completed(false, 5);

    // The direct decision call is the observable part: retrying dev-agent
    // on the same phase should yield Continue.
    let decision = orch.session_decision("dev-agent");
    assert!(
        matches!(decision, SessionDecision::Continue),
        "retry of same agent must be Continue; got {decision:?}"
    );
}

// ── Baseline: correct behavior that must not regress ───────────────────────

/// Single-phase pipeline reaches Terminal after one successful worker.
/// This verifies the terminal path works end-to-end.
#[test]
fn single_phase_pipeline_reaches_terminal() {
    let mut orch = make_orchestrator(vec!["dev-agent".into()], false);

    let t0 = orch.tick();
    assert!(
        matches!(t0, TickOutcome::NeedsSpawn { phase: 0, .. }),
        "expected NeedsSpawn phase=0, got {t0:?}"
    );

    orch.on_worker_completed(true, 5);

    let t1 = orch.tick();
    assert!(
        matches!(t1, TickOutcome::Terminal),
        "single-phase pipeline must reach Terminal; got {t1:?}"
    );
}

/// Empty pipeline terminates immediately on first tick.
#[test]
fn empty_pipeline_terminates_immediately() {
    let mut orch = make_orchestrator(vec![], false);
    let t = orch.tick();
    assert!(
        matches!(t, TickOutcome::Terminal),
        "empty pipeline must be Terminal immediately; got {t:?}"
    );
}

/// Max retries exceeded → orchestrator enters Failed state.
#[test]
fn max_retries_exceeded_enters_failed_state() {
    let mut orch = make_orchestrator(vec!["dev-agent".into()], false);

    orch.tick(); // phase 0 spawn

    // Fail max_retries times.
    let max = 3u32;
    for i in 0..max {
        orch.on_worker_completed(false, max);
        let outcome = orch.tick();
        if i < max - 1 {
            // Still retrying — AwaitingWorker (or NeedsSpawn once Finding 1 is fixed).
            assert!(
                !matches!(outcome, TickOutcome::Failed { .. }),
                "should not fail before max retries; retry {i}"
            );
        }
    }

    // Final tick after hitting the limit.
    let final_outcome = orch.tick();
    assert!(
        matches!(final_outcome, TickOutcome::Failed { .. }),
        "should be Failed after max retries; got {final_outcome:?}"
    );
}
