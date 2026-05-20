//! Agent completion polling, verification, and retry/deadletter logic.
//!
//! Sentry breadcrumb: each on_fail/on_fail_exit records retry count + reason.

use std::time::Instant;

use super::{BeadTracker, Reconciler};
use crate::verify::{Verifier, VerifySummary};

impl Reconciler {
    /// Poll active agents for completion. Returns vec of (bead_id, exit_success).
    pub(super) fn check_completed(&mut self) -> Vec<(String, bool)> {
        let mut completed = Vec::new();

        let bead_ids: Vec<String> = self.active.keys().cloned().collect();
        for bead_id in bead_ids {
            let handle = self.active.get_mut(&bead_id).unwrap();
            let mut done = false;
            let mut success = false;

            let mut capture_tools = false;
            match handle.try_wait() {
                Ok(Some(ok)) => {
                    done = true;
                    success = ok;
                    capture_tools = true; // session fully joined — safe to drain
                }
                Ok(None) => {
                    let elapsed = handle.elapsed();
                    // Soft timeout: warn but don't kill — agent may still be productive
                    if elapsed > chrono::Duration::minutes(30)
                        && elapsed < chrono::Duration::minutes(31)
                    {
                        eprintln!("[slow] {bead_id}: agent running for 30+ min");
                    }
                    // Hard timeout: safety valve for truly stuck agents (4 hours)
                    if elapsed > chrono::Duration::hours(4) {
                        eprintln!("[timeout] killing agent for {bead_id} (4h hard limit)");
                        let _ = handle.kill();
                        done = true;
                        // Don't capture tools: session killed, may still be writing
                    }
                }
                Err(e) => {
                    eprintln!("[error] polling agent for {bead_id}: {e}");
                    done = true;
                    // Don't capture tools: session state unknown
                }
            }

            if done {
                let mut handle = self.active.remove(&bead_id).unwrap();
                let repo = self
                    .trackers
                    .get(&bead_id)
                    .map(|t| t.repo.clone())
                    .unwrap_or_default();
                // Only drain tool records on confirmed clean exit (not kill/error paths).
                if capture_tools {
                    let tools = handle.session.take_tools_used();
                    if !tools.is_empty() {
                        self.pending_tools_used.insert(bead_id.clone(), tools);
                    }
                }
                // Stash workspace for checkpoint + teardown
                if let Some(ws) = handle.workspace.take() {
                    self.completed_workspaces.insert(bead_id.clone(), ws);
                }
                self.completed_work_dirs
                    .insert(bead_id.clone(), (handle.work_dir, repo));
                completed.push((bead_id, success));
            }
        }

        completed
    }

    /// Run verification tiers on an agent's work directory.
    /// Returns None (skip verification) for ReadOnly agents — they produce
    /// research/comments, not code changes.
    pub(super) fn verify_agent(&mut self, bead_id: &str) -> Option<VerifySummary> {
        // Skip verification for ReadOnly agents (scoping-agent, staging-agent review)
        // They don't write code — their output is bead comments, not commits.
        let is_readonly = self
            .trackers
            .get(bead_id)
            .and_then(|t| t.current_agent.as_deref())
            .is_some_and(|agent| matches!(agent, "scoping-agent" | "staging-agent"));
        if is_readonly {
            eprintln!("[verify] {bead_id}: skipped (ReadOnly agent)");
            return None;
        }

        let (work_dir, repo) = self.completed_work_dirs.remove(bead_id)?;

        // Look up language for this repo
        let lang = self
            .repo_info
            .get(&repo)
            .map(|(_, l)| l.as_str())
            .unwrap_or("unknown");

        let mut verifier = Verifier::for_language(lang);
        let ctx = crate::plugin::PluginContext::new(bead_id, &repo);
        for tier in self.plugin_registry.verify_tiers(ctx) {
            verifier.push(tier);
        }
        match verifier.run(&work_dir) {
            Ok(summary) => {
                eprintln!(
                    "[verify] {bead_id}: {} (highest_tier={:?})",
                    if summary.passed() { "PASS" } else { "FAIL" },
                    summary.highest_passing_tier,
                );
                Some(summary)
            }
            Err(e) => {
                eprintln!("[verify] {bead_id}: error running verification: {e}");
                None
            }
        }
    }

    pub(super) fn on_pass(&mut self, bead_id: &str) {
        eprintln!("[pass] {bead_id}");
        let repo = self
            .trackers
            .get(bead_id)
            .map(|t| t.repo.clone())
            .or_else(|| {
                self.orchestrators
                    .get(bead_id)
                    .map(|o| o.bead_ref.repo.clone())
            })
            .unwrap_or_default();
        self.queue.clear_backoff(&repo, bead_id);
        if let Some(tracker) = self.trackers.get_mut(bead_id) {
            tracker.consecutive_reverts = 0;
        }
        // Fire pipeline.close hooks — external tools notified of completion.
        let ctx = crate::plugin::PluginContext::new(bead_id, &repo);
        self.plugin_registry.call_close_hooks(&ctx);
        // Cleanup happens after checkpoint (called from iterate)
    }

    /// Handle a verification failure. Returns true if deadlettered.
    pub(super) fn on_fail(&mut self, bead_id: &str, summary: &VerifySummary) -> bool {
        // Resolve repo before or_insert — orchestrator beads may not be in trackers.
        let fallback_repo = self
            .orchestrators
            .get(bead_id)
            .map(|o| o.bead_ref.repo.clone())
            .unwrap_or_default();
        let tracker = self
            .trackers
            .entry(bead_id.to_string())
            .or_insert(BeadTracker {
                repo: fallback_repo,
                last_generation: 0,
                retries: 0,
                consecutive_reverts: 0,
                highest_tier: None,
                current_agent: None,
                phase_index: 0,
                issue_type: "task".into(),
                dispatch_id: None,
                scope: String::new(),
            });

        // Check for revert (regression from previous best)
        if let (Some(prev), Some(curr)) = (tracker.highest_tier, summary.highest_passing_tier) {
            if curr < prev {
                tracker.consecutive_reverts += 1;
            } else {
                tracker.consecutive_reverts = 0;
            }
        }
        tracker.highest_tier = summary.highest_passing_tier;
        tracker.retries += 1;

        // Stopping conditions
        if tracker.retries >= self.config.max_retries {
            eprintln!("[deadletter] {bead_id}: max retries ({})", tracker.retries);
            return true;
        }
        if tracker.consecutive_reverts >= 3 {
            eprintln!(
                "[deadletter] {bead_id}: {} consecutive reverts",
                tracker.consecutive_reverts
            );
            return true;
        }

        // Schedule retry with backoff
        let repo = tracker.repo.clone();
        let retries = tracker.retries;
        self.queue
            .record_backoff(&repo, bead_id, retries, Instant::now());
        if let Some((name, _)) = summary.first_failure() {
            eprintln!(
                "[retry] {bead_id}: failed at tier '{name}', retry #{} scheduled",
                tracker.retries
            );
        }

        false
    }

    /// Handle agent exit failure (non-zero exit). Returns true if deadlettered.
    pub(super) fn on_fail_exit(&mut self, bead_id: &str) -> bool {
        let fallback_repo = self
            .orchestrators
            .get(bead_id)
            .map(|o| o.bead_ref.repo.clone())
            .unwrap_or_default();
        let tracker = self
            .trackers
            .entry(bead_id.to_string())
            .or_insert(BeadTracker {
                repo: fallback_repo,
                scope: String::new(),
                last_generation: 0,
                retries: 0,
                consecutive_reverts: 0,
                highest_tier: None,
                current_agent: None,
                phase_index: 0,
                issue_type: "task".into(),
                dispatch_id: None,
            });
        tracker.retries += 1;

        if tracker.retries >= self.config.max_retries {
            eprintln!(
                "[deadletter] {bead_id}: max retries after exit failure ({})",
                tracker.retries
            );
            return true;
        }

        let repo = tracker.repo.clone();
        let retries = tracker.retries;
        self.queue
            .record_backoff(&repo, bead_id, retries, Instant::now());
        eprintln!(
            "[retry] {bead_id}: agent exited non-zero, retry #{} scheduled",
            retries
        );
        // Preserve workspace on failure so stderr/stream logs are readable
        eprintln!("[retry] {bead_id}: preserving workspace for post-mortem");

        false
    }
}
