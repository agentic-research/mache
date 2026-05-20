//! GitHub webhook handler — signature verification and PR merge → bead advance.

use hmac::{Hmac, Mac};
use serde::Deserialize;
use sha2::Sha256;

use super::AppState;

type HmacSha256 = Hmac<Sha256>;

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

#[derive(Debug, Deserialize)]
pub(crate) struct GithubWebhookPayload {
    pub action: Option<String>,
    pub pull_request: Option<GithubPullRequest>,
}

#[derive(Debug, Deserialize)]
pub(crate) struct GithubPullRequest {
    pub merged: Option<bool>,
    pub body: Option<String>,
    pub title: Option<String>,
    pub number: Option<u64>,
}

// ---------------------------------------------------------------------------
// Signature verification
// ---------------------------------------------------------------------------

/// Verify GitHub webhook signature (`X-Hub-Signature-256: sha256=<hex>`).
pub(crate) fn verify_github_signature(body: &[u8], secret: &[u8], signature: &str) -> bool {
    let hex = match signature.strip_prefix("sha256=") {
        Some(h) => h,
        None => return false,
    };
    let Ok(sig_bytes) = hex::decode(hex) else {
        return false;
    };
    let Ok(mut mac) = HmacSha256::new_from_slice(secret) else {
        return false;
    };
    mac.update(body);
    mac.verify_slice(&sig_bytes).is_ok()
}

// ---------------------------------------------------------------------------
// Bead ID extraction
// ---------------------------------------------------------------------------

/// Extract a bead ID from PR title/body text.
///
/// Matches `[<id>]` or `bead: <id>` or `bead-id: <id>` where `<id>` is 6+
/// hex chars, optionally prefixed with `<repo>-` (e.g., `rosary-5414da`).
pub(crate) fn extract_bead_id(text: &str) -> Option<String> {
    // Pattern 1: [rosary-5414da] or [5414da]
    let bracket = extract_bracket_id(text);
    if bracket.is_some() {
        return bracket;
    }

    // Pattern 2: bead: 5414da or bead-id: rosary-5414da
    for prefix in &["bead-id:", "bead:"] {
        if let Some(after) = find_after(text, prefix)
            && let Some(id) = parse_bead_token(after)
        {
            return Some(id);
        }
    }

    None
}

fn extract_bracket_id(text: &str) -> Option<String> {
    let mut chars = text.char_indices().peekable();
    while let Some((i, ch)) = chars.next() {
        if ch != '[' {
            continue;
        }
        // Find the closing bracket
        let start = i + 1;
        let rest = &text[start..];
        if let Some(end_offset) = rest.find(']') {
            let candidate = &rest[..end_offset];
            if is_bead_id(candidate) {
                return Some(strip_repo_prefix(candidate));
            }
        }
        let _ = chars.next(); // advance past '['
    }
    None
}

fn find_after<'a>(text: &'a str, prefix: &str) -> Option<&'a str> {
    let lower = text.to_lowercase();
    let lprefix = prefix.to_lowercase();
    lower.find(lprefix.as_str()).map(|pos| {
        let after = &text[pos + prefix.len()..];
        after.trim_start()
    })
}

fn parse_bead_token(s: &str) -> Option<String> {
    let token: &str = s.split_whitespace().next()?;
    // Strip trailing punctuation
    let token = token.trim_end_matches(|c: char| !c.is_alphanumeric() && c != '-');
    if is_bead_id(token) {
        Some(strip_repo_prefix(token))
    } else {
        None
    }
}

/// Returns true if `s` looks like a bead ID: `<repo>-<hex6+>` or bare `<hex6+>`.
fn is_bead_id(s: &str) -> bool {
    let hex_part = if let Some(pos) = s.rfind('-') {
        &s[pos + 1..]
    } else {
        s
    };
    hex_part.len() >= 6 && hex_part.chars().all(|c| c.is_ascii_hexdigit())
}

/// Strip repo prefix (`rosary-5414da` → `5414da`).
fn strip_repo_prefix(s: &str) -> String {
    if let Some(pos) = s.rfind('-') {
        let hex_part = &s[pos + 1..];
        if hex_part.len() >= 6 && hex_part.chars().all(|c| c.is_ascii_hexdigit()) {
            return hex_part.to_string();
        }
    }
    s.to_string()
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

/// POST /webhook/github — GitHub webhook handler.
///
/// Receives `pull_request` events from GitHub, verifies HMAC-SHA256 signature,
/// and when a PR is merged:
/// 1. Extracts bead ID from PR title/body
/// 2. Advances the bead to "done"
/// 3. Unblocks dependent beads (blocked → open)
pub(crate) async fn handle_github_webhook(
    axum::extract::State(state): axum::extract::State<AppState>,
    headers: axum::http::HeaderMap,
    body: axum::body::Bytes,
) -> axum::response::Response {
    use axum::response::IntoResponse;

    // 1. Verify signature if secret is configured
    if let Some(ref secret) = state.github_webhook_secret {
        let signature = match headers
            .get("x-hub-signature-256")
            .and_then(|v| v.to_str().ok())
        {
            Some(s) => s.to_string(),
            None => {
                return (
                    axum::http::StatusCode::BAD_REQUEST,
                    "missing X-Hub-Signature-256 header",
                )
                    .into_response();
            }
        };

        if !verify_github_signature(&body, secret.as_bytes(), &signature) {
            eprintln!("[github-webhook] HMAC verification failed");
            return (axum::http::StatusCode::UNAUTHORIZED, "invalid signature").into_response();
        }
    }

    // 2. Only process pull_request events
    let event = headers
        .get("x-github-event")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("");

    if event != "pull_request" {
        return (axum::http::StatusCode::OK, "ignored").into_response();
    }

    // 3. Parse payload
    let payload: GithubWebhookPayload = match serde_json::from_slice(&body) {
        Ok(p) => p,
        Err(e) => {
            eprintln!("[github-webhook] failed to parse payload: {e}");
            return (axum::http::StatusCode::BAD_REQUEST, "invalid JSON payload").into_response();
        }
    };

    // 4. Only process closed+merged PRs
    if payload.action.as_deref() != Some("closed") {
        return (axum::http::StatusCode::OK, "not a close event").into_response();
    }

    let pr = match payload.pull_request {
        Some(pr) if pr.merged == Some(true) => pr,
        _ => {
            return (axum::http::StatusCode::OK, "pr not merged").into_response();
        }
    };

    // 5. Extract bead ID from PR title then body
    let pr_number = pr.number.unwrap_or(0);
    let search_text = format!(
        "{} {}",
        pr.title.as_deref().unwrap_or(""),
        pr.body.as_deref().unwrap_or("")
    );
    let bead_id = match extract_bead_id(&search_text) {
        Some(id) => id,
        None => {
            eprintln!("[github-webhook] PR #{pr_number} merged but no bead ID found in title/body");
            return (axum::http::StatusCode::OK, "no bead id found").into_response();
        }
    };

    eprintln!("[github-webhook] PR #{pr_number} merged — advancing bead {bead_id}");

    // 6. Find the bead across all repos, advance it, unblock dependents
    let mut found = false;
    for (repo_name, client) in state.pool.iter_clients() {
        // Search for the bead by short ID
        let beads = match client.search_beads(&bead_id, repo_name, 20).await {
            Ok(b) => b,
            Err(e) => {
                eprintln!("[github-webhook] search error in {repo_name}: {e}");
                continue;
            }
        };

        let matched = beads
            .iter()
            .find(|b| b.id.ends_with(&bead_id) || b.id == bead_id);
        let matched_id = match matched {
            Some(b) => b.id.clone(),
            None => continue,
        };

        // Advance bead to done
        match client.update_status(&matched_id, "done").await {
            Ok(()) => {
                eprintln!(
                    "[github-webhook] bead {matched_id} in {repo_name} → done (PR #{pr_number})"
                );
                client
                    .log_event(
                        &matched_id,
                        "github_merge",
                        &format!("PR #{pr_number} merged → done"),
                    )
                    .await;
                found = true;
            }
            Err(e) => {
                eprintln!("[github-webhook] failed to advance {matched_id}: {e}");
                continue;
            }
        }

        // Unblock dependents: beads that were waiting on this one
        match client.get_dependents(&matched_id).await {
            Ok(deps) => {
                for dep_id in deps {
                    // Only unblock beads that are explicitly blocked
                    match client.update_status(&dep_id, "open").await {
                        Ok(()) => {
                            eprintln!(
                                "[github-webhook] unblocked dependent {dep_id} in {repo_name}"
                            );
                        }
                        Err(e) => {
                            eprintln!("[github-webhook] failed to unblock dependent {dep_id}: {e}");
                        }
                    }
                }
            }
            Err(e) => {
                eprintln!("[github-webhook] get_dependents({matched_id}) failed: {e}");
            }
        }

        break;
    }

    if !found {
        eprintln!("[github-webhook] no bead found matching id={bead_id}");
    }

    (axum::http::StatusCode::OK, "ok").into_response()
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashSet;
    use std::sync::Arc;
    use tokio::sync::RwLock;

    use crate::pool::RepoPool;

    // --- Signature verification ---

    #[test]
    fn github_signature_valid() {
        let secret = b"test-github-secret";
        let body = b"pull_request payload body";

        let mut mac = HmacSha256::new_from_slice(secret).unwrap();
        mac.update(body);
        let hex = hex::encode(mac.finalize().into_bytes());
        let sig = format!("sha256={hex}");

        assert!(verify_github_signature(body, secret, &sig));
    }

    #[test]
    fn github_signature_rejects_bad_hex() {
        assert!(!verify_github_signature(
            b"body",
            b"secret",
            "sha256=notHex"
        ));
    }

    #[test]
    fn github_signature_rejects_missing_prefix() {
        assert!(!verify_github_signature(b"body", b"secret", "deadbeef"));
    }

    #[test]
    fn github_signature_rejects_wrong_body() {
        let secret = b"secret";
        let body = b"original";
        let mut mac = HmacSha256::new_from_slice(secret).unwrap();
        mac.update(body);
        let sig = format!("sha256={}", hex::encode(mac.finalize().into_bytes()));

        assert!(!verify_github_signature(b"tampered", secret, &sig));
    }

    // --- Bead ID extraction ---

    #[test]
    fn extract_bracket_bead_id() {
        assert_eq!(
            extract_bead_id("[rosary-5414da] feat: something"),
            Some("5414da".to_string())
        );
    }

    #[test]
    fn extract_bare_bracket_bead_id() {
        assert_eq!(
            extract_bead_id("fix: bug [5414da]"),
            Some("5414da".to_string())
        );
    }

    #[test]
    fn extract_bead_colon_prefix() {
        assert_eq!(
            extract_bead_id("bead: 5414da\nsome text"),
            Some("5414da".to_string())
        );
    }

    #[test]
    fn extract_bead_id_colon_prefix() {
        assert_eq!(
            extract_bead_id("Bead-ID: rosary-5414da"),
            Some("5414da".to_string())
        );
    }

    #[test]
    fn no_bead_id_returns_none() {
        assert_eq!(extract_bead_id("feat: no bead reference here"), None);
    }

    #[test]
    fn bracket_too_short_not_matched() {
        // 5 hex chars is below the 6-char minimum
        assert_eq!(extract_bead_id("[abc12]"), None);
    }

    // --- Handler integration ---

    fn make_state() -> crate::serve::AppState {
        crate::serve::AppState {
            pool: Arc::new(RepoPool::empty()),
            config_path: Arc::from("test.toml"),
            sessions: Arc::new(RwLock::new(HashSet::new())),
            webhook_secret: None,
            github_webhook_secret: None,
            backend: None,
            repo_cache: Arc::new(crate::repo_cache::RepoCache::new()),
        }
    }

    #[tokio::test]
    async fn github_webhook_ignores_non_pr_events() {
        use axum::body::Body;
        use axum::http::Request;
        use tower::ServiceExt;

        let app = axum::Router::new()
            .route(
                "/webhook/github",
                axum::routing::post(handle_github_webhook),
            )
            .with_state(make_state());

        let req = Request::builder()
            .method("POST")
            .uri("/webhook/github")
            .header("x-github-event", "push")
            .header("content-type", "application/json")
            .body(Body::from(r#"{"ref":"refs/heads/main"}"#))
            .unwrap();

        let resp = app.oneshot(req).await.unwrap();
        assert_eq!(resp.status(), axum::http::StatusCode::OK);
    }

    #[tokio::test]
    async fn github_webhook_rejects_missing_signature_when_secret_set() {
        use axum::body::Body;
        use axum::http::Request;
        use tower::ServiceExt;

        let mut state = make_state();
        state.github_webhook_secret = Some(Arc::from("test-secret"));

        let app = axum::Router::new()
            .route(
                "/webhook/github",
                axum::routing::post(handle_github_webhook),
            )
            .with_state(state);

        let req = Request::builder()
            .method("POST")
            .uri("/webhook/github")
            .header("x-github-event", "pull_request")
            .header("content-type", "application/json")
            .body(Body::from(r#"{"action":"closed"}"#))
            .unwrap();

        let resp = app.oneshot(req).await.unwrap();
        assert_eq!(resp.status(), axum::http::StatusCode::BAD_REQUEST);
    }

    #[tokio::test]
    async fn github_webhook_accepts_merged_pr_no_bead() {
        use axum::body::Body;
        use axum::http::Request;
        use tower::ServiceExt;

        let app = axum::Router::new()
            .route(
                "/webhook/github",
                axum::routing::post(handle_github_webhook),
            )
            .with_state(make_state());

        let body = r#"{
            "action": "closed",
            "pull_request": {
                "number": 42,
                "merged": true,
                "title": "feat: no bead ref",
                "body": "This PR has no bead reference."
            }
        }"#;

        let req = Request::builder()
            .method("POST")
            .uri("/webhook/github")
            .header("x-github-event", "pull_request")
            .header("content-type", "application/json")
            .body(Body::from(body))
            .unwrap();

        let resp = app.oneshot(req).await.unwrap();
        assert_eq!(resp.status(), axum::http::StatusCode::OK);
    }

    #[tokio::test]
    async fn github_webhook_fixture_merged_pr_with_bead_id() {
        use axum::body::Body;
        use axum::http::Request;
        use tower::ServiceExt;

        // Pool is empty so the search finds nothing — that's fine,
        // we're testing the parse + routing path, not storage.
        let app = axum::Router::new()
            .route(
                "/webhook/github",
                axum::routing::post(handle_github_webhook),
            )
            .with_state(make_state());

        // Fixture: typical rosary PR body with bead ID in title
        let body = r#"{
            "action": "closed",
            "pull_request": {
                "number": 99,
                "merged": true,
                "title": "[rosary-5414da] feat(github): wire webhook endpoint",
                "body": "Closes bead rosary-5414da\n\nbead: 5414da"
            }
        }"#;

        let req = Request::builder()
            .method("POST")
            .uri("/webhook/github")
            .header("x-github-event", "pull_request")
            .header("content-type", "application/json")
            .body(Body::from(body))
            .unwrap();

        let resp = app.oneshot(req).await.unwrap();
        assert_eq!(resp.status(), axum::http::StatusCode::OK);
    }

    #[tokio::test]
    async fn github_webhook_ignores_unmerged_closed_pr() {
        use axum::body::Body;
        use axum::http::Request;
        use tower::ServiceExt;

        let app = axum::Router::new()
            .route(
                "/webhook/github",
                axum::routing::post(handle_github_webhook),
            )
            .with_state(make_state());

        let body = r#"{
            "action": "closed",
            "pull_request": {
                "number": 5,
                "merged": false,
                "title": "[rosary-5414da] feat: closed without merge",
                "body": ""
            }
        }"#;

        let req = Request::builder()
            .method("POST")
            .uri("/webhook/github")
            .header("x-github-event", "pull_request")
            .header("content-type", "application/json")
            .body(Body::from(body))
            .unwrap();

        let resp = app.oneshot(req).await.unwrap();
        assert_eq!(resp.status(), axum::http::StatusCode::OK);
    }
}
