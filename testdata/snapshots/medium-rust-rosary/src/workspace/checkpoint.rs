//! Workspace checkpoint: commit and snapshot agent work.

use anyhow::{Context, Result};

use super::{VcsKind, Workspace};

impl Workspace {
    /// Create a jj commit (code checkpoint) in the workspace.
    pub async fn jj_commit(&self, message: &str) -> Result<()> {
        if self.vcs != VcsKind::Jj {
            return Ok(());
        }
        let output = tokio::process::Command::new("jj")
            .args(["commit", "-m", message])
            .current_dir(&self.work_dir)
            .output()
            .await
            .context("jj commit")?;

        if !output.status.success() {
            let stderr = String::from_utf8_lossy(&output.stderr);
            anyhow::bail!("jj commit failed: {stderr}");
        }
        Ok(())
    }

    /// Create a jj bookmark for this workspace's work.
    pub async fn jj_bookmark(&self, name: &str) -> Result<()> {
        if self.vcs != VcsKind::Jj {
            return Ok(());
        }
        let output = tokio::process::Command::new("jj")
            .args(["bookmark", "create", name])
            .current_dir(&self.work_dir)
            .output()
            .await
            .context("jj bookmark create")?;

        if !output.status.success() {
            // Bookmark may already exist — try move instead
            let output = tokio::process::Command::new("jj")
                .args(["bookmark", "move", name])
                .current_dir(&self.work_dir)
                .output()
                .await
                .context("jj bookmark move")?;

            if !output.status.success() {
                let stderr = String::from_utf8_lossy(&output.stderr);
                anyhow::bail!("jj bookmark failed: {stderr}");
            }
        }
        Ok(())
    }

    /// Checkpoint the workspace: commit all changes, return commit/change ID.
    ///
    /// Call this BEFORE cleanup to preserve the agent's work.
    /// - Git worktree: `git add -A && git commit`
    /// - jj workspace: `jj commit` + bookmark
    ///   The orchestrator calls this — agents don't commit themselves.
    pub async fn checkpoint(&self, message: &str) -> Result<Option<String>> {
        match self.vcs {
            VcsKind::Git => self.git_checkpoint(message).await,
            VcsKind::Jj => {
                self.jj_commit(message).await?;
                let change_id = self.jj_change_id().await?;
                let bookmark = format!("fix/{}", self.id);
                self.jj_bookmark(&bookmark).await?;
                Ok(change_id)
            }
            VcsKind::None => Ok(None),
        }
    }

    /// Git checkpoint: stage all changes and commit. Returns short SHA.
    async fn git_checkpoint(&self, message: &str) -> Result<Option<String>> {
        // Check if there are any changes to commit
        let status = tokio::process::Command::new("git")
            .args(["status", "--porcelain"])
            .current_dir(&self.work_dir)
            .output()
            .await
            .context("git status")?;

        if status.stdout.is_empty() {
            return Ok(None); // Nothing to commit
        }

        // Stage all changes
        let add = tokio::process::Command::new("git")
            .args(["add", "-A"])
            .current_dir(&self.work_dir)
            .output()
            .await
            .context("git add")?;

        if !add.status.success() {
            let stderr = String::from_utf8_lossy(&add.stderr);
            anyhow::bail!("git add failed: {stderr}");
        }

        // Commit
        let commit = tokio::process::Command::new("git")
            .args(["commit", "-m", message])
            .current_dir(&self.work_dir)
            .output()
            .await
            .context("git commit")?;

        if !commit.status.success() {
            let stderr = String::from_utf8_lossy(&commit.stderr);
            anyhow::bail!("git commit failed: {stderr}");
        }

        // Get short SHA
        let rev = tokio::process::Command::new("git")
            .args(["rev-parse", "--short", "HEAD"])
            .current_dir(&self.work_dir)
            .output()
            .await
            .context("git rev-parse")?;

        let sha = String::from_utf8_lossy(&rev.stdout).trim().to_string();
        Ok(if sha.is_empty() { None } else { Some(sha) })
    }

    /// Get the jj change ID of the most recent commit (@-).
    ///
    /// After `jj commit`, the new commit is @- (jj advances the working copy).
    async fn jj_change_id(&self) -> Result<Option<String>> {
        if self.vcs != VcsKind::Jj {
            return Ok(None);
        }
        let output = tokio::process::Command::new("jj")
            .args(["log", "-r", "@-", "--no-graph", "-T", "change_id"])
            .current_dir(&self.work_dir)
            .output()
            .await
            .context("jj log change_id")?;

        if !output.status.success() {
            return Ok(None);
        }
        let id = String::from_utf8_lossy(&output.stdout).trim().to_string();
        if id.is_empty() {
            Ok(None)
        } else {
            Ok(Some(id))
        }
    }
}
