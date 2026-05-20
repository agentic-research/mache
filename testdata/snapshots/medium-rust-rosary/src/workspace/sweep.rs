//! Workspace helpers: directory resolution, VCS creation/cleanup, orphan sweep, merge.

use anyhow::{Context, Result};
use std::path::{Path, PathBuf};

// ---------------------------------------------------------------------------
// Directory resolution
// ---------------------------------------------------------------------------

/// Resolve the workspace directory for a bead, creating the parent if needed.
/// Uses `~/.rsry/worktrees/{repo}/{id}` — user-scoped, survives repo cleans,
/// doesn't collide with CC's .claude/worktrees/ or other tools.
pub fn workspace_dir(repo_path: &Path, id: &str) -> PathBuf {
    let repo_name = repo_path
        .file_name()
        .map(|n| n.to_string_lossy().to_string())
        .unwrap_or_else(|| "unknown".to_string());
    let home = dirs_next::home_dir().unwrap_or_else(|| PathBuf::from("."));
    let ws_root = home.join(".rsry").join("worktrees").join(&repo_name);
    // Best-effort mkdir — callers handle the actual VCS error if this fails
    let _ = std::fs::create_dir_all(&ws_root);
    ws_root.join(id)
}

// ---------------------------------------------------------------------------
// jj isolation
// ---------------------------------------------------------------------------

/// Create a jj workspace for isolated agent work.
pub(super) async fn create_jj_workspace(repo_path: &Path, id: &str) -> Result<PathBuf> {
    let workspace_name = format!("fix-{id}");
    let workspace_path = workspace_dir(repo_path, id);

    let output = tokio::process::Command::new("jj")
        .args([
            "workspace",
            "add",
            &workspace_path.to_string_lossy(),
            "--name",
            &workspace_name,
        ])
        .current_dir(repo_path)
        .output()
        .await
        .context("jj workspace add")?;

    if !output.status.success() {
        let stderr = String::from_utf8_lossy(&output.stderr);
        anyhow::bail!("jj workspace add failed: {stderr}");
    }

    // Set jj description so workspace shows bead context in `jj workspace list`
    let _ = tokio::process::Command::new("jj")
        .args(["describe", "-m", &format!("bead:{id}")])
        .current_dir(&workspace_path)
        .output()
        .await;

    eprintln!(
        "[workspace] created jj workspace: {}",
        workspace_path.display()
    );
    Ok(workspace_path)
}

/// Clean up a jj workspace. Best-effort — don't propagate errors.
pub fn cleanup_jj_workspace(repo_path: &Path, id: &str) {
    let workspace_name = format!("fix-{id}");
    let _ = std::process::Command::new("jj")
        .args(["workspace", "forget", &workspace_name])
        .current_dir(repo_path)
        .output();

    let ws_dir = workspace_dir(repo_path, id);
    let _ = std::fs::remove_dir_all(ws_dir);
}

// ---------------------------------------------------------------------------
// git worktree isolation (fallback)
// ---------------------------------------------------------------------------

/// Create a git worktree for isolated work.
///
/// Handles the common case where a previous dispatch left behind a
/// `fix/{id}` branch — deletes the stale branch and retries.
pub(super) async fn create_git_worktree(repo_path: &Path, id: &str) -> Result<PathBuf> {
    let branch_name = format!("fix/{id}");
    let worktree_path = workspace_dir(repo_path, id);

    // Fetch latest origin/main so the worktree branches from current remote HEAD,
    // not stale local HEAD. Without this, agents include already-merged changes
    // in their diffs — the root cause of every duplicate PR in the overnight session.
    let has_remote = tokio::process::Command::new("git")
        .args(["fetch", "origin", "main"])
        .current_dir(repo_path)
        .output()
        .await
        .map(|o| o.status.success())
        .unwrap_or(false);

    // Branch from origin/main if available, otherwise HEAD (local repos without remotes).
    // Always prefer origin/main to avoid including unmerged local commits in agent work.
    let wt_str = worktree_path.to_string_lossy().to_string();
    let start_point = if has_remote { "origin/main" } else { "HEAD" };
    let mut args: Vec<&str> = vec!["worktree", "add", &wt_str, "-b", &branch_name];
    if has_remote {
        args.push("origin/main");
    }
    eprintln!("[workspace] branching {branch_name} from {start_point}");

    let output = tokio::process::Command::new("git")
        .args(&args)
        .current_dir(repo_path)
        .output()
        .await
        .context("git worktree add")?;

    if output.status.success() {
        eprintln!(
            "[workspace] created git worktree: {}",
            worktree_path.display()
        );
        return Ok(worktree_path);
    }

    let stderr = String::from_utf8_lossy(&output.stderr);

    // Branch already exists from a previous (failed) dispatch — clean up and retry.
    if stderr.contains("already exists") {
        eprintln!("[workspace] branch {branch_name} already exists, cleaning up stale state");
        // Remove stale worktree dir if it exists
        let _ = std::fs::remove_dir_all(&worktree_path);
        // Prune worktree references that point to missing directories
        let _ = tokio::process::Command::new("git")
            .args(["worktree", "prune"])
            .current_dir(repo_path)
            .output()
            .await;
        // Delete the stale branch
        let _ = tokio::process::Command::new("git")
            .args(["branch", "-D", &branch_name])
            .current_dir(repo_path)
            .output()
            .await;

        // Retry
        let retry = tokio::process::Command::new("git")
            .args([
                "worktree",
                "add",
                &worktree_path.to_string_lossy(),
                "-b",
                &branch_name,
            ])
            .current_dir(repo_path)
            .output()
            .await
            .context("git worktree add (retry)")?;

        if retry.status.success() {
            eprintln!(
                "[workspace] created git worktree (after cleanup): {}",
                worktree_path.display()
            );
            return Ok(worktree_path);
        }
        let retry_err = String::from_utf8_lossy(&retry.stderr);
        anyhow::bail!("git worktree add failed after cleanup: {retry_err}");
    }

    anyhow::bail!("git worktree add failed: {stderr}");
}

/// Clean up a git worktree. Best-effort.
pub fn cleanup_git_worktree(repo_path: &Path, id: &str) {
    let worktree_path = workspace_dir(repo_path, id);
    let _ = std::process::Command::new("git")
        .args([
            "worktree",
            "remove",
            &worktree_path.to_string_lossy(),
            "--force",
        ])
        .current_dir(repo_path)
        .output();

    // Also delete the branch
    let branch_name = format!("fix/{id}");
    let _ = std::process::Command::new("git")
        .args(["branch", "-D", &branch_name])
        .current_dir(repo_path)
        .output();
}

// ---------------------------------------------------------------------------
// Branch sweep
// ---------------------------------------------------------------------------

/// Result of a branch sweep operation.
pub struct BranchSweepResult {
    /// Branches checked.
    pub checked: usize,
    /// Branches deleted (or that would be deleted in dry-run).
    pub deleted: usize,
    /// Branches skipped because bead is not closed/done.
    pub skipped_active: usize,
    /// Branches skipped because they have unmerged commits.
    pub skipped_unmerged: usize,
}

/// Garbage-collect merged `fix/{bead-id}` branches from origin.
///
/// Safety rules:
/// 1. **Closed bead check**: the branch is only deleted if the corresponding
///    bead is in `closed` or `done` status in the local `.beads/` store.
/// 2. **Merged check**: the branch must have no commits ahead of `main`
///    (i.e. appears in `git branch -r --merged origin/main`).
///
/// `dry_run = true` logs what would be deleted without touching anything.
pub async fn sweep_agent_branches(repo_path: &Path, dry_run: bool) -> BranchSweepResult {
    let mut result = BranchSweepResult {
        checked: 0,
        deleted: 0,
        skipped_active: 0,
        skipped_unmerged: 0,
    };

    // List all remote branches
    let branches_out = tokio::process::Command::new("git")
        .args(["branch", "-r", "--format", "%(refname:short)"])
        .current_dir(repo_path)
        .output()
        .await;

    let branches_text = match branches_out {
        Ok(o) if o.status.success() => String::from_utf8_lossy(&o.stdout).into_owned(),
        _ => return result,
    };

    // Fetch the set of branches already merged into origin/main — one call,
    // then use a HashSet for O(1) lookup per branch.
    // If this fails (no origin, no origin/main) we abort: an empty set would
    // incorrectly report every branch as "unmerged" and silently skip them all.
    let merged_out = tokio::process::Command::new("git")
        .args(["branch", "-r", "--merged", "origin/main"])
        .current_dir(repo_path)
        .output()
        .await;

    let merged_set: std::collections::HashSet<String> = match merged_out {
        Ok(o) if o.status.success() => String::from_utf8_lossy(&o.stdout)
            .lines()
            .map(|l| l.trim().to_string())
            .collect(),
        Ok(o) => {
            eprintln!(
                "[sweep] git branch --merged failed: {}",
                String::from_utf8_lossy(&o.stderr).trim()
            );
            return result;
        }
        Err(e) => {
            eprintln!("[sweep] git branch --merged error: {e}");
            return result;
        }
    };

    // Connect to the local bead store only if .beads/ already exists.
    // Connecting would create the directory + database on first access, which
    // is a surprising side-effect of running `rsry sweep` in a non-beads repo.
    let beads_dir = crate::resolve_beads_dir(repo_path);
    let store = if beads_dir.exists() {
        crate::bead_sqlite::connect_bead_store(&beads_dir)
            .await
            .ok()
    } else {
        None
    };
    let repo_name = repo_path
        .file_name()
        .map(|n| n.to_string_lossy().to_string())
        .unwrap_or_default();

    for line in branches_text.lines() {
        let remote_branch = line.trim(); // e.g. "origin/fix/rsry-abc123"
        let Some(local) = remote_branch.strip_prefix("origin/") else {
            continue;
        };
        // Only handle agent dispatch branches (fix/{bead-id})
        let Some(bead_id) = local.strip_prefix("fix/") else {
            continue;
        };
        // bead_id must contain a dash — skip generic fix/* branches
        if !bead_id.contains('-') {
            continue;
        }

        result.checked += 1;

        // Safety rule 1 (most critical): branch must be merged into main.
        // Check first — an unmerged branch is unsafe to delete regardless of bead status.
        if !merged_set.contains(remote_branch) {
            eprintln!("[sweep] {remote_branch}: has unmerged commits — skipping");
            result.skipped_unmerged += 1;
            continue;
        }

        // Safety rule 2: bead must be closed/done.
        let is_closed = if let Some(ref store) = store {
            match store.get_bead(bead_id, &repo_name).await {
                Ok(Some(b)) => matches!(b.status.as_str(), "closed" | "done"),
                _ => false,
            }
        } else {
            false
        };

        if !is_closed {
            result.skipped_active += 1;
            continue;
        }

        if dry_run {
            eprintln!("[sweep] would delete {remote_branch} (bead {bead_id} closed, merged)");
            result.deleted += 1;
        } else {
            eprintln!("[sweep] deleting {remote_branch} (bead {bead_id} closed, merged)");
            let del = tokio::process::Command::new("git")
                .args(["push", "origin", "--delete", local])
                .current_dir(repo_path)
                .output()
                .await;
            match del {
                Ok(o) if o.status.success() => {
                    result.deleted += 1;
                }
                Ok(o) => {
                    eprintln!(
                        "[sweep] failed to delete {remote_branch}: {}",
                        String::from_utf8_lossy(&o.stderr).trim()
                    );
                }
                Err(e) => {
                    eprintln!("[sweep] failed to delete {remote_branch}: {e}");
                }
            }
        }
    }

    result
}

// ---------------------------------------------------------------------------
// Orphan sweep
// ---------------------------------------------------------------------------

/// Scan `.rsry-workspaces/` directories and clean up any that don't
/// correspond to active bead IDs. Call on startup to reclaim leaked workspaces.
pub fn sweep_orphaned(repo_paths: &[PathBuf], active_bead_ids: &[String]) {
    let home = dirs_next::home_dir().unwrap_or_else(|| PathBuf::from("."));
    for repo_path in repo_paths {
        let repo_name = repo_path
            .file_name()
            .map(|n| n.to_string_lossy().to_string())
            .unwrap_or_else(|| "unknown".to_string());
        let ws_root = home.join(".rsry").join("worktrees").join(&repo_name);
        if !ws_root.exists() {
            continue;
        }
        let Ok(entries) = std::fs::read_dir(&ws_root) else {
            continue;
        };
        for entry in entries.flatten() {
            let name = entry.file_name().to_string_lossy().to_string();
            if !active_bead_ids.contains(&name) {
                eprintln!("[sweep] cleaning orphaned workspace: {name}");
                cleanup_jj_workspace(repo_path, &name);
                cleanup_git_worktree(repo_path, &name);
                let _ = std::fs::remove_dir_all(entry.path());
            }
        }
    }
}

// ---------------------------------------------------------------------------
// Terminal step: push branch + create PR
// ---------------------------------------------------------------------------

/// Result of the terminal merge_or_pr step.
pub struct TerminalResult {
    /// Human-readable summary of what happened.
    pub message: String,
    /// PR URL if one was created.
    pub pr_url: Option<String>,
}

/// Derive the feature branch name for a thread.
///
/// Format: `{prefix}/{thread_name}` where prefix comes from config
/// (default "rosary"). Thread name is slugified.
pub fn thread_branch_name(prefix: &str, thread_name: &str) -> String {
    let slug: String = thread_name
        .to_lowercase()
        .chars()
        .map(|c| {
            if c.is_alphanumeric() || c == '-' {
                c
            } else {
                '-'
            }
        })
        .collect::<String>()
        .replace("--", "-")
        .trim_matches('-')
        .to_string();
    format!("{prefix}/{slug}")
}

/// Ensure a feature branch exists for a thread, creating from base if needed.
pub async fn ensure_thread_branch(repo_path: &Path, branch: &str, base: &str) -> Result<()> {
    // Check if branch already exists (local or remote)
    let check = tokio::process::Command::new("git")
        .args(["rev-parse", "--verify", branch])
        .current_dir(repo_path)
        .output()
        .await;

    if let Ok(ref out) = check
        && out.status.success()
    {
        return Ok(()); // Already exists locally
    }

    // Try remote
    let remote_ref = format!("origin/{branch}");
    let check_remote = tokio::process::Command::new("git")
        .args(["rev-parse", "--verify", &remote_ref])
        .current_dir(repo_path)
        .output()
        .await;

    if let Ok(ref out) = check_remote
        && out.status.success()
    {
        // Track the remote branch locally
        let _ = tokio::process::Command::new("git")
            .args(["checkout", "-b", branch, &remote_ref])
            .current_dir(repo_path)
            .output()
            .await;
        // Switch back to original branch
        let _ = tokio::process::Command::new("git")
            .args(["checkout", "-"])
            .current_dir(repo_path)
            .output()
            .await;
        return Ok(());
    }

    // Create new branch from base
    let create = tokio::process::Command::new("git")
        .args(["branch", branch, base])
        .current_dir(repo_path)
        .output()
        .await
        .context("creating thread branch")?;

    if !create.status.success() {
        let stderr = String::from_utf8_lossy(&create.stderr);
        anyhow::bail!("failed to create thread branch {branch} from {base}: {stderr}");
    }

    // Push to remote so dev agents can branch from it
    let _ = tokio::process::Command::new("git")
        .args(["push", "-u", "origin", branch])
        .current_dir(repo_path)
        .output()
        .await;

    eprintln!("[thread] created branch {branch} from {base}");
    Ok(())
}

/// Terminal step: push branch and create a PR.
///
/// Called after an agent completes work and passes verification. Always pushes
/// the branch and creates a PR — branch protection rules require it.
///
/// `repo_path` should be the MAIN repo (not the worktree).
/// `base` is the PR target branch (thread branch for dev-agents, main for feature-agent).
pub async fn merge_or_pr(
    repo_path: &Path,
    branch: &str,
    bead_id: &str,
    issue_type: &str,
) -> Result<TerminalResult> {
    merge_or_pr_with_base(repo_path, branch, bead_id, issue_type, None).await
}

/// Terminal step with explicit base branch override.
///
/// Rebases the work branch onto `base` if provided, otherwise onto `"main"`.
/// The PR base is resolved from the same effective target so the rebase and
/// PR base are always consistent. Callers that know the configured default
/// branch should pass it explicitly rather than relying on the "main" fallback.
pub async fn merge_or_pr_with_base(
    repo_path: &Path,
    branch: &str,
    bead_id: &str,
    issue_type: &str,
    base: Option<&str>,
) -> Result<TerminalResult> {
    // Golden Rule 11: every commit must reference a bead.
    enforce_bead_refs(repo_path, branch, bead_id).await;

    // Rebase onto latest base to avoid stale-base PRs (git-k8s desired state).
    // Without this, worktrees created hours ago include already-merged changes in the diff.
    let rebase_target = base.unwrap_or("main");
    match rebase_onto_latest(repo_path, bead_id, rebase_target).await {
        Ok(()) => {}
        Err(e) => {
            eprintln!("[terminal] {bead_id}: rebase failed ({e}), PR will have stale base");
            // Continue anyway — a stale PR is better than no PR. Human can rebase.
        }
    }

    // Push the branch to origin
    let push = tokio::process::Command::new("git")
        .args(["push", "-u", "origin", branch])
        .current_dir(repo_path)
        .output()
        .await
        .context("pushing branch")?;

    if !push.status.success() {
        let stderr = String::from_utf8_lossy(&push.stderr);
        // No remote? Fall back to ff-merge (local repos, test environments).
        if stderr.contains("does not appear to be a git repository")
            || stderr.contains("No configured push destination")
            || stderr.contains("No such remote")
        {
            return merge_local(repo_path, branch, bead_id).await;
        }
        anyhow::bail!("[terminal] {bead_id}: push failed: {stderr}");
    }
    eprintln!("[terminal] {bead_id}: pushed {branch}");

    // Try to create PR via GitHub App or PAT — use the same effective base as
    // the rebase target so both operations are consistent.
    let pr_url =
        match create_pr_for_bead(repo_path, branch, bead_id, issue_type, Some(rebase_target)).await
        {
            Ok(url) => Some(url),
            Err(e) => {
                eprintln!(
                    "[terminal] {bead_id}: PR creation failed ({e}), branch pushed for manual PR"
                );
                None
            }
        };

    let message = if let Some(ref url) = pr_url {
        format!("pushed {branch}, PR: {url}")
    } else {
        format!("pushed {branch} — create PR manually")
    };
    eprintln!("[terminal] {bead_id}: {message}");
    Ok(TerminalResult { message, pr_url })
}

/// Local-only fallback: ff-merge to main when there's no remote.
async fn merge_local(repo_path: &Path, branch: &str, bead_id: &str) -> Result<TerminalResult> {
    eprintln!("[terminal] {bead_id}: no remote, falling back to local ff-merge");
    let merge = tokio::process::Command::new("git")
        .args(["merge", "--ff-only", branch])
        .current_dir(repo_path)
        .output()
        .await
        .context("ff-merging to main")?;

    if merge.status.success() {
        let message = format!("ff-merged {branch} to main (local)");
        eprintln!("[terminal] {bead_id}: {message}");
        Ok(TerminalResult {
            message,
            pr_url: None,
        })
    } else {
        let stderr = String::from_utf8_lossy(&merge.stderr);
        anyhow::bail!("[terminal] {bead_id}: ff-merge failed: {stderr}");
    }
}

/// Create a PR for a bead's branch using the configured GitHub auth.
async fn create_pr_for_bead(
    repo_path: &Path,
    branch: &str,
    bead_id: &str,
    issue_type: &str,
    base_override: Option<&str>,
) -> Result<String> {
    let github_config = load_github_config_best_effort()?;
    let gh = crate::github::GitHubClient::from_config(&github_config)?;

    let (owner, repo) = parse_github_remote(repo_path)
        .await
        .context("could not determine GitHub owner/repo from git remote")?;

    let base = base_override
        .map(|s| s.to_string())
        .unwrap_or_else(|| github_config.base.clone());

    // Build PR title and body from bead metadata + git diff
    let (title, body) = build_pr_content(repo_path, bead_id, issue_type, &base).await;

    let pr = gh
        .create_pr_from_worktree(repo_path, &owner, &repo, branch, &base, &title, &body)
        .await?;
    Ok(pr.html_url)
}

/// Build a rich PR title and body from bead metadata and git diff.
async fn build_pr_content(
    repo_path: &Path,
    bead_id: &str,
    issue_type: &str,
    base: &str,
) -> (String, String) {
    // Try to load bead title/description from Dolt
    let bead_info = load_bead_info(repo_path, bead_id).await;

    let title = if let Some((ref bead_title, _)) = bead_info {
        format!("[{bead_id}] {issue_type}: {bead_title}")
    } else {
        format!("[{bead_id}] {issue_type}: agent-generated changes")
    };

    // Truncate title to 72 chars (GitHub convention)
    let title = if title.len() > 72 {
        format!("{}...", &title[..69])
    } else {
        title
    };

    let mut body = String::new();

    // Bead description
    if let Some((ref bead_title, ref description)) = bead_info {
        body.push_str(&format!("## {bead_title}\n\n"));
        if !description.is_empty() {
            // Take first ~500 chars of description to avoid huge PR bodies
            let desc = if description.len() > 500 {
                format!("{}...", &description[..497])
            } else {
                description.clone()
            };
            body.push_str(&format!("{desc}\n\n"));
        }
    }

    // Diff stat
    if let Ok(output) = tokio::process::Command::new("git")
        .args(["diff", "--stat", &format!("{base}...HEAD")])
        .current_dir(repo_path)
        .output()
        .await
        && output.status.success()
    {
        let stat = String::from_utf8_lossy(&output.stdout);
        if !stat.trim().is_empty() {
            body.push_str("### Changes\n\n```\n");
            body.push_str(stat.trim());
            body.push_str("\n```\n\n");
        }
    }

    // Handoff chain (if phases ran)
    let handoff_chain = crate::handoff::Handoff::read_chain(repo_path);
    if !handoff_chain.is_empty() {
        body.push_str("### Pipeline Phases\n\n");
        for h in &handoff_chain {
            body.push_str(&format!(
                "**Phase {} — {}** ({})\n",
                h.phase, h.from_agent, h.provider
            ));
            body.push_str(&format!("{}\n", h.summary));
            if let Some(ref v) = h.verdict {
                body.push_str(&format!("Verdict: {}\n", v.decision));
                for c in &v.concerns {
                    body.push_str(&format!("- {c}\n"));
                }
            }
            body.push('\n');
        }
    }

    body.push_str("---\n*Generated by [rosary](https://github.com/agentic-research/rosary)*\n");

    (title, body)
}

/// Load bead title and description from the repo's bead store.
async fn load_bead_info(repo_path: &Path, bead_id: &str) -> Option<(String, String)> {
    let beads_dir = crate::resolve_beads_dir(repo_path);
    let store = crate::bead_sqlite::connect_bead_store(&beads_dir)
        .await
        .ok()?;
    let repo_name = repo_path
        .file_name()
        .map(|n| n.to_string_lossy().to_string())
        .unwrap_or_default();
    let bead = store.get_bead(bead_id, &repo_name).await.ok()??;
    Some((bead.title, bead.description))
}

/// Load GitHub config from ~/.rsry/config.toml, falling back to defaults.
fn load_github_config_best_effort() -> Result<crate::config::GitHubConfig> {
    let home = dirs_next::home_dir().context("no home dir")?;
    let config_path = home.join(".rsry").join("config.toml");
    let content = std::fs::read_to_string(&config_path)
        .with_context(|| format!("reading {}", config_path.display()))?;
    let config: crate::config::Config =
        toml::from_str(&content).with_context(|| format!("parsing {}", config_path.display()))?;
    config
        .github
        .context("[github] section not found in config.toml")
}

/// Rebase the current branch onto the latest remote base.
///
/// Fetches `origin/{base}`, then rebases. If the rebase has conflicts,
/// aborts and returns an error — the bead should be marked blocked.
async fn rebase_onto_latest(repo_path: &Path, bead_id: &str, base: &str) -> Result<()> {
    // Fetch latest base
    let fetch = tokio::process::Command::new("git")
        .args(["fetch", "origin", base])
        .current_dir(repo_path)
        .output()
        .await
        .context("fetching latest base")?;

    if !fetch.status.success() {
        // No remote or fetch failed — skip rebase (local repo)
        return Ok(());
    }

    // Check if we're already up-to-date
    let merge_base = tokio::process::Command::new("git")
        .args(["merge-base", "HEAD", &format!("origin/{base}")])
        .current_dir(repo_path)
        .output()
        .await;

    let remote_head = tokio::process::Command::new("git")
        .args(["rev-parse", &format!("origin/{base}")])
        .current_dir(repo_path)
        .output()
        .await;

    if let (Ok(mb), Ok(rh)) = (&merge_base, &remote_head)
        && mb.status.success()
        && rh.status.success()
    {
        let mb_sha = String::from_utf8_lossy(&mb.stdout).trim().to_string();
        let rh_sha = String::from_utf8_lossy(&rh.stdout).trim().to_string();
        if mb_sha == rh_sha {
            eprintln!("[rebase] {bead_id}: already up-to-date with origin/{base}");
            return Ok(());
        }
    }

    eprintln!("[rebase] {bead_id}: rebasing onto origin/{base}");
    let rebase = tokio::process::Command::new("git")
        .args(["rebase", &format!("origin/{base}")])
        .current_dir(repo_path)
        .output()
        .await
        .context("git rebase")?;

    if rebase.status.success() {
        eprintln!("[rebase] {bead_id}: rebase succeeded");
        return Ok(());
    }

    // Rebase failed — abort and report
    let stderr = String::from_utf8_lossy(&rebase.stderr);
    eprintln!("[rebase] {bead_id}: conflict detected, aborting rebase");
    let _ = tokio::process::Command::new("git")
        .args(["rebase", "--abort"])
        .current_dir(repo_path)
        .output()
        .await;

    anyhow::bail!("rebase conflict onto origin/{base}: {stderr}")
}

/// Parse owner/repo from the `origin` remote URL.
///
/// Handles both SSH (`git@github.com:owner/repo.git`) and HTTPS
/// (`https://github.com/owner/repo.git`) formats.
async fn parse_github_remote(repo_path: &Path) -> Result<(String, String)> {
    let output = tokio::process::Command::new("git")
        .args(["remote", "get-url", "origin"])
        .current_dir(repo_path)
        .output()
        .await
        .context("git remote get-url origin")?;

    if !output.status.success() {
        anyhow::bail!("no origin remote configured");
    }

    let url = String::from_utf8_lossy(&output.stdout).trim().to_string();
    parse_owner_repo(&url).with_context(|| format!("parsing remote URL: {url}"))
}

/// Extract (owner, repo) from a GitHub URL.
pub(super) fn parse_owner_repo(url: &str) -> Result<(String, String)> {
    // SSH: git@github.com:owner/repo.git
    if let Some(path) = url.strip_prefix("git@github.com:") {
        let path = path.trim_end_matches(".git");
        if let Some((owner, repo)) = path.split_once('/') {
            return Ok((owner.to_string(), repo.to_string()));
        }
    }
    // HTTPS: https://github.com/owner/repo.git
    if url.contains("github.com/") {
        let parts: Vec<&str> = url.split("github.com/").collect();
        if parts.len() >= 2 {
            let path = parts[1].trim_end_matches(".git");
            if let Some((owner, repo)) = path.split_once('/') {
                return Ok((owner.to_string(), repo.to_string()));
            }
        }
    }
    anyhow::bail!("unrecognized GitHub remote format")
}

/// Check commit messages for bead references (Golden Rule 11) and auto-amend if missing.
async fn enforce_bead_refs(repo_path: &Path, branch: &str, bead_id: &str) {
    let log_output = tokio::process::Command::new("git")
        .args(["log", "main..HEAD", "--format=%H %s"])
        .current_dir(repo_path)
        .output()
        .await;

    let Ok(log_output) = log_output else { return };
    if !log_output.status.success() {
        return;
    }

    let log = String::from_utf8_lossy(&log_output.stdout);
    let missing_refs: Vec<&str> = log
        .lines()
        .filter(|line| {
            let msg = line.split_once(' ').map(|(_, m)| m).unwrap_or(line);
            !msg.starts_with('[') && !msg.to_lowercase().contains("bead:")
        })
        .collect();

    if missing_refs.is_empty() {
        return;
    }

    eprintln!(
        "[workspace] WARNING: {} commit(s) on {branch} missing bead reference (Golden Rule 11):",
        missing_refs.len()
    );
    for line in &missing_refs {
        eprintln!("[workspace]   {line}");
    }

    let original_msg = log
        .lines()
        .next()
        .unwrap_or("")
        .split_once(' ')
        .map(|(_, msg)| msg)
        .unwrap_or("");
    let amend_msg = format!("[{bead_id}] {original_msg}");
    if let Ok(out) = tokio::process::Command::new("git")
        .args(["commit", "--amend", "-m", &amend_msg])
        .current_dir(repo_path)
        .output()
        .await
        && out.status.success()
    {
        eprintln!("[workspace] auto-amended last commit with [{bead_id}] prefix");
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Set up a bare "origin" + a work clone with one optional fix/* branch.
    ///
    /// Returns (origin_dir, work_dir). The TempDirs must be kept alive for the
    /// duration of the test.
    async fn setup_origin_and_clone(
        origin_dir: &tempfile::TempDir,
        work_dir: &tempfile::TempDir,
        branch: &str,
        merged: bool,
    ) -> (PathBuf, PathBuf) {
        let origin = origin_dir.path().to_path_buf();
        let work = work_dir.path().to_path_buf();

        macro_rules! git_in {
            ($dir:expr, $($arg:expr),+) => {{
                tokio::process::Command::new("git")
                    .args([$($arg),+])
                    .current_dir($dir)
                    .output()
                    .await
                    .unwrap()
            }};
        }

        // Initialise bare origin
        git_in!(&origin, "init", "-q", "--bare", "--initial-branch", "main");

        // Clone into work dir
        git_in!(
            &work,
            "clone",
            "-q",
            origin.to_str().unwrap(),
            work.to_str().unwrap()
        );
        git_in!(&work, "config", "user.email", "test@test.com");
        git_in!(&work, "config", "user.name", "test");

        // Push an initial commit so origin/main exists
        std::fs::write(work.join("file.txt"), "hello").unwrap();
        git_in!(&work, "add", ".");
        git_in!(&work, "commit", "-q", "-m", "init");
        git_in!(&work, "push", "-q", "origin", "main");

        if merged {
            // Branch from main, then push without extra commits — same SHA as main,
            // so it shows up in `git branch -r --merged origin/main`.
            git_in!(&work, "checkout", "-q", "-b", branch);
            git_in!(&work, "push", "-q", "origin", branch);
            git_in!(&work, "checkout", "-q", "main");
        } else {
            // Branch with a new commit so it is NOT merged into main.
            git_in!(&work, "checkout", "-q", "-b", branch);
            std::fs::write(work.join("new.txt"), "change").unwrap();
            git_in!(&work, "add", ".");
            git_in!(&work, "commit", "-q", "-m", "unmerged change");
            git_in!(&work, "push", "-q", "origin", branch);
            git_in!(&work, "checkout", "-q", "main");
        }

        // Fetch so remote-tracking refs are up to date
        git_in!(&work, "fetch", "-q", "origin");

        (origin, work)
    }

    #[tokio::test]
    async fn sweep_skips_when_no_beads_dir() {
        let origin_dir = tempfile::TempDir::new().unwrap();
        let work_dir = tempfile::TempDir::new().unwrap();
        let (_, work) =
            setup_origin_and_clone(&origin_dir, &work_dir, "fix/rsry-abc123", true).await;

        // No .beads/ dir — store is None, bead not found → skipped_active
        let result = sweep_agent_branches(&work, true).await;
        assert_eq!(result.checked, 1);
        assert_eq!(result.deleted, 0);
        assert_eq!(result.skipped_active, 1);
        assert_eq!(result.skipped_unmerged, 0);
    }

    #[tokio::test]
    async fn sweep_skips_unmerged_branch() {
        let origin_dir = tempfile::TempDir::new().unwrap();
        let work_dir = tempfile::TempDir::new().unwrap();
        let (_, work) =
            setup_origin_and_clone(&origin_dir, &work_dir, "fix/rsry-def456", false).await;

        let result = sweep_agent_branches(&work, true).await;
        assert_eq!(result.checked, 1);
        assert_eq!(result.skipped_unmerged, 1);
        assert_eq!(result.deleted, 0);
    }

    #[tokio::test]
    async fn sweep_skips_non_fix_branches() {
        let origin_dir = tempfile::TempDir::new().unwrap();
        let work_dir = tempfile::TempDir::new().unwrap();
        let (_, work) =
            setup_origin_and_clone(&origin_dir, &work_dir, "feature/something", true).await;

        // feature/* is not fix/* — should not be checked at all
        let result = sweep_agent_branches(&work, true).await;
        assert_eq!(result.checked, 0);
    }
}
