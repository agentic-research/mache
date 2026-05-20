//! Auto-thread clustering and thread map building.
//!
//! Sentry span: `reconcile.threading`

use std::collections::HashMap;

use crate::bead::BeadState;
use crate::epic;

use super::Reconciler;

impl Reconciler {
    /// Cluster open beads into threads and persist to hierarchy store.
    /// Only runs when hierarchy store is available. Sequential and SharedScope
    /// clusters become threads; NearDuplicate and Overlapping are left for dedup.
    pub(super) async fn auto_thread(&mut self, beads: &[crate::bead::Bead]) {
        let Some(ref hierarchy) = self.hierarchy else {
            return;
        };

        let open_beads: Vec<&crate::bead::Bead> = beads
            .iter()
            .filter(|b| b.state() == BeadState::Open)
            .collect();
        let owned: Vec<crate::bead::Bead> = open_beads.iter().map(|b| (*b).clone()).collect();
        let clusters = epic::cluster_beads(&owned);

        for cluster in &clusters {
            let should_thread = matches!(
                cluster.relationship,
                epic::ClusterRelationship::Sequential | epic::ClusterRelationship::SharedScope
            );
            if !should_thread || cluster.bead_ids.len() < 2 {
                continue;
            }

            let thread_id = format!("backlog/{}-{}", &cluster.bead_ids[0], &cluster.bead_ids[1]);
            // decade_id must match the prefix extracted by workspace_ops.rs
            // (thread_id.split('/').next()) so list_threads("backlog") finds it.
            // The "backlog" name signals these are auto-clustered beads
            // awaiting triage (by a triage-agent or a human), rather than
            // ignore-me dead-letters.
            let backlog_decade_id = "backlog";

            // Check if any bead in the cluster already has a thread
            let mut already_threaded = false;
            for bid in &cluster.bead_ids {
                let bead_ref = crate::store::WorkRef {
                    repo: owned
                        .iter()
                        .find(|b| b.id == *bid)
                        .map(|b| b.repo.clone())
                        .unwrap_or_default(),
                    scope: String::new(),
                    bead_id: bid.clone(),
                };
                if let Ok(Some(_)) = hierarchy.find_thread_for_bead(&bead_ref).await {
                    already_threaded = true;
                    break;
                }
            }
            if already_threaded {
                continue;
            }

            // Derive feature branch name from the cluster relationship.
            let thread_name = format!("{:?} cluster", cluster.relationship);
            let feature_branch = crate::workspace::thread_branch_name("feature", &thread_name);

            // Ensure the "backlog" decade exists (FK constraint in SQLite backend).
            let _ = hierarchy
                .upsert_decade(&crate::store::DecadeRecord {
                    id: backlog_decade_id.to_string(),
                    title: "Backlog: auto-clustered beads awaiting triage".to_string(),
                    source_path: String::new(),
                    status: "active".to_string(),
                })
                .await;

            // Ensure the feature branch exists in the first bead's repo. Best-effort:
            // if git fails, we still persist the thread — the branch is created lazily
            // when the first dev PR targets it and ensure_thread_branch is called again.
            let first_repo_path = cluster
                .bead_ids
                .first()
                .and_then(|bid| owned.iter().find(|b| b.id == *bid))
                .and_then(|b| self.repo_info.get(&b.repo))
                .map(|(p, _)| p.clone());
            if let Some(ref repo_path) = first_repo_path {
                let _ = crate::workspace::ensure_thread_branch(
                    repo_path,
                    &feature_branch,
                    &self.config.default_branch,
                )
                .await;
            }

            let thread = crate::store::ThreadRecord {
                id: thread_id.clone(),
                name: thread_name,
                decade_id: backlog_decade_id.to_string(),
                feature_branch: Some(feature_branch),
            };
            if let Err(e) = hierarchy.upsert_thread(&thread).await {
                eprintln!("[auto-thread] failed to create thread {thread_id}: {e}");
                continue;
            }
            for bid in &cluster.bead_ids {
                let bead_ref = crate::store::WorkRef {
                    repo: owned
                        .iter()
                        .find(|b| b.id == *bid)
                        .map(|b| b.repo.clone())
                        .unwrap_or_default(),
                    scope: String::new(),
                    bead_id: bid.clone(),
                };
                let _ = hierarchy.add_bead_to_thread(&thread_id, &bead_ref).await;
            }
            eprintln!(
                "[auto-thread] created thread {thread_id} with {} beads ({:?})",
                cluster.bead_ids.len(),
                cluster.relationship
            );
        }
    }

    /// Pre-compute bead→thread mapping for triage.
    /// Done before triage to avoid async calls inside the triage loop
    /// (which would make iterate() non-Send due to AgentHandle borrows).
    pub(super) async fn build_thread_map(
        &self,
        beads: &[crate::bead::Bead],
    ) -> HashMap<String, String> {
        let Some(ref hierarchy) = self.hierarchy else {
            return HashMap::new();
        };

        let mut map = HashMap::new();
        for bead in beads {
            let bead_ref = crate::store::WorkRef {
                repo: bead.repo.clone(),
                scope: String::new(),
                bead_id: bead.id.clone(),
            };
            if let Ok(Some(thread_id)) = hierarchy.find_thread_for_bead(&bead_ref).await {
                map.insert(bead.id.clone(), thread_id);
            }
        }
        map
    }

    /// Phase 5.5: FEATURE ASSEMBLY.
    ///
    /// Called after verify_completed with the set of beads that just reached
    /// `pr_open`. For each, checks whether its thread is now fully complete
    /// (all member beads at `pr_open` / `closed` / `done`). If so, opens a
    /// feature PR from the thread's feature branch into `default_branch`.
    ///
    /// This is the boundary between the dev tier (per-bead compile/test/lint
    /// refinement loop) and the feature tier (assembled feature PR for human
    /// review). Dev PRs target `fix/{bead_id}` → `feature/{thread}`. This
    /// method fires the `feature/{thread}` → `main` PR once all dev work lands.
    ///
    /// Gemini/copilot review and integration verification hook here (not yet
    /// wired — the PR is opened; CI + review bots run on it from there).
    ///
    /// # Borrow structure
    ///
    /// Two explicit phases: Phase 1 borrows `self.hierarchy` (immutable) and
    /// collects owned candidates; Phase 2 drops that borrow and calls
    /// `self.dolt_client()` (needs `&mut self`). The hierarchy borrow and
    /// the mutable borrow never overlap.
    pub(super) async fn assemble_feature_prs(&mut self, newly_pr_open: &[(String, String)]) {
        if self.hierarchy.is_none() {
            return;
        }

        // Owned data collected during Phase 1 so Phase 2 can use &mut self.
        struct Candidate {
            thread_id: String,
            feature_branch: String,
            bead_refs: Vec<crate::store::WorkRef>,
            /// Repo of the first bead — used to look up repo_path in Phase 2.
            first_repo: String,
        }

        // ── Phase 1: read hierarchy (immutable) ──────────────────────────
        // All hierarchy calls happen here. The borrow of self.hierarchy ends
        // at the closing `}` so Phase 2 can call self.dolt_client().
        let candidates: Vec<Candidate> = {
            let hierarchy = self.hierarchy.as_deref().unwrap();

            // Collect unique thread_ids triggered by the newly-completed beads.
            let mut triggered: Vec<String> = Vec::new();
            for (bead_id, repo) in newly_pr_open {
                let bead_ref = crate::store::WorkRef {
                    repo: repo.clone(),
                    scope: String::new(),
                    bead_id: bead_id.clone(),
                };
                if let Ok(Some(thread_id)) = hierarchy.find_thread_for_bead(&bead_ref).await
                    && !triggered.contains(&thread_id)
                {
                    triggered.push(thread_id);
                }
            }

            let mut out: Vec<Candidate> = Vec::new();
            for thread_id in triggered {
                // decade_id is the prefix before the first '/' in the thread id.
                // auto_thread stores threads with decade_id = "backlog" and thread_ids
                // like "backlog/{bead1}-{bead2}", so this correctly gives "backlog".
                let decade_id = thread_id
                    .split('/')
                    .next()
                    .unwrap_or(&thread_id)
                    .to_string();
                let threads = match hierarchy.list_threads(&decade_id).await {
                    Ok(t) => t,
                    Err(e) => {
                        eprintln!("[feature-assembly] list_threads({decade_id}) failed: {e}");
                        continue;
                    }
                };
                let thread = match threads.into_iter().find(|t| t.id == thread_id) {
                    Some(t) => t,
                    None => continue,
                };
                let feature_branch = match thread.feature_branch {
                    // No feature branch set — standalone bead PRs directly to default.
                    None => continue,
                    Some(b) => b,
                };
                let bead_refs = match hierarchy.list_beads_in_thread(&thread_id).await {
                    Ok(r) if !r.is_empty() => r,
                    _ => continue,
                };
                let first_repo = bead_refs
                    .first()
                    .map(|r| r.repo.clone())
                    .unwrap_or_default();
                out.push(Candidate {
                    thread_id,
                    feature_branch,
                    bead_refs,
                    first_repo,
                });
            }
            out
            // self.hierarchy borrow released here — Phase 2 may use &mut self
        };

        // ── Phase 2: check statuses + fire PRs (uses &mut self) ──────────
        for candidate in candidates {
            let repo_path = match self
                .repo_info
                .get(&candidate.first_repo)
                .map(|(p, _)| p.clone())
            {
                Some(p) => p,
                None => continue,
            };

            // All thread members must be at a terminal dev state.
            let mut all_done = true;
            for bead_ref in &candidate.bead_refs {
                if let Some(client) = self.dolt_client(&bead_ref.repo).await {
                    match client.get_status(&bead_ref.bead_id).await {
                        Ok(Some(ref s)) if s == "pr_open" || s == "closed" || s == "done" => {}
                        _ => {
                            all_done = false;
                            break;
                        }
                    }
                } else {
                    all_done = false;
                    break;
                }
            }

            if !all_done {
                continue;
            }

            eprintln!(
                "[feature-assembly] thread {} complete ({} beads) — \
                 opening feature PR from {} → {}",
                candidate.thread_id,
                candidate.bead_refs.len(),
                candidate.feature_branch,
                self.config.default_branch
            );

            // Ensure the feature branch exists before the PR (idempotent).
            // This is a safety net: auto_thread calls ensure_thread_branch on creation,
            // but git ops are best-effort and may have failed.
            let _ = crate::workspace::ensure_thread_branch(
                &repo_path,
                &candidate.feature_branch,
                &self.config.default_branch,
            )
            .await;

            let _ = crate::workspace::merge_or_pr_with_base(
                &repo_path,
                &candidate.feature_branch,
                &candidate.thread_id,
                "feature",
                Some(&self.config.default_branch),
            )
            .await;
        }
    }
}
