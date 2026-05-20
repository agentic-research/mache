//! Hierarchical orchestration integration.
//!
//! When `[orchestration] mode = "hierarchical"`, the reconciler delegates
//! per-bead lifecycle management to `FeatureOrchestrator` instances instead
//! of directly spawning agents. This module contains the reconciler methods
//! that drive orchestrators and translate their `TickOutcome`s into the
//! same actions the flat path uses (dispatch, verify, advance, deadletter).

use std::path::PathBuf;

use super::Reconciler;
use crate::dispatch;
use crate::orchestrate::{
    FeatureOrchestrator, OrchestratorBehavior, OrchestratorState, TickOutcome,
};
use crate::store::{DispatchRecord, WorkRef};

impl Reconciler {
    /// Returns true if we're in hierarchical orchestration mode.
    pub(super) fn is_hierarchical(&self) -> bool {
        self.config.orchestration.mode == "hierarchical"
    }

    /// Build OrchestratorBehavior from the global config.
    fn orchestrator_behavior(&self) -> OrchestratorBehavior {
        let oc = &self.config.orchestration;
        OrchestratorBehavior {
            synthesis: oc.synthesis,
            fan_out: oc.fan_out,
            plan_gate: oc.plan_gate,
            max_research_workers: oc.max_research_workers,
            fork_context: oc.fork_context,
        }
    }

    /// Create a FeatureOrchestrator for a bead and register it.
    ///
    /// Called during the dispatch phase when mode=hierarchical, instead of
    /// directly calling `dispatch::spawn()`.
    pub(super) fn create_orchestrator(
        &mut self,
        bead_id: &str,
        repo: &str,
        scope: &str,
        issue_type: &str,
        work_dir: PathBuf,
    ) {
        let bead_ref = WorkRef {
            repo: repo.to_string(),
            scope: scope.to_string(),
            bead_id: bead_id.to_string(),
        };
        let pipeline = self.pipeline.agents_for(issue_type);
        let behavior = self.orchestrator_behavior();

        let orchestrator = FeatureOrchestrator::new(
            bead_ref,
            issue_type.to_string(),
            pipeline,
            work_dir,
            behavior,
        );

        eprintln!(
            "[orchestrator] created for {} (pipeline={:?})",
            bead_id, orchestrator.pipeline
        );

        self.orchestrators.insert(bead_id.to_string(), orchestrator);
    }

    /// Drive all orchestrators forward one step.
    ///
    /// Called each iteration between check_completed and dispatch.
    /// Processes tick outcomes:
    /// - NeedsSpawn → dispatch an agent, give handle to orchestrator
    /// - WorkerCompleted → run verification, call on_worker_completed
    /// - Terminal → mark bead as pr_open, remove orchestrator
    /// - Failed → deadletter bead, remove orchestrator
    pub(super) async fn orchestrator_tick(
        &mut self,
        beads: &[crate::bead::Bead],
    ) -> OrchestratorTickResult {
        let mut result = OrchestratorTickResult::default();

        // Collect bead_ids to avoid borrowing self in the loop.
        let bead_ids: Vec<String> = self.orchestrators.keys().cloned().collect();

        for bead_id in bead_ids {
            let outcome = {
                let orch = match self.orchestrators.get_mut(&bead_id) {
                    Some(o) => o,
                    None => continue,
                };
                orch.tick()
            };

            match outcome {
                TickOutcome::Idle => {}

                TickOutcome::NeedsSpawn {
                    agent,
                    phase,
                    session_decision: _session_decision,
                } => {
                    // Don't exceed concurrency limit (orchestrator workers count
                    // against the same pool as flat-mode agents).
                    if self.active.len() >= self.config.max_concurrent {
                        continue;
                    }

                    let bead = beads.iter().find(|b| b.id == bead_id);
                    let repo_path = self
                        .orchestrators
                        .get(&bead_id)
                        .and_then(|o| self.repo_info.get(&o.bead_ref.repo))
                        .map(|(p, _)| p.clone());

                    if let (Some(bead), Some(path)) = (bead, repo_path) {
                        let mut dispatch_bead = bead.clone();
                        dispatch_bead.owner = Some(agent.clone());

                        // Update assignee in dolt so it shows the current pipeline agent
                        if let Some(client) = self.dolt_client(&bead.repo).await {
                            let _ = client.set_assignee(&bead.id, &agent).await;
                        }

                        match dispatch::spawn(
                            &dispatch_bead,
                            &path,
                            true,
                            0, // generation not used for orchestrator-managed beads
                            self.provider.as_ref(),
                            self.agents_dir.as_deref(),
                            None,
                            None, // model: orchestrator uses provider default
                        )
                        .await
                        {
                            Ok(handle) => {
                                eprintln!(
                                    "[orchestrator] spawned {agent} for {bead_id} (phase {phase})"
                                );

                                // Record dispatch to backend
                                let dispatch_id =
                                    format!("{}-{}", bead_id, handle.started_at.timestamp_millis());
                                let dispatch_record = DispatchRecord {
                                    id: dispatch_id.clone(),
                                    bead_ref: WorkRef {
                                        repo: bead.repo.clone(),
                                        scope: String::new(),
                                        bead_id: bead_id.clone(),
                                    },
                                    agent: agent.clone(),
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

                                // Track as active so concurrency limits work
                                self.active.insert(bead_id.clone(), handle);

                                result.dispatched += 1;
                            }
                            Err(e) => {
                                eprintln!("[orchestrator] spawn failed for {bead_id}/{agent}: {e}");
                                // The orchestrator already moved to AwaitingWorker before
                                // returning NeedsSpawn. Without feedback it returns Idle
                                // forever. Notify it so it can retry or deadletter.
                                if let Some(orch) = self.orchestrators.get_mut(&bead_id) {
                                    orch.on_worker_completed(false, self.config.max_retries);
                                }
                            }
                        }
                    }
                }

                TickOutcome::WorkerCompleted { exit_success } => {
                    // Run verification through the same path as flat mode.
                    let (action, _verify_summary) =
                        self.verify_and_decide(&bead_id, exit_success, beads);

                    let passed = matches!(
                        action,
                        crate::pipeline::CompletionAction::Advance { .. }
                            | crate::pipeline::CompletionAction::Terminal
                    );

                    // Tell the orchestrator about the result.
                    if let Some(orch) = self.orchestrators.get_mut(&bead_id) {
                        orch.on_worker_completed(passed, self.config.max_retries);
                    }

                    if passed {
                        result.passed += 1;
                    } else {
                        result.failed += 1;
                    }
                }

                TickOutcome::Terminal => {
                    eprintln!("[orchestrator] {} pipeline complete", bead_id);
                    let Some(repo) = self
                        .orchestrators
                        .get(&bead_id)
                        .map(|o| o.bead_ref.repo.clone())
                    else {
                        eprintln!(
                            "[orchestrator] BUG: {} terminal but missing from map — \
                             workspace and status update skipped",
                            bead_id
                        );
                        result.completed += 1;
                        continue;
                    };
                    self.checkpoint_and_cleanup(&bead_id).await;
                    self.persist_status(&bead_id, &repo, "pr_open").await;
                    self.orchestrators.remove(&bead_id);
                    result.completed += 1;
                }

                TickOutcome::Failed { reason } => {
                    eprintln!("[orchestrator] {} failed: {reason}", bead_id);
                    let Some(repo) = self
                        .orchestrators
                        .get(&bead_id)
                        .map(|o| o.bead_ref.repo.clone())
                    else {
                        eprintln!(
                            "[orchestrator] BUG: {} failed but missing from map — \
                             status update skipped",
                            bead_id
                        );
                        result.deadlettered += 1;
                        continue;
                    };
                    self.cleanup_workspace(&bead_id);
                    let bead_ref = WorkRef {
                        repo: repo.clone(),
                        scope: String::new(),
                        bead_id: bead_id.clone(),
                    };
                    self.pipeline.clear_state(&bead_ref).await;
                    self.persist_status(&bead_id, &repo, "blocked").await;
                    self.orchestrators.remove(&bead_id);
                    result.deadlettered += 1;
                }

                TickOutcome::ReadyToAdvance { next_agent: _ } => {
                    // This is a future hook for when synthesis produces a
                    // "ready to spawn next" signal. For now, tick() handles
                    // advancement internally.
                }
            }
        }

        result
    }
}

/// Result of an orchestrator tick pass.
#[derive(Debug, Default)]
pub(super) struct OrchestratorTickResult {
    pub dispatched: usize,
    pub completed: usize,
    pub passed: usize,
    pub failed: usize,
    pub deadlettered: usize,
}

impl Reconciler {
    /// Persist all orchestrator records to their workspace directories.
    ///
    /// Each orchestrator writes `.rsry-orchestrator.json` to its work_dir.
    /// On crash recovery, `recover_orchestrators()` reads these back.
    pub(super) fn persist_orchestrator_records(&self) {
        for (bead_id, orch) in &self.orchestrators {
            let record = crate::orchestrate::OrchestratorRecord {
                bead_ref: orch.bead_ref.clone(),
                issue_type: orch.issue_type.clone(),
                state: orch.state.clone(),
                current_phase: orch.current_phase as u32,
                current_agent: orch.current_agent().map(|s| s.to_string()),
                retries: orch.retries,
                last_session_id: orch.last_session_id.clone(),
                created_at: orch.created_at,
                updated_at: chrono::Utc::now(),
            };
            let path = orch.work_dir.join(".rsry-orchestrator.json");
            match serde_json::to_string_pretty(&record) {
                Ok(json) => {
                    if let Err(e) = std::fs::write(&path, json) {
                        eprintln!("[orchestrator] failed to persist record for {bead_id}: {e}");
                    }
                }
                Err(e) => {
                    eprintln!("[orchestrator] failed to serialize record for {bead_id}: {e}");
                }
            }
        }
    }

    /// Recover orchestrators from workspace directories after a crash.
    ///
    /// Scans all repo paths for `.rsry-orchestrator.json` files in workspaces.
    /// Restores FeatureOrchestrator instances so the reconciler can resume
    /// managing them. Called during startup alongside `recover_stuck_beads()`.
    pub(super) async fn recover_orchestrators(&mut self) {
        if !self.is_hierarchical() {
            return;
        }

        for (repo_name, (repo_path, _lang)) in &self.repo_info {
            // Workspaces live at ~/.rsry/worktrees/<repo_name>/<bead_id>/
            // (see workspace::workspace_dir). workspace_dir(path, "") returns the
            // per-repo worktrees root — scan that, not the repo root.
            let ws_root = crate::workspace::workspace_dir(repo_path, "");
            let entries: Vec<PathBuf> = match std::fs::read_dir(&ws_root) {
                Ok(dir) => dir
                    .filter_map(|e| e.ok())
                    .map(|e| e.path().join(".rsry-orchestrator.json"))
                    .filter(|p| p.exists())
                    .collect(),
                Err(_) => continue,
            };

            for record_path in entries {
                let json = match std::fs::read_to_string(&record_path) {
                    Ok(j) => j,
                    Err(e) => {
                        eprintln!(
                            "[recover] failed to read orchestrator record {}: {e}",
                            record_path.display()
                        );
                        continue;
                    }
                };
                let record: crate::orchestrate::OrchestratorRecord =
                    match serde_json::from_str(&json) {
                        Ok(r) => r,
                        Err(e) => {
                            eprintln!(
                                "[recover] failed to parse orchestrator record {}: {e}",
                                record_path.display()
                            );
                            continue;
                        }
                    };

                // Skip terminal/failed orchestrators — they don't need recovery.
                if matches!(
                    record.state,
                    OrchestratorState::Terminal | OrchestratorState::Failed { .. }
                ) {
                    // Clean up the record file.
                    let _ = std::fs::remove_file(&record_path);
                    continue;
                }

                let work_dir = record_path.parent().unwrap().to_path_buf();
                let pipeline = self.pipeline.agents_for(&record.issue_type);
                let behavior = self.orchestrator_behavior();

                let mut orch = FeatureOrchestrator::new(
                    record.bead_ref.clone(),
                    record.issue_type,
                    pipeline,
                    work_dir,
                    behavior,
                );
                orch.state = record.state;
                orch.current_phase = record.current_phase as usize;
                orch.retries = record.retries;
                orch.last_session_id = record.last_session_id;
                orch.created_at = record.created_at;
                // Worker handle is gone (process died) — orchestrator will
                // request a new spawn on next tick().
                orch.worker_handle = None;

                eprintln!(
                    "[recover] restored orchestrator for {} (phase {}, repo={})",
                    record.bead_ref.bead_id, record.current_phase, repo_name,
                );
                self.orchestrators
                    .insert(record.bead_ref.bead_id.clone(), orch);
            }
        }

        if !self.orchestrators.is_empty() {
            eprintln!(
                "[recover] restored {} orchestrator(s) from previous run",
                self.orchestrators.len()
            );
        }
    }
}
