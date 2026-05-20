//! Mirror bead context as structured comments on linked GitHub PRs/issues.
//!
//! `sync_beads_to_github` scans beads for `pr_url`, then calls
//! `GitHubMirror::post_bead_context` to post a comment containing the bead's
//! decade / thread / success criteria / deps.

use crate::bead::Bead;
use anyhow::{Context, Result};
use serde::Serialize;

const COMMENT_SENTINEL: &str = "<!-- rosary-bead-context -->";
const GITHUB_API_VERSION: &str = "2022-11-28";

pub struct GitHubMirror {
    client: reqwest::Client,
    token: String,
    api_base: String,
}

#[derive(Serialize)]
struct CreateCommentRequest {
    body: String,
}

impl GitHubMirror {
    pub fn new(token: &str) -> Self {
        Self {
            client: reqwest::Client::new(),
            token: token.to_string(),
            api_base: "https://api.github.com".to_string(),
        }
    }

    #[cfg(test)]
    pub fn with_base_url(token: &str, api_base: &str) -> Self {
        Self {
            client: reqwest::Client::new(),
            token: token.to_string(),
            api_base: api_base.to_string(),
        }
    }

    /// Post a bead-context comment on the GitHub PR or issue identified by `pr_url`.
    pub async fn post_bead_context(&self, pr_url: &str, bead: &Bead) -> Result<()> {
        let (owner, repo, pr_number) =
            parse_pr_url(pr_url).with_context(|| format!("parsing pr_url '{pr_url}'"))?;
        let url = format!(
            "{}/repos/{owner}/{repo}/issues/{pr_number}/comments",
            self.api_base
        );
        let body = format_bead_comment(bead);
        let req = CreateCommentRequest { body };
        let resp = self
            .client
            .post(&url)
            .header("Authorization", format!("Bearer {}", self.token))
            .header("Accept", "application/vnd.github+json")
            .header("X-GitHub-Api-Version", GITHUB_API_VERSION)
            .header("User-Agent", "rosary-stringer[bot]")
            .json(&req)
            .send()
            .await
            .context("GitHub API request")?;

        if !resp.status().is_success() {
            let status = resp.status();
            let text = resp.text().await.unwrap_or_default();
            anyhow::bail!("GitHub comment failed ({status}): {text}");
        }
        eprintln!(
            "[github_mirror] posted context for {} on {owner}/{repo}#{pr_number}",
            bead.id
        );
        Ok(())
    }
}

/// Build the markdown comment body for a bead.
pub fn format_bead_comment(bead: &Bead) -> String {
    let mut out = format!("{COMMENT_SENTINEL}\n## 🔗 Bead Context: {}\n\n", bead.id);
    out.push_str(&format!("**Title:** {}\n", bead.title));
    out.push_str(&format!("**Status:** {}\n", bead.status));
    out.push_str(&format!("**Priority:** P{}\n", bead.priority));
    out.push_str(&format!("**Type:** {}\n", bead.issue_type));

    if !bead.description.is_empty() {
        out.push_str("\n### Description\n\n");
        // Emit only the first 500 chars to keep comments readable.
        let desc = if bead.description.len() > 500 {
            format!("{}…", &bead.description[..500])
        } else {
            bead.description.clone()
        };
        out.push_str(&desc);
        out.push('\n');
    }

    if !bead.files.is_empty() {
        out.push_str("\n### File Scopes\n\n");
        for f in &bead.files {
            out.push_str(&format!("- `{f}`\n"));
        }
    }

    out.push_str("\n---\n*Posted by [rosary](https://github.com/agentic-research/rosary)*\n");
    out
}

/// Parse `https://github.com/<owner>/<repo>/pull/<n>` → (owner, repo, n).
/// Also accepts `/issues/<n>`.
pub fn parse_pr_url(url: &str) -> Result<(String, String, u64)> {
    // Strip scheme + host
    let path = url
        .strip_prefix("https://github.com/")
        .or_else(|| url.strip_prefix("http://github.com/"))
        .with_context(|| format!("not a github.com URL: {url}"))?;

    // path: "owner/repo/pull/42" or "owner/repo/issues/42"
    let parts: Vec<&str> = path.split('/').collect();
    if parts.len() < 4 || (parts[2] != "pull" && parts[2] != "issues") {
        anyhow::bail!("expected github.com/<owner>/<repo>/pull/<n> or /issues/<n>, got: {url}");
    }
    let owner = parts[0].to_string();
    let repo = parts[1].to_string();
    let number: u64 = parts[3]
        .parse()
        .with_context(|| format!("PR number '{}' is not a u64", parts[3]))?;
    Ok((owner, repo, number))
}

/// Post bead-context comments for every bead in `beads` that has a `pr_url`.
/// Returns the number of comments successfully posted.
pub async fn sync_beads_to_github(beads: &[Bead], token: &str) -> Result<u32> {
    let mirror = GitHubMirror::new(token);
    let mut posted = 0u32;
    for bead in beads {
        if let Some(ref pr_url) = bead.pr_url {
            match mirror.post_bead_context(pr_url, bead).await {
                Ok(()) => posted += 1,
                Err(e) => eprintln!("[github_mirror] skipping {}: {e}", bead.id),
            }
        }
    }
    Ok(posted)
}

// ── Tests ─────────────────────────────────────────────────────────────────────

#[cfg(test)]
mod github_mirror {
    use super::*;
    use axum::{Json, Router, extract::Path, routing::post};
    use std::sync::{Arc, Mutex};

    fn make_bead(id: &str, pr_url: Option<&str>) -> Bead {
        Bead {
            id: id.to_string(),
            title: "Test bead".to_string(),
            description: "A test bead description.".to_string(),
            status: "open".to_string(),
            priority: 2,
            issue_type: "task".to_string(),
            owner: None,
            repo: "rosary".to_string(),
            created_at: chrono::Utc::now(),
            updated_at: chrono::Utc::now(),
            dependency_count: 0,
            dependent_count: 0,
            comment_count: 0,
            branch: None,
            pr_url: pr_url.map(str::to_string),
            jj_change_id: None,
            external_ref: None,
            files: vec![],
            test_files: vec![],
            created_by: None,
            scope: String::new(),
            derived_from: vec![],
        }
    }

    #[test]
    fn parse_pr_url_pull() {
        let (owner, repo, n) =
            parse_pr_url("https://github.com/agentic-research/rosary/pull/42").unwrap();
        assert_eq!(owner, "agentic-research");
        assert_eq!(repo, "rosary");
        assert_eq!(n, 42);
    }

    #[test]
    fn parse_pr_url_issue() {
        let (owner, repo, n) = parse_pr_url("https://github.com/org/repo/issues/7").unwrap();
        assert_eq!(owner, "org");
        assert_eq!(repo, "repo");
        assert_eq!(n, 7);
    }

    #[test]
    fn parse_pr_url_invalid_host() {
        assert!(parse_pr_url("https://gitlab.com/org/repo/pull/1").is_err());
    }

    #[test]
    fn parse_pr_url_missing_number() {
        assert!(parse_pr_url("https://github.com/org/repo/pull/abc").is_err());
    }

    #[test]
    fn format_comment_includes_sentinel_and_id() {
        let bead = make_bead("rosary-5414da", None);
        let comment = format_bead_comment(&bead);
        assert!(comment.contains(COMMENT_SENTINEL));
        assert!(comment.contains("rosary-5414da"));
        assert!(comment.contains("Test bead"));
    }

    #[test]
    fn format_comment_truncates_long_description() {
        let mut bead = make_bead("rosary-abc", None);
        bead.description = "x".repeat(600);
        let comment = format_bead_comment(&bead);
        assert!(comment.contains('…'));
        // The truncated slice is 500 chars of 'x' + '…'
        let desc_start = comment.find("### Description").unwrap();
        let after = &comment[desc_start..];
        assert!(after.len() < 600);
    }

    #[tokio::test]
    async fn posts_comment_on_linked_pr() {
        // Spin up an in-process mock server using axum.
        let received: Arc<Mutex<Vec<String>>> = Arc::new(Mutex::new(vec![]));
        let received2 = received.clone();

        let app = Router::new().route(
            "/repos/{owner}/{repo}/issues/{number}/comments",
            post(
                move |Path((_owner, _repo, _number)): Path<(String, String, u64)>,
                      Json(body): Json<serde_json::Value>| {
                    let received = received2.clone();
                    async move {
                        received
                            .lock()
                            .unwrap()
                            .push(body["body"].as_str().unwrap_or("").to_string());
                        Json(serde_json::json!({"id": 1, "body": "ok"}))
                    }
                },
            ),
        );

        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = listener.local_addr().unwrap().port();
        tokio::spawn(async move {
            axum::serve(listener, app).await.unwrap();
        });

        let mirror = GitHubMirror::with_base_url("test-token", &format!("http://127.0.0.1:{port}"));
        let bead = make_bead("rosary-5414da", None);
        mirror
            .post_bead_context("https://github.com/agentic-research/rosary/pull/160", &bead)
            .await
            .unwrap();

        let captured = received.lock().unwrap();
        assert_eq!(captured.len(), 1);
        assert!(captured[0].contains(COMMENT_SENTINEL));
        assert!(captured[0].contains("rosary-5414da"));
    }

    #[tokio::test]
    async fn sync_beads_skips_without_pr_url() {
        // No PR URLs → no HTTP calls → count = 0.
        let beads = vec![make_bead("rosary-abc", None)];
        // Using a real token to a non-existent host would fail if called.
        let count = sync_beads_to_github(&beads, "tok").await.unwrap();
        assert_eq!(count, 0);
    }
}
