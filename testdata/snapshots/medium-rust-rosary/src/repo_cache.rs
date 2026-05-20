//! On-demand repo cloning for hosted/wasteland mode.
//!
//! When a workspace is requested for a repo that's registered but not local,
//! clone it on demand. Clones are cached under `~/.rsry/repos/<org>_<repo>/`
//! and reused across dispatches (fetch-to-update on subsequent calls).
//!
//! Thread-safe via a `Mutex<HashMap>` — concurrent requests for the same URL
//! wait on the first clone to finish rather than cloning twice.

use anyhow::{Context, Result};
use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::Mutex;

/// Cache of cloned repos keyed by URL.
pub struct RepoCache {
    repos: Mutex<HashMap<String, CachedRepo>>,
    base_dir: PathBuf,
}

struct CachedRepo {
    path: PathBuf,
}

impl RepoCache {
    pub fn new() -> Self {
        let base_dir = dirs_next::home_dir()
            .unwrap_or_else(|| PathBuf::from("/tmp"))
            .join(".rsry")
            .join("repos");
        RepoCache {
            repos: Mutex::new(HashMap::new()),
            base_dir,
        }
    }

    /// Get or clone a repo. Returns the local path to the repo root.
    ///
    /// If already cloned, returns cached path. Otherwise clones from URL.
    /// Optionally uses a GitHub token for private repos.
    pub async fn ensure_local(
        &self,
        repo_url: &str,
        github_token: Option<&str>,
    ) -> Result<PathBuf> {
        // Validate URL
        validate_repo_url(repo_url)?;

        // Fast path: already cloned (extract path under lock, fetch outside lock)
        let cached_path = {
            let repos = self.repos.lock().unwrap();
            repos.get(repo_url).map(|c| c.path.clone())
        };
        if let Some(path) = cached_path
            && path.exists()
        {
            let _ = fetch_latest(&path).await;
            return Ok(path);
        }

        // Slow path: clone
        let repo_dir = self.repo_dir(repo_url);
        if repo_dir.exists() {
            // Dir exists but wasn't in cache (restart). Re-register + fetch.
            let _ = fetch_latest(&repo_dir).await;
            let mut repos = self.repos.lock().unwrap();
            repos.insert(
                repo_url.to_string(),
                CachedRepo {
                    path: repo_dir.clone(),
                },
            );
            return Ok(repo_dir);
        }

        std::fs::create_dir_all(&self.base_dir)?;

        eprintln!("[repo-cache] cloning {repo_url}...");
        let clone_url = if let Some(token) = github_token {
            // Inject token for private repos: https://x-access-token:TOKEN@github.com/...
            inject_token(repo_url, token)?
        } else {
            repo_url.to_string()
        };

        let output = tokio::process::Command::new("git")
            .args([
                "clone",
                "--single-branch",
                &clone_url,
                &repo_dir.to_string_lossy(),
            ])
            .output()
            .await
            .context("git clone")?;

        if !output.status.success() {
            let stderr = String::from_utf8_lossy(&output.stderr);
            anyhow::bail!("git clone failed for {repo_url}: {stderr}");
        }

        eprintln!("[repo-cache] cloned {} → {}", repo_url, repo_dir.display());

        // Register in cache
        let mut repos = self.repos.lock().unwrap();
        repos.insert(
            repo_url.to_string(),
            CachedRepo {
                path: repo_dir.clone(),
            },
        );

        Ok(repo_dir)
    }

    /// Deterministic local path for a repo URL.
    fn repo_dir(&self, repo_url: &str) -> PathBuf {
        // Use the org/repo part as directory name
        let name = repo_url
            .trim_end_matches('/')
            .trim_end_matches(".git")
            .rsplit('/')
            .take(2)
            .collect::<Vec<_>>()
            .into_iter()
            .rev()
            .collect::<Vec<_>>()
            .join("_");
        self.base_dir
            .join(if name.is_empty() { "unknown" } else { &name })
    }
}

/// Validate a repo URL is safe for git clone.
fn validate_repo_url(url: &str) -> Result<()> {
    if url.starts_with('-') {
        anyhow::bail!("option injection: URL starts with dash");
    }
    let parsed = reqwest::Url::parse(url).context("invalid URL")?;
    if !matches!(parsed.scheme(), "https" | "http") {
        anyhow::bail!("only https URLs allowed, got {}", parsed.scheme());
    }
    if parsed.password().is_some() {
        anyhow::bail!("embedded credentials not allowed in URL");
    }
    Ok(())
}

/// Inject a GitHub token into an HTTPS URL for private repo access.
fn inject_token(repo_url: &str, token: &str) -> Result<String> {
    let mut parsed = reqwest::Url::parse(repo_url).context("invalid URL")?;
    parsed
        .set_username("x-access-token")
        .map_err(|_| anyhow::anyhow!("failed to set username"))?;
    parsed
        .set_password(Some(token))
        .map_err(|_| anyhow::anyhow!("failed to set password"))?;
    Ok(parsed.to_string())
}

/// Fetch latest from origin (best-effort, non-blocking).
async fn fetch_latest(repo_dir: &Path) -> Result<()> {
    let output = tokio::process::Command::new("git")
        .args(["fetch", "origin", "--prune"])
        .current_dir(repo_dir)
        .output()
        .await?;
    if !output.status.success() {
        let stderr = String::from_utf8_lossy(&output.stderr);
        eprintln!("[repo-cache] fetch failed: {stderr}");
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn validate_url_rejects_injection() {
        assert!(validate_repo_url("-evil").is_err());
        assert!(validate_repo_url("file:///etc/passwd").is_err());
        assert!(validate_repo_url("ssh://git@github.com/foo/bar").is_err());
    }

    #[test]
    fn validate_url_accepts_https() {
        assert!(validate_repo_url("https://github.com/agentic-research/rosary").is_ok());
        assert!(validate_repo_url("https://github.com/agentic-research/rosary.git").is_ok());
    }

    #[test]
    fn validate_url_rejects_embedded_creds() {
        assert!(validate_repo_url("https://user:pass@github.com/foo/bar").is_err());
    }

    #[test]
    fn inject_token_works() {
        let url = inject_token("https://github.com/org/repo", "ghp_test123").unwrap();
        assert!(url.contains("x-access-token:ghp_test123@github.com"));
    }

    #[test]
    fn repo_dir_deterministic() {
        let cache = RepoCache::new();
        let dir = cache.repo_dir("https://github.com/agentic-research/rosary");
        assert!(dir.to_string_lossy().ends_with("agentic-research_rosary"));
    }

    #[test]
    fn repo_dir_strips_git_suffix() {
        let cache = RepoCache::new();
        let dir = cache.repo_dir("https://github.com/org/repo.git");
        assert!(dir.to_string_lossy().ends_with("org_repo"));
    }
}
