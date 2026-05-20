//! Phase 5: Verification, pipeline decisions, and wait_and_verify sub-loop.
//!
//! Sentry span: `reconcile.verify`
//! Attributes: bead.id, action (advance/terminal/retry/deadletter), verify.tier_reached

use std::collections::HashMap;
use std::time::Duration;

use anyhow::Result;

use crate::dispatch;
use crate::dolt::observations::Verdict;
use crate::pipeline::CompletionAction;
use crate::store::WorkRef;
use crate::verify::VerifySummary;

use super::Reconciler;

/// Result of Phase 5 verification pass.
pub(super) struct VerifyResult {
    pub status_updates: Vec<(String, String, String)>,
    pub phase_advances: Vec<(String, String, String)>,
    pub passed: usize,
    pub failed: usize,
    pub deadlettered: usize,
    /// Beads already closed by the agent via MCP — verification skipped.
    pub agent_closed: usize,
}

/// Outcome of executing a single CompletionAction.
enum ActionOutcome {
    /// Pipeline advanced to next agent — caller decides whether to dispatch inline or defer.
    Advanced { next_agent: String },
    /// Pipeline complete — PR created, awaiting merge.
    Completed,
    /// Verification or exit failed — bead reopened for retry.
    Retrying,
    /// Max retries exceeded — bead blocked.
    Deadlettered,
}

impl Reconciler {
    // ── Shared decision logic ───────────────────────────────────────────

    /// Verify an agent's work and compute the pipeline decision.
    /// Returns (action, verify_summary) for the caller to execute.
    pub(super) fn verify_and_decide(
        &mut self,
        bead_id: &str,
        exit_success: bool,
        beads: &[crate::bead::Bead],
    ) -> (CompletionAction, Option<VerifySummary>) {
        let (_, issue_type, current_agent) = self
            .trackers
            .get(bead_id)
            .map(|t| {
                (
                    t.repo.clone(),
                    t.issue_type.clone(),
                    t.current_agent.clone(),
                )
            })
            .unwrap_or_else(|| {
                beads
                    .iter()
                    .find(|b| b.id == bead_id)
                    .map(|b| (b.repo.clone(), b.issue_type.clone(), b.owner.clone()))
                    .unwrap_or_default()
            });

        let retries = self.trackers.get(bead_id).map(|t| t.retries).unwrap_or(0);

        let (verify_passed, verify_summary) = if exit_success {
            let vs = self.verify_agent(bead_id);
            match &vs {
                Some(v) if v.passed() => (Some(true), vs),
                Some(_) => (Some(false), vs),
                None => (None, None),
            }
        } else {
            (Some(false), None)
        };

        let action = self.pipeline.decide(
            &issue_type,
            current_agent.as_deref(),
            exit_success,
            verify_passed,
            retries,
            self.config.max_retries,
        );

        (action, verify_summary)
    }

    /// Execute a CompletionAction — the shared match logic used by both
    /// verify_completed (iterate mode) and wait_and_verify (targeted mode).
    ///
    /// Handles: on_pass/on_fail, workspace checkpoint/cleanup, pipeline state,
    /// handoff writing, tracker updates.
    async fn execute_action(
        &mut self,
        bead_id: &str,
        repo: &str,
        action: &CompletionAction,
        exit_success: bool,
        verify_summary: Option<&VerifySummary>,
        thread_map: &HashMap<String, String>,
    ) -> ActionOutcome {
        // Resolve agent name and phase for the observation.
        let (agent, phase) = self
            .trackers
            .get(bead_id)
            .map(|t| {
                (
                    t.current_agent
                        .clone()
                        .unwrap_or_else(|| "unknown".to_string()),
                    t.phase_index,
                )
            })
            .unwrap_or_else(|| ("unknown".to_string(), 0));

        match action {
            CompletionAction::Advance { next_agent, .. } => {
                self.on_pass(bead_id);
                self.checkpoint_workspace(bead_id).await;
                self.append_observation(
                    bead_id,
                    repo,
                    &agent,
                    phase,
                    Verdict::Pass,
                    "phase passed",
                )
                .await;
                self.write_handoff_and_advance(bead_id, repo, next_agent, thread_map)
                    .await;
                ActionOutcome::Advanced {
                    next_agent: next_agent.clone(),
                }
            }
            CompletionAction::Terminal => {
                self.on_pass(bead_id);
                self.checkpoint_and_cleanup(bead_id).await;
                self.append_observation(
                    bead_id,
                    repo,
                    &agent,
                    phase,
                    Verdict::PrOpen,
                    "pipeline terminal",
                )
                .await;
                self.persist_status(bead_id, repo, "pr_open").await;
                ActionOutcome::Completed
            }
            CompletionAction::Retry => {
                let detail = if exit_success {
                    "verify failed"
                } else {
                    "agent exit non-zero"
                };
                self.handle_failure(bead_id, exit_success, verify_summary);
                self.append_observation(bead_id, repo, &agent, phase, Verdict::Fail, detail)
                    .await;
                self.persist_status(bead_id, repo, "open").await;
                ActionOutcome::Retrying
            }
            CompletionAction::Deadletter => {
                let detail = if exit_success {
                    "verify failed, max retries"
                } else {
                    "agent exit non-zero, max retries"
                };
                self.handle_failure(bead_id, exit_success, verify_summary);
                self.append_observation(bead_id, repo, &agent, phase, Verdict::Deadletter, detail)
                    .await;
                self.cleanup_workspace(bead_id);
                let bead_ref = WorkRef {
                    repo: repo.to_string(),
                    scope: self
                        .trackers
                        .get(bead_id)
                        .map(|t| t.scope.clone())
                        .unwrap_or_default(),
                    bead_id: bead_id.to_string(),
                };
                self.pipeline.clear_state(&bead_ref).await;
                self.persist_status(bead_id, repo, "blocked").await;
                ActionOutcome::Deadlettered
            }
        }
    }

    /// Handle a failure (retry or deadletter) — shared between both verify paths.
    fn handle_failure(
        &mut self,
        bead_id: &str,
        exit_success: bool,
        verify_summary: Option<&VerifySummary>,
    ) {
        if exit_success {
            if let Some(vs) = verify_summary {
                self.on_fail(bead_id, vs);
            }
        } else {
            self.completed_work_dirs.remove(bead_id);
            self.on_fail_exit(bead_id);
        }
    }

    /// Write handoff, update tracker, persist pipeline state, set assignee, reopen.
    /// Shared by verify_completed (via execute_action) and wait_and_verify.
    async fn write_handoff_and_advance(
        &mut self,
        bead_id: &str,
        repo: &str,
        next_agent: &str,
        thread_map: &HashMap<String, String>,
    ) {
        let (from_agent, phase) = self
            .trackers
            .get(bead_id)
            .map(|t| {
                (
                    t.current_agent
                        .clone()
                        .unwrap_or_else(|| "dev-agent".to_string()),
                    t.phase_index,
                )
            })
            .unwrap_or_else(|| ("dev-agent".to_string(), 0));

        if let Some(ws) = self.completed_workspaces.get(bead_id) {
            let work = crate::manifest::Work::from_git(&ws.work_dir, None);
            // Read previous handoff for content-linked chain hash.
            // For phase > 0, missing previous handoff means the chain is broken —
            // skip handoff write entirely to avoid creating an unlinked attestation.
            let previous = if phase > 0 {
                match crate::handoff::Handoff::read_from(&ws.work_dir, phase - 1) {
                    Ok(h) => Some(h),
                    Err(e) => {
                        eprintln!(
                            "[handoff] {bead_id}: phase {phase} cannot read previous handoff, \
                             skipping handoff write to preserve chain integrity: {e}"
                        );
                        return;
                    }
                }
            } else {
                None
            };
            let mut handoff = crate::handoff::Handoff::new(
                phase,
                &from_agent,
                Some(next_agent),
                bead_id,
                self.provider.name(),
                &work,
                previous.as_ref(),
            );
            // Always set thread_id (was missing in wait_and_verify before this refactor)
            handoff.thread_id = thread_map.get(bead_id).cloned();
            // APAS L2: emit DSSE envelope only after a successful handoff write.
            match handoff.write_to(&ws.work_dir) {
                Ok(handoff_path) => {
                    self.write_handoff_envelope(bead_id, &handoff_path, phase, &handoff);
                }
                Err(e) => {
                    eprintln!("[handoff] {bead_id}: failed to write phase handoff: {e}");
                }
            }
        }

        if let Some(tracker) = self.trackers.get_mut(bead_id) {
            tracker.current_agent = Some(next_agent.to_string());
            tracker.phase_index = phase + 1;
        }

        let bead_ref = WorkRef {
            repo: repo.to_string(),
            scope: self
                .trackers
                .get(bead_id)
                .map(|t| t.scope.clone())
                .unwrap_or_default(),
            bead_id: bead_id.to_string(),
        };
        self.pipeline
            .upsert_state(&crate::store::PipelineState {
                bead_ref,
                pipeline_phase: (phase + 1) as u8,
                pipeline_agent: next_agent.to_string(),
                phase_status: "pending".to_string(),
                retries: 0,
                consecutive_reverts: 0,
                highest_verify_tier: None,
                last_generation: 0,
                backoff_until: None,
            })
            .await;

        if let Some(client) = self.dolt_client(repo).await {
            client
                .log_event(
                    bead_id,
                    "phase_complete",
                    &format!("{from_agent} → {next_agent}"),
                )
                .await;
            if let Err(e) = client.set_assignee(bead_id, next_agent).await {
                eprintln!("[phase] failed to advance {bead_id}: {e}");
            } else {
                eprintln!("[phase] {bead_id} → {next_agent} (phase {})", phase + 1);
            }
        }
        self.persist_status(bead_id, repo, "open").await;
    }

    // ── Public entry points ─────────────────────────────────────────────

    /// Phase 5: verify completed agents and decide pipeline action.
    /// Returns status updates and phase advances for the caller to persist.
    pub(super) async fn verify_completed(
        &mut self,
        completed: &[(String, bool)],
        beads: &[crate::bead::Bead],
        thread_map: &HashMap<String, String>,
    ) -> VerifyResult {
        let mut result = VerifyResult {
            status_updates: Vec::new(),
            phase_advances: Vec::new(),
            passed: 0,
            failed: 0,
            deadlettered: 0,
            agent_closed: 0,
        };

        for (bead_id, exit_success) in completed {
            // Skip orchestrator-managed beads — their completions are handled
            // by orchestrator_tick() which calls on_worker_completed().
            if self.orchestrators.contains_key(bead_id.as_str()) {
                // Feed the completion back to the orchestrator.
                let (action, _verify_summary) =
                    self.verify_and_decide(bead_id, *exit_success, beads);
                let passed = matches!(
                    action,
                    CompletionAction::Advance { .. } | CompletionAction::Terminal
                );
                if let Some(orch) = self.orchestrators.get_mut(bead_id.as_str()) {
                    orch.on_worker_completed(passed, self.config.max_retries);
                }
                if passed {
                    result.passed += 1;
                } else {
                    result.failed += 1;
                }
                continue;
            }
            let repo = self
                .trackers
                .get(bead_id.as_str())
                .map(|t| t.repo.clone())
                .unwrap_or_else(|| {
                    beads
                        .iter()
                        .find(|b| b.id == *bead_id)
                        .map(|b| b.repo.clone())
                        .unwrap_or_default()
                });

            // Agent-first fast path: the agent already closed the bead via MCP
            // (rsry_bead_close). Skip the full compile/test/lint pipeline — it's
            // wasted compute and would produce noisy rejected-state-transition logs
            // (BeadState validation blocks Closed → Verifying and Closed → PrOpen).
            // Design note: this path trusts the agent's self-close signal, which is
            // a conscious tradeoff against the "pipeline enforces shape" principle.
            // The dev-tier refinement loop (compile/test/lint) is bypassed for
            // self-closing agents. The feature-tier verification (Phase 5.5) still
            // runs on the assembled thread branch, providing a downstream gate.
            if self.is_bead_agent_closed(bead_id, &repo).await {
                self.trackers.remove(bead_id.as_str());
                result.agent_closed += 1;
                continue;
            }

            // Signal "In Review" in Linear while verification tiers run —
            // but only when the agent exited successfully. A crashed agent
            // (exit_success=false) goes straight to retry/deadletter without
            // running any verification, so "verifying" would be misleading.
            if *exit_success {
                let (agent, phase) = self
                    .trackers
                    .get(bead_id.as_str())
                    .map(|t| {
                        (
                            t.current_agent
                                .clone()
                                .unwrap_or_else(|| "unknown".to_string()),
                            t.phase_index,
                        )
                    })
                    .unwrap_or_else(|| ("unknown".to_string(), 0));
                self.persist_status(bead_id, &repo, "verifying").await;
                self.append_observation(
                    bead_id,
                    &repo,
                    &agent,
                    phase,
                    Verdict::Verifying,
                    "entering verifying state",
                )
                .await;
                result
                    .status_updates
                    .push((bead_id.clone(), repo.clone(), "verifying".into()));
            }

            let (action, verify_summary) = self.verify_and_decide(bead_id, *exit_success, beads);

            let outcome = self
                .execute_action(
                    bead_id,
                    &repo,
                    &action,
                    *exit_success,
                    verify_summary.as_ref(),
                    thread_map,
                )
                .await;

            let outcome_str = match &outcome {
                ActionOutcome::Advanced { .. } => "success",
                ActionOutcome::Completed => "success",
                ActionOutcome::Retrying => "failure",
                ActionOutcome::Deadlettered => "deadletter",
            };
            if let Some(dispatch_id) = self
                .trackers
                .get(bead_id.as_str())
                .and_then(|t| t.dispatch_id.as_deref())
            {
                self.pipeline
                    .complete_dispatch(dispatch_id, outcome_str)
                    .await;
            }

            match outcome {
                ActionOutcome::Advanced { ref next_agent } => {
                    result.passed += 1;
                    result
                        .phase_advances
                        .push((bead_id.clone(), repo, next_agent.clone()));
                }
                ActionOutcome::Completed => {
                    result.passed += 1;
                    result
                        .status_updates
                        .push((bead_id.clone(), repo, "pr_open".into()));
                }
                ActionOutcome::Retrying => {
                    result.failed += 1;
                    result
                        .status_updates
                        .push((bead_id.clone(), repo, "open".into()));
                }
                ActionOutcome::Deadlettered => {
                    result.failed += 1;
                    result.deadlettered += 1;
                    result
                        .status_updates
                        .push((bead_id.clone(), repo, "blocked".into()));
                }
            }
        }

        result
    }

    /// Wait for all active agents to complete, then verify their work.
    /// Supports multi-stage pipelines: on phase advance, re-dispatches the
    /// next agent inline and continues the wait loop.
    pub(super) async fn wait_and_verify(&mut self) -> Result<()> {
        let poll_interval = Duration::from_secs(5);
        let timeout = Duration::from_secs(1800);
        let start = std::time::Instant::now();
        let mut last_heartbeat = std::time::Instant::now();

        while !self.active.is_empty() {
            if start.elapsed() > timeout {
                eprintln!("[timeout] killing {} remaining agent(s)", self.active.len());
                let ids: Vec<String> = self.active.keys().cloned().collect();
                for id in &ids {
                    if let Some(handle) = self.active.get_mut(id) {
                        let _ = handle.kill();
                    }
                }
            }

            let completed = self.check_completed();
            if completed.is_empty() {
                if last_heartbeat.elapsed() >= Duration::from_secs(30) {
                    for (id, handle) in &self.active {
                        let agent = self
                            .trackers
                            .get(id.as_str())
                            .and_then(|t| t.current_agent.as_deref())
                            .unwrap_or("agent");
                        let running_for = (chrono::Utc::now() - handle.started_at).num_seconds();
                        eprintln!("[waiting] {id} ({agent}) running for {running_for}s");
                    }
                    last_heartbeat = std::time::Instant::now();
                }
                tokio::time::sleep(poll_interval).await;
                continue;
            }

            let empty_beads: Vec<crate::bead::Bead> = Vec::new();
            let thread_map: HashMap<String, String> = HashMap::new();

            for (bead_id, exit_success) in &completed {
                let repo = self
                    .trackers
                    .get(bead_id.as_str())
                    .map(|t| t.repo.clone())
                    .unwrap_or_default();
                let issue_type = self
                    .trackers
                    .get(bead_id.as_str())
                    .map(|t| t.issue_type.clone())
                    .unwrap_or_default();

                // Signal "In Review" in Linear while verification runs —
                // only when the agent exited successfully (same guard as verify_completed).
                if *exit_success {
                    let (agent, phase) = self
                        .trackers
                        .get(bead_id.as_str())
                        .map(|t| {
                            (
                                t.current_agent
                                    .clone()
                                    .unwrap_or_else(|| "unknown".to_string()),
                                t.phase_index,
                            )
                        })
                        .unwrap_or_else(|| ("unknown".to_string(), 0));
                    self.persist_status(bead_id, &repo, "verifying").await;
                    self.append_observation(
                        bead_id,
                        &repo,
                        &agent,
                        phase,
                        Verdict::Verifying,
                        "entering verifying state",
                    )
                    .await;
                }

                let (action, verify_summary) =
                    self.verify_and_decide(bead_id, *exit_success, &empty_beads);

                let outcome = self
                    .execute_action(
                        bead_id,
                        &repo,
                        &action,
                        *exit_success,
                        verify_summary.as_ref(),
                        &thread_map,
                    )
                    .await;

                // Record completion for every dispatch attempt in --once mode.
                let outcome_str = match &outcome {
                    ActionOutcome::Advanced { .. } => "success",
                    ActionOutcome::Completed => "success",
                    ActionOutcome::Retrying => "failure",
                    ActionOutcome::Deadlettered => "deadletter",
                };
                if let Some(dispatch_id) = self
                    .trackers
                    .get(bead_id.as_str())
                    .and_then(|t| t.dispatch_id.as_deref())
                {
                    self.pipeline
                        .complete_dispatch(dispatch_id, outcome_str)
                        .await;
                }

                // wait_and_verify dispatches the next agent inline (unlike verify_completed
                // which defers to the next triage pass).
                if let ActionOutcome::Advanced { ref next_agent } = outcome {
                    let path = self
                        .repo_info
                        .get(&repo)
                        .map(|(p, _)| p.clone())
                        .unwrap_or_default();

                    let phase = self
                        .trackers
                        .get(bead_id.as_str())
                        .map(|t| t.phase_index)
                        .unwrap_or(1);

                    let mut dispatch_bead = crate::bead::Bead {
                        id: bead_id.clone(),
                        title: String::new(),
                        description: String::new(),
                        status: "dispatched".into(),
                        priority: 2,
                        issue_type,
                        owner: Some(next_agent.clone()),
                        repo: repo.clone(),
                        created_at: chrono::Utc::now(),
                        updated_at: chrono::Utc::now(),
                        dependency_count: 0,
                        dependent_count: 0,
                        comment_count: 0,
                        branch: None,
                        pr_url: None,
                        jj_change_id: None,
                        external_ref: None,
                        files: Vec::new(),
                        test_files: Vec::new(),
                        created_by: None,
                        scope: String::new(),
                        derived_from: vec![],
                    };
                    if let Some(client) = self.dolt_client(&repo).await
                        && let Ok(Some(full)) = client.get_bead(bead_id, &repo).await
                    {
                        dispatch_bead.title = full.title;
                        dispatch_bead.description = full.description;
                        dispatch_bead.files = full.files;
                        dispatch_bead.test_files = full.test_files;
                    }
                    dispatch_bead.owner = Some(next_agent.clone());

                    match dispatch::spawn(
                        &dispatch_bead,
                        &path,
                        true,
                        0,
                        self.provider.as_ref(),
                        self.agents_dir.as_deref(),
                        None,
                        None, // model: verify re-dispatch uses provider default
                    )
                    .await
                    {
                        Ok(handle) => {
                            eprintln!("[dispatch] {bead_id} phase {phase} → {next_agent}");
                            // Record this inline re-dispatch and update tracker.
                            let new_dispatch_id =
                                format!("{}-{}", bead_id, handle.started_at.timestamp_millis());
                            let dispatch_record = crate::store::DispatchRecord {
                                id: new_dispatch_id.clone(),
                                bead_ref: crate::store::WorkRef {
                                    repo: repo.clone(),
                                    scope: self
                                        .trackers
                                        .get(bead_id.as_str())
                                        .map(|t| t.scope.clone())
                                        .unwrap_or_default(),
                                    bead_id: bead_id.clone(),
                                },
                                agent: next_agent.clone(),
                                provider: self.provider.name().to_string(),
                                started_at: handle.started_at,
                                completed_at: None,
                                outcome: None,
                                work_dir: handle.work_dir.display().to_string(),
                                session_id: None,
                                workspace_path: handle.workspace_path.clone(),
                                chain_hash: handle.chain_hash.clone(),
                            };
                            self.pipeline.record_dispatch(&dispatch_record).await;
                            if let Some(tracker) = self.trackers.get_mut(bead_id.as_str()) {
                                tracker.dispatch_id = Some(new_dispatch_id);
                            }
                            self.persist_status(bead_id, &repo, "dispatched").await;
                            self.append_observation(
                                bead_id,
                                &repo,
                                next_agent,
                                phase,
                                Verdict::Dispatched,
                                "re-dispatched for next phase",
                            )
                            .await;
                            self.active.insert(bead_id.clone(), handle);
                        }
                        Err(e) => {
                            // Phase was already advanced (write_handoff_and_advance committed
                            // tracker + pipeline state) but the next agent couldn't spawn.
                            // This is unrecoverable without human intervention — the bead is
                            // in an inconsistent state. Block it rather than leave it wedged
                            // at Dispatched with no running agent.
                            eprintln!(
                                "[dispatch] {bead_id}: spawn failed for {next_agent} after \
                                 phase advance — deadlettering: {e}"
                            );
                            self.persist_status(bead_id, &repo, "blocked").await;
                            self.append_observation(
                                bead_id,
                                &repo,
                                next_agent,
                                phase,
                                Verdict::Fail,
                                &format!("spawn failed after phase advance: {e}"),
                            )
                            .await;
                            self.trackers.remove(bead_id.as_str());
                        }
                    }
                }
            }
        }

        Ok(())
    }
}
