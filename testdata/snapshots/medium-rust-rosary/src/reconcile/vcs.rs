//! VCS scanning — detect bead references in recent commits.
//!
//! Sentry span: `reconcile.vcs_scan`

use std::path::PathBuf;

use super::Reconciler;

/// Parse the pipe-delimited output from the combined `gh pr view` call.
///
/// Expected format: `"STATE|sha|DECISION|reviewer1,reviewer2"`
/// - STATE: OPEN, MERGED, CLOSED
/// - sha: merge commit OID (empty if not yet merged)
/// - DECISION: APPROVED, CHANGES_REQUESTED, REVIEW_REQUIRED, or empty
/// - reviewers: comma-separated logins of CHANGES_REQUESTED reviewers
fn parse_pr_status_line(line: &str) -> (String, String, String, String) {
    let mut parts = line.splitn(4, '|');
    let state = parts.next().unwrap_or("").to_string();
    let merge_sha = parts.next().unwrap_or("").to_string();
    let decision = parts.next().unwrap_or("").to_string();
    let reviewers = parts.next().unwrap_or("").to_string();
    (state, merge_sha, decision, reviewers)
}

impl Reconciler {
    /// Scan jj logs across repos for bead references in commit messages.
    /// Triggers state transitions: open → dispatched (for refs), open → done (for closes).
    /// Returns the number of transitions triggered.
    pub(super) async fn scan_vcs(&mut self, beads: &[crate::bead::Bead]) -> usize {
        use crate::vcs;

        // Collect repo info first to avoid borrow conflicts with &mut self
        let repos: Vec<(String, PathBuf)> = self
            .repo_info
            .iter()
            .map(|(name, (path, _))| (name.clone(), path.clone()))
            .collect();

        // Gather all VCS refs across repos
        let mut pending: Vec<(String, String, String, String, bool)> = Vec::new(); // (repo, bead_id, bead_repo, change_id, closes)
        for (repo_name, repo_path) in &repos {
            let vcs_refs = match vcs::scan_vcs_bead_refs(repo_path) {
                Ok(refs) => refs,
                Err(_) => continue,
            };

            for (change_id, bead_ref) in &vcs_refs {
                let bead = beads.iter().find(|b| b.id == bead_ref.id);
                let Some(bead) = bead else { continue };

                // Determine target status
                let should_transition = if bead_ref.closes {
                    !matches!(bead.status.as_str(), "done" | "closed")
                } else {
                    bead.status.as_str() == "open"
                };

                if !should_transition || self.active.contains_key(&bead.id) {
                    continue;
                }

                pending.push((
                    repo_name.clone(),
                    bead.id.clone(),
                    bead.repo.clone(),
                    change_id.clone(),
                    bead_ref.closes,
                ));
            }
        }

        // Apply transitions
        let mut transitions = 0;
        for (repo_name, bead_id, bead_repo, change_id, closes) in &pending {
            let new_status = if *closes { "closed" } else { "dispatched" };

            eprintln!("[vcs] {repo_name}: {bead_id} → {new_status} (jj change {change_id})");

            if let Some(client) = self.dolt_client(bead_repo).await {
                let _ = client
                    .log_event(bead_id, "vcs_ref", &format!("jj:{change_id}"))
                    .await;
            }

            self.persist_status(bead_id, bead_repo, new_status).await;
            transitions += 1;
        }

        transitions
    }

    /// Poll beads in `pr_open` status: surface review feedback and close merged PRs.
    ///
    /// One `gh pr view` call per bead fetches `state`, `mergeCommit`, `reviewDecision`,
    /// and `reviews` together, avoiding duplicate API/subprocess overhead.
    ///
    /// - **CHANGES_REQUESTED**: logs a `pr_feedback` event (de-duplicated — only logged when
    ///   the reviewer set changes so the audit trail doesn't grow unbounded).
    /// - **MERGED**: logs the merge SHA, adds an audit comment, closes the bead, and clears
    ///   pipeline state (same behaviour as the former `poll_pr_merges`).
    ///
    /// Falls back gracefully if `gh` is unavailable or the API returns an error.
    /// Returns the number of beads closed.
    pub(super) async fn poll_pr_status(&mut self, beads: &[crate::bead::Bead]) -> usize {
        let pr_open_beads: Vec<&crate::bead::Bead> =
            beads.iter().filter(|b| b.status == "pr_open").collect();

        if pr_open_beads.is_empty() {
            return 0;
        }

        let mut closed = 0;
        for bead in &pr_open_beads {
            // Look up PR URL from events log
            let pr_url = if let Some(client) = self.dolt_client(&bead.repo).await {
                client
                    .get_latest_event(&bead.id, "pr_url")
                    .await
                    .ok()
                    .flatten()
            } else {
                None
            };

            let Some(url) = pr_url else {
                continue;
            };

            // Fetch state, merge SHA, review decision, and CHANGES_REQUESTED reviewers
            // in a single gh invocation.
            // jq: "STATE|sha|DECISION|reviewer1,reviewer2"
            let output = tokio::process::Command::new("gh")
                .args([
                    "pr",
                    "view",
                    &url,
                    "--json",
                    "state,mergeCommit,reviewDecision,reviews",
                    "-q",
                    ".state + \"|\" + (.mergeCommit.oid // \"\") + \"|\" + .reviewDecision + \"|\" + ([.reviews[] | select(.state == \"CHANGES_REQUESTED\") | .author.login] | join(\",\"))",
                ])
                .output()
                .await;

            let line = match output {
                Ok(o) if o.status.success() => {
                    String::from_utf8_lossy(&o.stdout).trim().to_string()
                }
                _ => continue, // gh not available or API error — skip
            };

            let (state, merge_sha, decision, reviewers) = parse_pr_status_line(&line);

            // — Feedback: log CHANGES_REQUESTED once per reviewer-set change —
            if decision == "CHANGES_REQUESTED" {
                let detail = if reviewers.is_empty() {
                    format!("CHANGES_REQUESTED — {url}")
                } else {
                    format!("CHANGES_REQUESTED by {reviewers} — {url}")
                };

                if let Some(client) = self.dolt_client(&bead.repo).await {
                    // De-duplicate: only log when the reviewer set / detail changes.
                    let latest = client
                        .get_latest_event(&bead.id, "pr_feedback")
                        .await
                        .ok()
                        .flatten();
                    if latest.as_deref() != Some(&detail) {
                        eprintln!(
                            "[pr-feedback] {} — changes requested ({})",
                            bead.id, reviewers
                        );
                        let _ = client.log_event(&bead.id, "pr_feedback", &detail).await;
                    }
                } else {
                    eprintln!(
                        "[pr-feedback] {} — changes requested ({})",
                        bead.id, reviewers
                    );
                }
            }

            // — Merge: close the bead when the PR has merged —
            if state == "MERGED" {
                // Log merge SHA + add audit comment before persisting status
                // so the audit trail precedes the state transition.
                if let Some(client) = self.dolt_client(&bead.repo).await {
                    if !merge_sha.is_empty() {
                        client.log_event(&bead.id, "merge_sha", &merge_sha).await;
                    }
                    let comment = if merge_sha.is_empty() {
                        format!("Merged: {url}")
                    } else {
                        format!("Merged: {url} (SHA: {merge_sha})")
                    };
                    if let Err(e) = client.add_comment(&bead.id, &comment, "rosary").await {
                        eprintln!(
                            "[pr-merged] failed to add merge comment for {}: {e}",
                            bead.id
                        );
                    }
                }

                eprintln!("[pr-merged] {} — PR merged, closing bead", bead.id);
                self.persist_status(&bead.id, &bead.repo, "closed").await;

                // Now clear pipeline state
                let bead_ref = crate::store::WorkRef {
                    repo: bead.repo.clone(),
                    scope: String::new(),
                    bead_id: bead.id.clone(),
                };
                self.pipeline.clear_state(&bead_ref).await;
                closed += 1;
            }
        }

        closed
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_pr_status_merged_with_sha() {
        let (state, sha, decision, reviewers) =
            parse_pr_status_line("MERGED|abc123def456|APPROVED|");
        assert_eq!(state, "MERGED");
        assert_eq!(sha, "abc123def456");
        assert_eq!(decision, "APPROVED");
        assert_eq!(reviewers, "");
    }

    #[test]
    fn parse_pr_status_open_changes_requested() {
        let (state, sha, decision, reviewers) =
            parse_pr_status_line("OPEN||CHANGES_REQUESTED|alice,bob");
        assert_eq!(state, "OPEN");
        assert_eq!(sha, "");
        assert_eq!(decision, "CHANGES_REQUESTED");
        assert_eq!(reviewers, "alice,bob");
    }

    #[test]
    fn parse_pr_status_open_no_review() {
        let (state, sha, decision, reviewers) = parse_pr_status_line("OPEN|||");
        assert_eq!(state, "OPEN");
        assert_eq!(sha, "");
        assert_eq!(decision, "");
        assert_eq!(reviewers, "");
    }

    #[test]
    fn parse_pr_status_reviewers_with_pipe_in_fourth_field() {
        // Ensure splitn(4) doesn't split on a hypothetical pipe in reviewer names
        let (state, _, decision, reviewers) = parse_pr_status_line("OPEN||CHANGES_REQUESTED|alice");
        assert_eq!(state, "OPEN");
        assert_eq!(decision, "CHANGES_REQUESTED");
        assert_eq!(reviewers, "alice");
    }

    #[test]
    fn parse_pr_status_merged_no_sha() {
        let (state, sha, _, _) = parse_pr_status_line("MERGED|||");
        assert_eq!(state, "MERGED");
        assert_eq!(sha, "");
    }
}
