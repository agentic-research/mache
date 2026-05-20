//! Workspace lifecycle: create, provision, exec, teardown.

use anyhow::Result;
use std::path::Path;

use crate::backend::{ComputeProvider, ExecHandle, ExecResult, ProvisionOpts};

use super::sweep::{
    cleanup_git_worktree, cleanup_jj_workspace, create_git_worktree, create_jj_workspace,
    workspace_dir,
};
use super::{VcsKind, Workspace};

impl Workspace {
    /// Create a new workspace with code isolation.
    ///
    /// Tries jj first, falls back to git worktree, then in-place.
    pub async fn create(id: &str, repo: &str, repo_path: &Path, isolate: bool) -> Result<Self> {
        // Expand tilde but do NOT canonicalize — that resolves symlinks
        // and breaks paths like ~/github → ~/remotes.
        let repo_path = crate::scanner::expand_path(repo_path);
        let repo_path = repo_path.as_path();

        let vcs = if isolate {
            super::detect_vcs(repo_path)
        } else {
            VcsKind::None
        };

        // Reuse existing workspace if it exists (resume after agent death)
        let existing_ws = workspace_dir(repo_path, id);
        if existing_ws.exists() && vcs != VcsKind::None {
            eprintln!(
                "[workspace] reusing existing workspace: {}",
                existing_ws.display()
            );
            return Ok(Workspace {
                id: id.to_string(),
                repo: repo.to_string(),
                repo_path: repo_path.to_path_buf(),
                work_dir: existing_ws,
                vcs,
                exec_handle: None,
            });
        }

        let (work_dir, actual_vcs) = match vcs {
            VcsKind::Jj => match create_jj_workspace(repo_path, id).await {
                Ok(path) => (path, VcsKind::Jj),
                Err(e) if isolate => {
                    // Isolation was requested — don't silently run in-place.
                    // This prevents agents from writing to the main repo.
                    anyhow::bail!(
                        "workspace isolation failed for {id}: jj workspace creation failed: {e}"
                    );
                }
                Err(e) => {
                    eprintln!("[workspace] jj isolation failed ({e}), falling back to in-place");
                    (repo_path.to_path_buf(), VcsKind::None)
                }
            },
            VcsKind::Git => match create_git_worktree(repo_path, id).await {
                Ok(path) => (path, VcsKind::Git),
                Err(e) if isolate => {
                    // Isolation was requested — don't silently run in-place.
                    // This prevents agents from writing to the main repo.
                    anyhow::bail!(
                        "workspace isolation failed for {id}: git worktree creation failed: {e}"
                    );
                }
                Err(e) => {
                    eprintln!("[workspace] git worktree failed ({e}), falling back to in-place");
                    (repo_path.to_path_buf(), VcsKind::None)
                }
            },
            VcsKind::None if isolate => {
                anyhow::bail!(
                    "workspace isolation failed for {id}: no VCS found in {} \
                     (need .git or .jj for isolation)",
                    repo_path.display()
                );
            }
            VcsKind::None => (repo_path.to_path_buf(), VcsKind::None),
        };

        Ok(Workspace {
            id: id.to_string(),
            repo: repo.to_string(),
            repo_path: repo_path.to_path_buf(),
            work_dir,
            vcs: actual_vcs,
            exec_handle: None,
        })
    }

    /// Provision compute for this workspace.
    pub async fn provision(&mut self, provider: &dyn ComputeProvider) -> Result<()> {
        let opts = ProvisionOpts::new(&self.id, &self.repo);
        let handle = provider.provision(&opts).await?;
        self.exec_handle = Some(handle);
        Ok(())
    }

    /// Execute a command in this workspace's compute environment.
    ///
    /// If a compute provider is provisioned, runs there.
    /// Otherwise runs locally in the workspace directory.
    pub async fn exec(&self, provider: &dyn ComputeProvider, cmd: &[&str]) -> Result<ExecResult> {
        if let Some(ref handle) = self.exec_handle {
            provider.exec(handle, cmd).await
        } else {
            // No provisioned handle — run locally
            crate::backend::LocalProvider
                .exec(
                    &ExecHandle {
                        id: format!("local-{}", self.id),
                        backend: "local".into(),
                    },
                    cmd,
                )
                .await
        }
    }

    /// Tear down the workspace: destroy compute + clean up VCS isolation.
    pub async fn teardown(self, provider: &dyn ComputeProvider) -> Result<()> {
        // Destroy compute
        if let Some(ref handle) = self.exec_handle
            && let Err(e) = provider.destroy(handle).await
        {
            eprintln!("[workspace] compute destroy failed: {e}");
        }

        // Clean up VCS isolation
        match self.vcs {
            VcsKind::Jj => {
                cleanup_jj_workspace(&self.repo_path, &self.id);
            }
            VcsKind::Git => {
                cleanup_git_worktree(&self.repo_path, &self.id);
            }
            VcsKind::None => {}
        }

        Ok(())
    }
}
