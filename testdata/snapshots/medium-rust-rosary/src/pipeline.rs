//! Schema-driven pipeline engine.
//!
//! Replaces the hardcoded `agent_pipeline()` match in dispatch.rs with
//! config-driven pipeline definitions. Provides a unified completion handler
//! used by both `iterate()` Phase 5 and `wait_and_verify()`.
//!
//! Pipeline state is persisted to Dolt via `DispatchStore` (survives crashes).

use std::collections::HashMap;

use crate::store::{DispatchRecord, DispatchStore, PipelineState, WorkRef};

/// What the reconciler should do after a bead's agent completes.
#[derive(Debug, Clone, PartialEq)]
pub enum CompletionAction {
    /// Advance to next agent in pipeline.
    Advance { next_agent: String, phase: u8 },
    /// Pipeline complete — checkpoint, merge/PR, close bead.
    Terminal,
    /// Verification failed — retry same phase.
    Retry,
    /// Max retries exceeded — block bead.
    Deadletter,
}

/// Config-driven pipeline engine with optional persistent state.
pub struct PipelineEngine {
    /// issue_type → ordered agent sequence (from config).
    definitions: HashMap<String, Vec<String>>,
    /// Dolt backend for persistent pipeline state. Optional — degrades to
    /// in-memory-only when unavailable.
    store: Option<Box<dyn DispatchStore>>,
    /// Maximum pipeline stages to execute. 0 = unlimited.
    max_depth: usize,
}

static DEFAULT_AGENT: &str = "dev-agent";

impl PipelineEngine {
    /// Create a new engine from config-driven pipeline definitions.
    /// `max_depth` of 0 means unlimited.
    pub fn new(
        definitions: HashMap<String, Vec<String>>,
        store: Option<Box<dyn DispatchStore>>,
        max_depth: usize,
    ) -> Self {
        Self {
            definitions,
            store,
            max_depth,
        }
    }

    /// Look up the agent sequence for an issue type.
    /// Falls back to `["dev-agent"]` for unknown types.
    /// Truncates to `max_depth` stages when set (0 = unlimited).
    pub fn agents_for(&self, issue_type: &str) -> Vec<String> {
        let mut agents = self
            .definitions
            .get(issue_type)
            .cloned()
            .unwrap_or_else(|| vec![DEFAULT_AGENT.to_string()]);
        if self.max_depth > 0 {
            agents.truncate(self.max_depth);
        }
        agents
    }

    /// First agent in the pipeline for an issue type.
    pub fn default_agent(&self, issue_type: &str) -> String {
        self.agents_for(issue_type)
            .first()
            .cloned()
            .unwrap_or_else(|| DEFAULT_AGENT.to_string())
    }

    /// Next agent in the pipeline after `current`, or None if at end.
    pub fn next_agent(&self, issue_type: &str, current: &str) -> Option<String> {
        let agents = self.agents_for(issue_type);
        let idx = agents.iter().position(|a| a == current)?;
        agents.get(idx + 1).cloned()
    }

    /// Determine the completion action after an agent finishes.
    ///
    /// Arguments:
    /// - `issue_type`: the bead's issue type (drives pipeline lookup)
    /// - `current_agent`: which agent just finished
    /// - `exit_success`: did the agent process exit 0?
    /// - `verify_passed`: Some(true) = passed, Some(false) = failed, None = no verifier
    /// - `retries`: how many times this phase has been retried
    /// - `max_retries`: threshold for deadlettering
    pub fn decide(
        &self,
        issue_type: &str,
        current_agent: Option<&str>,
        exit_success: bool,
        verify_passed: Option<bool>,
        retries: u32,
        max_retries: u32,
    ) -> CompletionAction {
        // Non-zero exit → retry or deadletter
        if !exit_success {
            return if retries >= max_retries {
                CompletionAction::Deadletter
            } else {
                CompletionAction::Retry
            };
        }

        // Verification failed → retry or deadletter
        if verify_passed == Some(false) {
            return if retries >= max_retries {
                CompletionAction::Deadletter
            } else {
                CompletionAction::Retry
            };
        }

        // Passed (either explicitly or no verifier). Check next agent.
        if let Some(agent) = current_agent
            && let Some(next) = self.next_agent(issue_type, agent)
        {
            let agents = self.agents_for(issue_type);
            let phase = agents.iter().position(|a| a == &next).unwrap_or(0) as u8;
            return CompletionAction::Advance {
                next_agent: next,
                phase,
            };
        }

        CompletionAction::Terminal
    }

    // ── DispatchStore delegation (best-effort) ──────────────────────

    /// Persist pipeline state to backend. No-op if store unavailable.
    pub async fn upsert_state(&self, state: &PipelineState) {
        if let Some(ref store) = self.store
            && let Err(e) = store.upsert_pipeline(state).await
        {
            eprintln!(
                "[pipeline] failed to persist state for {}/{}: {e}",
                state.bead_ref.repo, state.bead_ref.bead_id
            );
        }
    }

    /// Read pipeline state from backend. Returns None if store unavailable.
    #[allow(dead_code)] // API surface — per-bead lookup for future use
    pub async fn get_state(&self, bead_ref: &WorkRef) -> Option<PipelineState> {
        if let Some(ref store) = self.store {
            match store.get_pipeline(bead_ref).await {
                Ok(state) => state,
                Err(e) => {
                    eprintln!(
                        "[pipeline] failed to read state for {}/{}: {e}",
                        bead_ref.repo, bead_ref.bead_id
                    );
                    None
                }
            }
        } else {
            None
        }
    }

    /// Clear pipeline state (bead done or deadlettered). No-op if store unavailable.
    pub async fn clear_state(&self, bead_ref: &WorkRef) {
        if let Some(ref store) = self.store
            && let Err(e) = store.clear_pipeline(bead_ref).await
        {
            eprintln!(
                "[pipeline] failed to clear state for {}/{}: {e}",
                bead_ref.repo, bead_ref.bead_id
            );
        }
    }

    /// Record a new dispatch to backend store. Best-effort — no-op if store unavailable.
    pub async fn record_dispatch(&self, record: &DispatchRecord) {
        if let Some(ref store) = self.store
            && let Err(e) = store.record_dispatch(record).await
        {
            eprintln!("[pipeline] failed to record dispatch {}: {e}", record.id);
        }
    }

    /// Mark a dispatch as complete in backend store. Best-effort — no-op if store unavailable.
    pub async fn complete_dispatch(&self, id: &str, outcome: &str) {
        if let Some(ref store) = self.store
            && let Err(e) = store.complete_dispatch(id, outcome).await
        {
            eprintln!("[pipeline] failed to complete dispatch {id}: {e}");
        }
    }

    /// List all dispatch records with no completion timestamp.
    /// Used by crash recovery to abandon orphaned dispatches from a previous run.
    pub async fn active_dispatches(&self) -> Vec<crate::store::DispatchRecord> {
        if let Some(ref store) = self.store {
            store.active_dispatches().await.unwrap_or_default()
        } else {
            Vec::new()
        }
    }

    /// List all active pipeline states. Returns empty vec if store unavailable.
    pub async fn list_active(&self) -> Vec<PipelineState> {
        if let Some(ref store) = self.store {
            store.list_active_pipelines().await.unwrap_or_default()
        } else {
            Vec::new()
        }
    }

    /// Build a PipelineState for a freshly dispatched bead.
    pub fn initial_state(&self, bead_ref: WorkRef, issue_type: &str) -> PipelineState {
        let agent = self.default_agent(issue_type);
        PipelineState {
            bead_ref,
            pipeline_phase: 0,
            pipeline_agent: agent,
            phase_status: "executing".to_string(),
            retries: 0,
            consecutive_reverts: 0,
            highest_verify_tier: None,
            last_generation: 0,
            backoff_until: None,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::default_pipelines;

    fn engine() -> PipelineEngine {
        PipelineEngine::new(default_pipelines(), None, 0)
    }

    #[test]
    fn agents_for_known_types() {
        let e = engine();
        assert_eq!(
            e.agents_for("bug"),
            vec!["scoping-agent", "dev-agent", "staging-agent"]
        );
        assert_eq!(
            e.agents_for("feature"),
            vec!["scoping-agent", "dev-agent", "staging-agent", "prod-agent"]
        );
        assert_eq!(e.agents_for("task"), vec!["dev-agent"]);
        assert_eq!(e.agents_for("review"), vec!["staging-agent"]);
        assert_eq!(e.agents_for("epic"), vec!["pm-agent"]);
    }

    #[test]
    fn agents_for_unknown_falls_back() {
        let e = engine();
        assert_eq!(e.agents_for("unknown_type"), vec!["dev-agent"]);
    }

    #[test]
    fn default_agent_returns_first() {
        let e = engine();
        assert_eq!(e.default_agent("bug"), "scoping-agent");
        assert_eq!(e.default_agent("review"), "staging-agent");
        assert_eq!(e.default_agent("epic"), "pm-agent");
    }

    #[test]
    fn next_agent_advances() {
        let e = engine();
        assert_eq!(
            e.next_agent("bug", "scoping-agent"),
            Some("dev-agent".to_string())
        );
        assert_eq!(
            e.next_agent("bug", "dev-agent"),
            Some("staging-agent".to_string())
        );
        assert_eq!(e.next_agent("bug", "staging-agent"), None);
    }

    #[test]
    fn next_agent_feature_four_stages() {
        let e = engine();
        assert_eq!(
            e.next_agent("feature", "scoping-agent"),
            Some("dev-agent".to_string())
        );
        assert_eq!(
            e.next_agent("feature", "dev-agent"),
            Some("staging-agent".to_string())
        );
        assert_eq!(
            e.next_agent("feature", "staging-agent"),
            Some("prod-agent".to_string())
        );
        assert_eq!(e.next_agent("feature", "prod-agent"), None);
    }

    #[test]
    fn next_agent_single_stage_returns_none() {
        let e = engine();
        assert_eq!(e.next_agent("task", "dev-agent"), None);
    }

    #[test]
    fn next_agent_unknown_current_returns_none() {
        let e = engine();
        assert_eq!(e.next_agent("bug", "unknown-agent"), None);
    }

    #[test]
    fn decide_exit_failure_retries() {
        let e = engine();
        assert_eq!(
            e.decide("bug", Some("dev-agent"), false, None, 0, 3),
            CompletionAction::Retry
        );
    }

    #[test]
    fn decide_exit_failure_deadletters() {
        let e = engine();
        assert_eq!(
            e.decide("bug", Some("dev-agent"), false, None, 3, 3),
            CompletionAction::Deadletter
        );
    }

    #[test]
    fn decide_verify_failed_retries() {
        let e = engine();
        assert_eq!(
            e.decide("bug", Some("dev-agent"), true, Some(false), 1, 3),
            CompletionAction::Retry
        );
    }

    #[test]
    fn decide_verify_failed_deadletters() {
        let e = engine();
        assert_eq!(
            e.decide("bug", Some("dev-agent"), true, Some(false), 3, 3),
            CompletionAction::Deadletter
        );
    }

    #[test]
    fn decide_pass_advances_bug() {
        let e = engine();
        assert_eq!(
            e.decide("bug", Some("dev-agent"), true, Some(true), 0, 3),
            CompletionAction::Advance {
                next_agent: "staging-agent".to_string(),
                phase: 2,
            }
        );
    }

    #[test]
    fn decide_pass_terminal_at_end() {
        let e = engine();
        assert_eq!(
            e.decide("bug", Some("staging-agent"), true, Some(true), 0, 3),
            CompletionAction::Terminal
        );
    }

    #[test]
    fn decide_no_verifier_advances() {
        let e = engine();
        assert_eq!(
            e.decide("bug", Some("dev-agent"), true, None, 0, 3),
            CompletionAction::Advance {
                next_agent: "staging-agent".to_string(),
                phase: 2,
            }
        );
    }

    #[test]
    fn decide_task_single_stage_terminal() {
        let e = engine();
        assert_eq!(
            e.decide("task", Some("dev-agent"), true, Some(true), 0, 3),
            CompletionAction::Terminal
        );
    }

    #[test]
    fn decide_no_current_agent_terminal() {
        let e = engine();
        assert_eq!(
            e.decide("bug", None, true, Some(true), 0, 3),
            CompletionAction::Terminal
        );
    }

    #[test]
    fn custom_pipeline_overrides() {
        let mut defs = HashMap::new();
        defs.insert(
            "bug".into(),
            vec![
                "dev-agent".into(),
                "staging-agent".into(),
                "prod-agent".into(),
            ],
        );
        let e = PipelineEngine::new(defs, None, 0);
        // Custom bug pipeline has 3 stages
        assert_eq!(
            e.next_agent("bug", "staging-agent"),
            Some("prod-agent".to_string())
        );
        // Unknown type still falls back
        assert_eq!(e.agents_for("task"), vec!["dev-agent"]);
    }

    #[test]
    fn initial_state_uses_config() {
        let e = engine();
        let bead_ref = WorkRef {
            repo: "test-repo".into(),
            bead_id: "test-001".into(),
            scope: String::new(),
        };
        let state = e.initial_state(bead_ref.clone(), "bug");
        assert_eq!(state.pipeline_agent, "scoping-agent");
        assert_eq!(state.pipeline_phase, 0);
        assert_eq!(state.phase_status, "executing");
    }

    // -- max_depth gating tests --

    #[test]
    fn depth_zero_is_unlimited() {
        let e = PipelineEngine::new(default_pipelines(), None, 0);
        assert_eq!(
            e.agents_for("bug"),
            vec!["scoping-agent", "dev-agent", "staging-agent"]
        );
        assert_eq!(
            e.agents_for("feature"),
            vec!["scoping-agent", "dev-agent", "staging-agent", "prod-agent"]
        );
    }

    #[test]
    fn depth_one_truncates_to_single_agent() {
        let e = PipelineEngine::new(default_pipelines(), None, 1);
        assert_eq!(e.agents_for("bug"), vec!["scoping-agent"]);
        assert_eq!(e.agents_for("feature"), vec!["scoping-agent"]);
        assert_eq!(e.agents_for("task"), vec!["dev-agent"]);
    }

    #[test]
    fn depth_two_allows_two_stages() {
        let e = PipelineEngine::new(default_pipelines(), None, 2);
        assert_eq!(e.agents_for("bug"), vec!["scoping-agent", "dev-agent"]);
        assert_eq!(e.agents_for("feature"), vec!["scoping-agent", "dev-agent"]);
        assert_eq!(e.agents_for("task"), vec!["dev-agent"]);
    }

    #[test]
    fn depth_gate_affects_next_agent() {
        let e = PipelineEngine::new(default_pipelines(), None, 1);
        // With depth=1, bug pipeline is just ["scoping-agent"], so no next
        assert_eq!(e.next_agent("bug", "scoping-agent"), None);
    }

    #[test]
    fn depth_gate_affects_decide() {
        let e = PipelineEngine::new(default_pipelines(), None, 1);
        // Bug with depth=1: scoping-agent passes → Terminal (only agent in truncated pipeline)
        assert_eq!(
            e.decide("bug", Some("scoping-agent"), true, Some(true), 0, 3),
            CompletionAction::Terminal
        );
    }
}
