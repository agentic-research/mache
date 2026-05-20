//! Phase 3: Triage — score and filter beads for dispatch.
//!
//! Sentry span: `reconcile.triage`
//! Attributes: beads.scanned, beads.triaged, filter.reason

use std::collections::HashMap;
use std::time::Instant;

use crate::bead::BeadState;
use crate::epic;
use crate::queue::{self, QueueEntry};

use super::Reconciler;

impl Reconciler {
    /// Score open beads, apply filters, enqueue above threshold.
    /// If --bead is set, skip normal triage and only enqueue that bead.
    pub(super) fn triage(
        &mut self,
        beads: &[crate::bead::Bead],
        thread_map: &HashMap<String, String>,
        cross_repo_blocked: &std::collections::HashSet<String>,
    ) -> usize {
        let target_filter = self.config.target_bead.clone();
        let now = chrono::Utc::now();
        let mut triaged = 0;

        for bead in beads {
            if let Some(ref target) = target_filter {
                if bead.id != *target {
                    continue;
                }
                // Targeted dispatch: log if bead is in non-open state
                if bead.status != "open" {
                    eprintln!(
                        "[triage] targeted bead {target} is '{}', overriding",
                        bead.status
                    );
                }
            } else if bead.state() != BeadState::Open {
                continue;
            }
            if self.active.contains_key(&bead.id) {
                continue;
            }
            if self.queue.is_deadlettered(&bead.repo, &bead.id) {
                continue;
            }

            // Severity floor: skip beads below minimum priority level.
            // Targeted dispatch (--bead) bypasses — explicit intent overrides floor.
            if !queue::passes_severity_floor(bead, self.queue.min_priority)
                && target_filter.is_none()
            {
                continue;
            }

            // Skip epics — they're planning beads, not actionable work.
            // Targeted dispatch bypasses — user explicitly chose this bead.
            if bead.issue_type == "epic" && target_filter.is_none() {
                continue;
            }

            // Golden Rule 12: implementation beads need refinement (5-whys)
            // before dispatch. Unrefined beads need a research pass first.
            // Targeted dispatch (--bead) bypasses — explicit user intent overrides gate.
            if bead.needs_refinement() && target_filter.is_none() {
                eprintln!(
                    "[refinement] deferring {} — description too short, needs 5-whys (rule 12)",
                    bead.id
                );
                continue;
            }

            // Dependency-aware: hard-filter beads with unresolved deps.
            // Targeted dispatch (--bead) bypasses this — explicit override.
            if bead.is_blocked() && target_filter.is_none() {
                continue;
            }

            // Cross-repo blocking (ADR-0009): defer if any Asserted/Derived blocking
            // dep exists whose target bead is not yet closed.
            if cross_repo_blocked.contains(&bead.id) && target_filter.is_none() {
                continue;
            }

            // Dispatch approval gate (Warp-style): when require_approval is on,
            // beads from non-Approved repos are held. --bead bypasses.
            //
            // We must check BOTH config.repo (statically registered) and
            // remote_repos (cloned via backend at runtime) — the reconciler
            // scans both, and remote_repos entries are created with
            // approval: Approved so they should pass when the gate is on.
            if self.config.require_approval && target_filter.is_none() {
                let admits = self
                    .config
                    .repo
                    .iter()
                    .chain(self.remote_repos.iter())
                    .find(|r| r.name == bead.repo)
                    .is_some_and(|r| r.approval.admits());
                if !admits {
                    eprintln!(
                        "[approval] {} held — repo '{}' is not approved for dispatch",
                        bead.id, bead.repo
                    );
                    continue;
                }
            }

            // Per-repo coordination: don't dispatch to a repo that
            // already has an active agent.
            let repo_busy = self.active.keys().any(|active_id| {
                self.trackers
                    .get(active_id)
                    .is_some_and(|t| t.repo == bead.repo)
            });
            if repo_busy {
                continue;
            }

            // Thread-aware sequencing: defer if a thread-mate is currently active.
            if let Some(thread_id) = thread_map.get(&bead.id) {
                let thread_mate_active = self
                    .active
                    .keys()
                    .any(|active_id| thread_map.get(active_id).is_some_and(|at| at == thread_id));
                if thread_mate_active {
                    eprintln!(
                        "[thread] deferring {} — thread-mate active (thread {thread_id})",
                        bead.id
                    );
                    continue;
                }
            }

            // Dedup: skip if semantically dominated by an active or queued bead.
            let active_beads: Vec<&crate::bead::Bead> = beads
                .iter()
                .filter(|other| other.id != bead.id)
                .filter(|other| {
                    self.active.contains_key(&other.id)
                        || self.queue.contains(&other.repo, &other.id)
                })
                .collect();
            if let Some(dominator) = epic::is_dominated_by(bead, &active_beads) {
                eprintln!(
                    "[dedup] skipping {} — too similar to active {dominator}",
                    bead.id
                );
                continue;
            }

            // File overlap: defer if candidate's files conflict with active/queued.
            if let Some(blocker) = epic::has_file_overlap(bead, &active_beads) {
                eprintln!(
                    "[file-overlap] deferring {} — files conflict with active {blocker}",
                    bead.id
                );
                continue;
            }

            let retries = self.queue.retries(&bead.repo, &bead.id);
            let score = if target_filter.is_some() {
                // Targeted dispatch: force max score to bypass threshold
                1.0
            } else {
                let mut s = if self.config.overnight {
                    queue::triage_score_overnight(bead, retries, now)
                } else {
                    queue::triage_score(bead, retries, now)
                };

                // Self-managed repo preference: boost dogfooding beads.
                if self
                    .config
                    .repo
                    .iter()
                    .any(|r| r.name == bead.repo && r.self_managed)
                {
                    s = (s + 0.15).min(1.0);
                }
                s
            };

            if score >= self.config.triage_threshold {
                let bead_gen = bead.generation();

                // Skip if already processed at this generation —
                // UNLESS the bead has pending retries
                if let Some(tracker) = self.trackers.get(&bead.id)
                    && tracker.last_generation == bead_gen
                    && tracker.retries == 0
                {
                    continue;
                }

                // Plugin triage hooks: external tools can veto dispatch.
                let ctx = crate::plugin::PluginContext::new(&bead.id, &bead.repo);
                if let Some(reason) = self.plugin_registry.call_triage_hooks(&ctx) {
                    eprintln!("[plugin-triage] skipping {} — {reason}", bead.id);
                    continue;
                }

                let enqueued = self.queue.enqueue(QueueEntry {
                    bead_id: bead.id.clone(),
                    repo: bead.repo.clone(),
                    score,
                    enqueued_at: Instant::now(),
                    retries,
                    generation: bead_gen,
                });
                if enqueued {
                    triaged += 1;
                }
            }
        }

        triaged
    }
}
