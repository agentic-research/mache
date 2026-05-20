//! Feature-scoped workspace — jj-native isolation + pluggable compute.
//!
//! A `Workspace` manages the full lifecycle of agent work:
//!   1. Isolate: create a jj workspace (or git worktree fallback)
//!   2. Provision: set up compute environment (local or remote)
//!   3. Execute: run agent commands
//!   4. Checkpoint: snapshot state (jj commit + optional provider checkpoint)
//!   5. Teardown: destroy compute + clean up workspace
//!
//! Workspaces can be **bead-scoped** (one workspace per bead, current model)
//! or **feature-scoped** (one workspace for many beads, rollup model).

mod checkpoint;
mod lifecycle;
mod sweep;

#[cfg(test)]
mod tests;

use std::path::{Path, PathBuf};

use crate::backend::ExecHandle;

// Re-export public API from submodules.
pub(crate) use sweep::{cleanup_git_worktree, cleanup_jj_workspace, workspace_dir};
pub use sweep::{
    ensure_thread_branch, merge_or_pr_with_base, sweep_agent_branches, sweep_orphaned,
    thread_branch_name,
};

/// VCS backend for code isolation.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum VcsKind {
    /// jj (preferred — zero-cost branching, first-class isolation).
    Jj,
    /// git worktree (fallback when jj unavailable).
    Git,
    /// No VCS isolation — run in-place. Only for single-concurrency.
    None,
}

/// Detect which VCS to use for workspace isolation.
///
/// Colocated repos (both .jj and .git): use git worktree. Agents use
/// `git add/commit` which needs a proper `.git` file that only
/// `git worktree add` provides. jj sees git commits via colocation.
/// The orchestrator handles jj checkpoint/bookmark separately.
pub fn detect_vcs(repo_path: &Path) -> VcsKind {
    let has_jj = repo_path.join(".jj").exists();
    let has_git = repo_path.join(".git").exists();
    match (has_jj, has_git) {
        (_, true) => VcsKind::Git,    // git worktree (jj tracks via colocation)
        (true, false) => VcsKind::Jj, // pure jj
        _ => VcsKind::None,
    }
}

/// A workspace manages isolated agent execution for a bead or feature.
pub struct Workspace {
    /// Bead or feature ID this workspace serves.
    pub id: String,
    /// Repo this workspace operates on.
    pub repo: String,
    /// The repo root path (original, not the workspace copy).
    pub repo_path: PathBuf,
    /// Working directory for this workspace (may differ from repo_path).
    pub work_dir: PathBuf,
    /// VCS used for isolation.
    pub vcs: VcsKind,
    /// Compute provider handle (None until provisioned).
    pub(crate) exec_handle: Option<ExecHandle>,
}

impl Workspace {
    /// Tear down the workspace — removes the worktree or jj workspace.
    ///
    /// Called when spawn fails after workspace creation, ensuring no orphaned
    /// worktrees are left on disk. For `VcsKind::None` (in-place), this is a no-op.
    pub fn cleanup(self) {
        match self.vcs {
            VcsKind::Git => cleanup_git_worktree(&self.repo_path, &self.id),
            VcsKind::Jj => cleanup_jj_workspace(&self.repo_path, &self.id),
            VcsKind::None => {}
        }
    }

    /// Reconstruct a workspace from existing on-disk state.
    ///
    /// Used by MCP tools that need to operate on a workspace created by
    /// a previous call. Does not create anything — just rebuilds the struct.
    pub fn from_existing(id: &str, repo: &str, repo_path: &Path) -> Self {
        let vcs = detect_vcs(repo_path);
        let ws_dir = workspace_dir(repo_path, id);
        let (work_dir, vcs) = if ws_dir.exists() {
            (ws_dir, vcs)
        } else {
            (repo_path.to_path_buf(), VcsKind::None)
        };
        Workspace {
            id: id.to_string(),
            repo: repo.to_string(),
            repo_path: repo_path.to_path_buf(),
            work_dir,
            vcs,
            exec_handle: None,
        }
    }
}
