//! Bead persistence and status mirroring.
//!
//! Sentry span: `reconcile.persist`

use super::Reconciler;
use crate::scanner;
use crate::store::BeadStore;

impl Reconciler {
    /// Get or lazily connect a BeadStore for a repo.
    pub(super) async fn dolt_client(&mut self, repo: &str) -> Option<&dyn BeadStore> {
        if self.dolt_clients.contains_key(repo) {
            return self.dolt_clients.get(repo).map(|b| b.as_ref());
        }

        let (path, _) = self.repo_info.get(repo)?;
        let beads_dir = path.join(".beads");
        match crate::bead_sqlite::connect_bead_store(&beads_dir).await {
            Ok(store) => {
                self.dolt_clients.insert(repo.to_string(), store);
                self.dolt_clients.get(repo).map(|b| b.as_ref())
            }
            Err(e) => {
                eprintln!("[bead] failed to connect for {repo}: {e}");
                None
            }
        }
    }

    /// Update bead status in Dolt and log the transition. Best-effort.
    /// Also mirrors the transition to the external issue tracker (Linear)
    /// if the bead has an external_ref and a tracker is configured.
    pub(super) async fn persist_status(&mut self, bead_id: &str, repo: &str, status: &str) {
        // 1. Write to Dolt (source of truth) and fetch external_ref
        let has_tracker = self.issue_tracker.is_some();
        let mut external_ref: Option<String> = None;
        if let Some(client) = self.dolt_client(repo).await {
            if let Err(e) = client.update_status(bead_id, status).await {
                eprintln!("[dolt] failed to update {bead_id} to {status}: {e}");
            }
            client
                .log_event(bead_id, "state_change", &format!("→ {status}"))
                .await;
            if has_tracker {
                external_ref = client.get_external_ref(bead_id).await.ok().flatten();
            }
        }

        // 2. Mirror to external issue tracker (best-effort, never blocks)
        // Pass bead status — the tracker handles mapping to its native states.
        if let (Some(tracker), Some(ext_ref)) = (&self.issue_tracker, external_ref) {
            if let Err(e) = tracker.update_status(&ext_ref, status).await {
                eprintln!(
                    "[{}] failed to mirror {bead_id} → {ext_ref}: {e}",
                    tracker.name()
                );
            } else {
                eprintln!(
                    "[{}] mirrored {bead_id} → {ext_ref} ({status})",
                    tracker.name()
                );
            }
        }
    }

    /// Check if a bead was already closed by the dispatched agent via MCP.
    ///
    /// This is the "agent-first" fast path: when agents self-close beads,
    /// we skip the full verification pipeline (compile+test+lint+diff-sanity),
    /// which is the main consumption throughput bottleneck.
    pub(super) async fn is_bead_agent_closed(&mut self, bead_id: &str, repo: &str) -> bool {
        if let Some(client) = self.dolt_client(repo).await {
            match client.get_status(bead_id).await {
                Ok(Some(ref status)) if status == "closed" || status == "done" => {
                    eprintln!("[agent-closed] {bead_id} — skipping verification (agent-first)");
                    true
                }
                _ => false,
            }
        } else {
            false
        }
    }

    /// Append an observation for a bead (CRDT-lattice dual-write).
    ///
    /// This runs alongside persist_status — both the mutable cell AND the
    /// append-only observation are written. Once we validate the lattice
    /// produces identical status, persist_status can be removed.
    pub(super) async fn append_observation(
        &mut self,
        bead_id: &str,
        repo: &str,
        agent: &str,
        phase: u32,
        verdict: crate::dolt::observations::Verdict,
        detail: &str,
    ) {
        if let Some(client) = self.dolt_client(repo).await {
            let event_detail =
                format!("phase={phase} agent={agent} verdict={verdict:?} detail={detail}");
            client
                .log_event(bead_id, "observation", &event_detail)
                .await;
        }
    }

    /// Reset beads stuck at 'dispatched' from a previous run.
    /// On startup, any bead with status=dispatched has no running agent
    /// (the reconciler that dispatched it is dead). Reset to open.
    ///
    /// Also restores pipeline state: if a bead had progressed to phase 2
    /// before the crash, the tracker is rebuilt from persistent PipelineState
    /// so it resumes at the correct phase (not phase 0).
    pub(super) async fn recover_stuck_beads(&mut self) {
        // Load persistent pipeline state so we can restore phase progress.
        let active_pipelines = self.pipeline.list_active().await;
        let pipeline_map: std::collections::HashMap<String, crate::store::PipelineState> =
            active_pipelines
                .into_iter()
                .map(|ps| (ps.bead_ref.bead_id.clone(), ps))
                .collect();

        if !pipeline_map.is_empty() {
            eprintln!(
                "[recover] found {} active pipeline states from previous run",
                pipeline_map.len()
            );
        }

        let beads = match scanner::scan_repos(&self.config.repo).await {
            Ok(b) => b,
            Err(_) => return,
        };
        for bead in &beads {
            // Use BeadState::from(&str) so the recovery path catches BOTH
            // "dispatched" (the canonical status rsry_dispatch writes) AND
            // "in_progress" (Linear's vocabulary, written back by the
            // Linear→Dolt sync path — bead.rs:147 maps it to
            // BeadState::Dispatched as a legacy alias). Before this fix,
            // string-equality on "dispatched" missed every bead that had
            // been touched by a Linear sync round-trip after dispatch,
            // leaving them stuck forever (rosary-67c43d).
            if bead.state() == crate::bead::BeadState::Dispatched {
                // Restore tracker from persistent pipeline state if available.
                // This preserves phase progress across crashes — without it,
                // a bead at phase 2 (staging) would restart at phase 0 (scoping).
                if let Some(ps) = pipeline_map.get(&bead.id) {
                    eprintln!(
                        "[recover] restoring {} to phase {} ({})",
                        bead.id, ps.pipeline_phase, ps.pipeline_agent
                    );
                    self.trackers.insert(
                        bead.id.clone(),
                        super::BeadTracker {
                            repo: bead.repo.clone(),
                            scope: bead.scope.clone(),
                            last_generation: 0,
                            retries: ps.retries,
                            consecutive_reverts: ps.consecutive_reverts,
                            highest_tier: ps.highest_verify_tier.map(|t| t as usize),
                            current_agent: Some(ps.pipeline_agent.clone()),
                            phase_index: ps.pipeline_phase as u32,
                            issue_type: bead.issue_type.clone(),
                            dispatch_id: None,
                        },
                    );
                }

                eprintln!("[recover] resetting stuck bead {} to open", bead.id);
                self.persist_status(&bead.id, &bead.repo, "open").await;
            }
        }

        // Abandon orphaned dispatch records — these have completed_at=NULL because
        // the previous reconciler process died between record_dispatch() and
        // complete_dispatch(). Without cleanup they accumulate indefinitely.
        // The pipeline is authoritative: if no process owns a dispatch, it is abandoned.
        let orphaned = self.pipeline.active_dispatches().await;
        if !orphaned.is_empty() {
            eprintln!(
                "[recover] abandoning {} orphaned dispatch record(s) from previous run",
                orphaned.len()
            );
            for record in orphaned {
                eprintln!(
                    "[recover] abandoning dispatch {} (bead={})",
                    record.id, record.bead_ref.bead_id
                );
                let _ = self
                    .pipeline
                    .complete_dispatch(&record.id, "abandoned")
                    .await;
            }
        }
    }

    /// Per-iteration liveness sweep: walks each repo's `Dispatched` beads,
    /// cross-references `sessions`, and transitions to `dead_letter` any
    /// bead whose registered worker pid is gone.
    ///
    /// Returns the IDs of beads moved to dead_letter this iteration. The
    /// caller updates `IterationSummary.deadlettered_ids` (set-membership
    /// for target-bead-mode exit) and `IterationSummary.deadlettered`
    /// (count for observability).
    ///
    /// Wired into `iterate()` Phase 1.8. Unlike `recover_stuck_beads()`
    /// (which runs once at startup and reverts ALL Dispatched beads
    /// unconditionally), this runs every iteration AND is liveness-aware
    /// — live workers are left alone.
    pub(super) async fn liveness_sweep(
        &mut self,
        beads: &[crate::bead::Bead],
        sessions: &[crate::session::SessionEntry],
    ) -> Vec<String> {
        // Empty session list → no dispatched workers we could possibly
        // detect as dead. Short-circuit BEFORE the per-repo list_beads
        // scans, which would otherwise run every iteration in the
        // SessionRegistry::load failure path (where the caller passes
        // an empty slice as a fallback) and do guaranteed-zero work.
        // Round-5 review on PR #202.
        if sessions.is_empty() {
            return Vec::new();
        }

        // Collect unique repo names that have at least one Dispatched bead.
        // Avoids per-bead client lookups for repos with no live work.
        let mut repos_to_sweep: std::collections::HashSet<String> =
            std::collections::HashSet::new();
        for bead in beads {
            if bead.state() == crate::bead::BeadState::Dispatched {
                repos_to_sweep.insert(bead.repo.clone());
            }
        }

        // Build the session index ONCE; reused across all per-repo sweeps
        // so total cost is O(sessions) + O(beads), not O(repos × sessions).
        // Round-9 finding on PR #202.
        let session_index = crate::dispatch::sweep::build_session_index(sessions);

        // Two-phase: first collect candidates across all repos (sweep is
        // read-only on bead status), then perform the state transition via
        // `persist_status` so Linear mirroring + state_change events fire.
        // Routing dead_letter through persist_status was the round-7
        // Copilot finding — direct `update_status` from inside the sweep
        // bypassed the audit/sync path.
        let mut all_candidates: Vec<(String, crate::dispatch::sweep::DeadWorkerCandidate)> =
            Vec::new();
        for repo in repos_to_sweep {
            let Some(client) = self.dolt_client(&repo).await else {
                continue;
            };
            let report =
                crate::dispatch::sweep::sweep_dead_workers(client, &repo, &session_index).await;
            for candidate in report.candidates {
                all_candidates.push((repo.clone(), candidate));
            }
        }

        let mut deadlettered = Vec::new();
        for (repo, candidate) in all_candidates {
            // persist_status borrows &mut self; the outer `client` borrow
            // from above is already released by this point.
            self.persist_status(&candidate.bead_id, &repo, "dead_letter")
                .await;

            // Verify the write actually landed before counting it.
            // `persist_status` is best-effort (eprintln on update_status
            // failure, doesn't propagate). Counting unconditionally would
            // make `deadlettered_ids` lie about the bead's real state and
            // could false-trip target-bead-mode exit. Round-9 finding on
            // PR #202.
            let persisted = match self.dolt_client(&repo).await {
                Some(client) => matches!(
                    client.get_status(&candidate.bead_id).await,
                    Ok(Some(ref s)) if s == "dead_letter"
                ),
                None => false,
            };
            if !persisted {
                eprintln!(
                    "[reconcile] persist_status for {} didn't land — not counting as deadlettered",
                    candidate.bead_id
                );
                continue;
            }

            // Surface the forensic context the sweep collected so operators
            // tailing the reconciler log see WHY each bead got deadlettered
            // (pid, worktree, last_activity) — not just the bead id.
            eprintln!(
                "[reconcile] {} → dead_letter [{}]",
                candidate.bead_id, candidate.detail
            );
            deadlettered.push(candidate.bead_id);
        }
        deadlettered
    }
}
