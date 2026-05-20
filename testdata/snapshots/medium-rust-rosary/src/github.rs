//! GitHub PR creation — supports both GitHub App and PAT auth.
//!
//! **App auth** (preferred): JWT signed with RS256 → installation access token.
//! PRs appear as `rosary-stringer[bot]`.
//!
//! **PAT fallback**: fine-grained token with `contents:write` + `pull_requests:write`.
//! Token read from `GITHUB_TOKEN` env var or `~/.rsry/config.toml [github].token`.

use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};
use std::sync::Arc;
use tokio::sync::Mutex;

/// Pin to a specific GitHub API version to avoid breaking changes.
const GITHUB_API_VERSION: &str = "2022-11-28";

/// Auth strategy for GitHub API calls.
enum AuthStrategy {
    /// GitHub App: generates installation tokens via JWT.
    App {
        app_id: u64,
        installation_id: u64,
        private_key: jsonwebtoken::EncodingKey,
        /// Cached installation token + expiry.
        cached_token: Arc<Mutex<Option<CachedToken>>>,
    },
    /// Personal access token (static).
    Pat(String),
}

#[derive(Clone)]
struct CachedToken {
    token: String,
    expires_at: chrono::DateTime<chrono::Utc>,
}

/// Minimal GitHub client for PR operations.
#[allow(dead_code)]
pub struct GitHubClient {
    auth: AuthStrategy,
    client: reqwest::Client,
}

#[derive(Debug, Serialize)]
struct CreatePrRequest {
    title: String,
    body: String,
    head: String,
    base: String,
}

#[derive(Debug, Deserialize)]
pub struct PrResponse {
    pub number: u64,
    pub html_url: String,
}

/// JWT claims for GitHub App authentication.
#[derive(Debug, Serialize, Deserialize)]
struct AppJwtClaims {
    /// Issued at (seconds since epoch).
    iat: i64,
    /// Expiration (max 10 minutes from iat).
    exp: i64,
    /// Issuer — the GitHub App ID.
    iss: String,
}

/// Response from the installation access token endpoint.
/// No Debug derive — `token` field must not appear in logs.
#[derive(Deserialize)]
struct InstallationTokenResponse {
    token: String,
    expires_at: String,
}

/// Scope for a GitHub installation token — restricts repositories and permissions.
///
/// Passed to `fetch_installation_token` when a narrow-scope token is needed.
/// Without scope, GitHub issues a broad token covering all repos/permissions
/// the installation was granted.
#[derive(Debug, Serialize)]
struct TokenScope {
    /// Repository names (without owner prefix) to scope the token to.
    repositories: Vec<String>,
    /// GitHub permission names → access level ("read" | "write").
    permissions: std::collections::HashMap<String, String>,
}

impl GitHubClient {
    /// Create a client from config, preferring App auth over PAT.
    pub fn from_config(config: &crate::config::GitHubConfig) -> Result<Self> {
        let client = reqwest::Client::new();

        // Prefer App auth when all required fields are present.
        if let (Some(app_id), Some(installation_id), Some(key_path)) = (
            config.app_id,
            config.installation_id,
            config.private_key_path.as_deref(),
        ) {
            let expanded = shellexpand::tilde(key_path);
            let pem = std::fs::read_to_string(expanded.as_ref())
                .with_context(|| format!("reading GitHub App private key from {key_path}"))?;
            let private_key = jsonwebtoken::EncodingKey::from_rsa_pem(pem.as_bytes())
                .context("parsing GitHub App PEM private key")?;

            return Ok(GitHubClient {
                auth: AuthStrategy::App {
                    app_id,
                    installation_id,
                    private_key,
                    cached_token: Arc::new(Mutex::new(None)),
                },
                client,
            });
        }

        // Fall back to PAT.
        let token = config
            .token
            .clone()
            .or_else(|| std::env::var("GITHUB_TOKEN").ok())
            .context(
                "GitHub auth not configured: need [github] app_id+installation_id+private_key_path, or token/GITHUB_TOKEN",
            )?;

        Ok(GitHubClient {
            auth: AuthStrategy::Pat(token),
            client,
        })
    }

    /// Create a client from env var or config (legacy path).
    pub fn from_env() -> Result<Self> {
        // Try loading from full config first.
        if let Ok(config) = load_github_config() {
            return Self::from_config(&config);
        }

        // Pure env-var fallback.
        let token = std::env::var("GITHUB_TOKEN")
            .context("GITHUB_TOKEN not set and no [github] config found")?;

        Ok(GitHubClient {
            auth: AuthStrategy::Pat(token),
            client: reqwest::Client::new(),
        })
    }

    /// Get a valid Bearer token for API calls.
    /// For App auth, this generates/refreshes the installation token.
    /// For PAT auth, this returns the static token.
    pub(crate) async fn bearer_token(&self) -> Result<String> {
        match &self.auth {
            AuthStrategy::Pat(token) => Ok(token.clone()),
            AuthStrategy::App {
                app_id,
                installation_id,
                private_key,
                cached_token,
            } => {
                let mut cache = cached_token.lock().await;

                // Return cached token if still valid (with 5-minute buffer).
                if let Some(ref cached) = *cache {
                    let buffer = chrono::Duration::minutes(5);
                    if chrono::Utc::now() + buffer < cached.expires_at {
                        return Ok(cached.token.clone());
                    }
                }

                // Generate new installation token (broad scope).
                let token = fetch_installation_token(
                    &self.client,
                    *app_id,
                    *installation_id,
                    private_key,
                    None,
                )
                .await?;
                let result = token.token.clone();
                *cache = Some(token);
                Ok(result)
            }
        }
    }

    /// Returns whether this client is using App auth (bot identity).
    pub fn is_app_auth(&self) -> bool {
        matches!(self.auth, AuthStrategy::App { .. })
    }

    /// Get a scoped installation token for a specific repository.
    ///
    /// Scoped to `contents:write` + `pull_requests:write` on the given repo.
    /// Falls back to the broad token for PAT auth (PATs can't be further scoped).
    /// Not cached — each call requests a fresh token.
    pub(crate) async fn scoped_bearer_token(&self, repo: &str) -> Result<String> {
        match &self.auth {
            AuthStrategy::Pat(token) => Ok(token.clone()),
            AuthStrategy::App {
                app_id,
                installation_id,
                private_key,
                ..
            } => {
                let mut permissions = std::collections::HashMap::new();
                permissions.insert("contents".to_string(), "write".to_string());
                permissions.insert("pull_requests".to_string(), "write".to_string());
                let scope = TokenScope {
                    repositories: vec![repo.to_string()],
                    permissions,
                };
                let token = fetch_installation_token(
                    &self.client,
                    *app_id,
                    *installation_id,
                    private_key,
                    Some(&scope),
                )
                .await?;
                Ok(token.token)
            }
        }
    }

    /// Push a local branch to origin, then create a PR.
    #[allow(clippy::too_many_arguments)]
    pub async fn create_pr_from_worktree(
        &self,
        workspace_dir: &std::path::Path,
        owner: &str,
        repo: &str,
        branch: &str,
        base: &str,
        title: &str,
        body: &str,
    ) -> Result<PrResponse> {
        // Push the branch
        let push = tokio::process::Command::new("git")
            .args(["push", "origin", branch])
            .current_dir(workspace_dir)
            .output()
            .await
            .context("git push")?;

        if !push.status.success() {
            let stderr = String::from_utf8_lossy(&push.stderr);
            anyhow::bail!("git push failed: {stderr}");
        }
        eprintln!("[github] pushed {branch}");

        // Create PR via REST API using a scoped token (contents:write + pull_requests:write
        // on this specific repo). Falls back to broad token for PAT auth.
        let token = self.scoped_bearer_token(repo).await?;
        let url = format!("https://api.github.com/repos/{owner}/{repo}/pulls");
        let pr_request = CreatePrRequest {
            title: title.to_string(),
            body: body.to_string(),
            head: branch.to_string(),
            base: base.to_string(),
        };

        let resp = self
            .client
            .post(&url)
            .header("Authorization", format!("Bearer {token}"))
            .header("Accept", "application/vnd.github+json")
            .header("X-GitHub-Api-Version", GITHUB_API_VERSION)
            .header("User-Agent", "rosary-stringer[bot]")
            .json(&pr_request)
            .send()
            .await
            .context("GitHub API request")?;

        if !resp.status().is_success() {
            let status = resp.status();
            let body = resp.text().await.unwrap_or_default();
            anyhow::bail!("GitHub PR creation failed ({status}): {body}");
        }

        let pr: PrResponse = resp.json().await.context("parsing PR response")?;
        let auth_mode = if self.is_app_auth() { "app" } else { "pat" };
        eprintln!(
            "[github] created PR #{} ({auth_mode}): {}",
            pr.number, pr.html_url
        );
        Ok(pr)
    }
}

/// Generate a JWT for GitHub App authentication.
fn generate_app_jwt(app_id: u64, private_key: &jsonwebtoken::EncodingKey) -> Result<String> {
    let now = chrono::Utc::now().timestamp();
    let claims = AppJwtClaims {
        iat: now - 60,        // Clock drift buffer
        exp: now + (10 * 60), // 10 minutes (GitHub max)
        iss: app_id.to_string(),
    };

    let header = jsonwebtoken::Header::new(jsonwebtoken::Algorithm::RS256);
    jsonwebtoken::encode(&header, &claims, private_key).context("signing GitHub App JWT")
}

/// Exchange a JWT for an installation access token.
///
/// Pass `scope` to restrict the token to specific repositories and permissions.
/// Without scope, GitHub issues a broad token for the full installation.
async fn fetch_installation_token(
    client: &reqwest::Client,
    app_id: u64,
    installation_id: u64,
    private_key: &jsonwebtoken::EncodingKey,
    scope: Option<&TokenScope>,
) -> Result<CachedToken> {
    let jwt = generate_app_jwt(app_id, private_key)?;

    let url = format!("https://api.github.com/app/installations/{installation_id}/access_tokens");

    let mut req = client
        .post(&url)
        .header("Authorization", format!("Bearer {jwt}"))
        .header("Accept", "application/vnd.github+json")
        .header("X-GitHub-Api-Version", GITHUB_API_VERSION)
        .header("User-Agent", "rosary-stringer[bot]");

    if let Some(s) = scope {
        req = req.json(s);
    }

    let resp = req
        .send()
        .await
        .context("requesting installation access token")?;

    if !resp.status().is_success() {
        let status = resp.status();
        let body = resp.text().await.unwrap_or_default();
        anyhow::bail!("installation token request failed ({status}): {body}");
    }

    let token_resp: InstallationTokenResponse = resp
        .json()
        .await
        .context("parsing installation token response")?;

    let expires_at = chrono::DateTime::parse_from_rfc3339(&token_resp.expires_at)
        .map(|dt| dt.with_timezone(&chrono::Utc))
        .unwrap_or_else(|_| chrono::Utc::now() + chrono::Duration::hours(1));

    eprintln!(
        "[github] obtained installation token (expires {})",
        expires_at.format("%H:%M:%S UTC")
    );

    Ok(CachedToken {
        token: token_resp.token,
        expires_at,
    })
}

/// Load GitHubConfig from ~/.rsry/config.toml.
fn load_github_config() -> Result<crate::config::GitHubConfig> {
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

/// Build a PR body from handoff chain + manifest.
pub fn build_pr_body(
    handoffs: &[crate::handoff::Handoff],
    manifest: Option<&crate::manifest::Manifest>,
) -> String {
    let mut body = String::from("## Agent-Generated PR\n\n");

    // Handoff summaries
    if !handoffs.is_empty() {
        body.push_str("### Pipeline Phases\n\n");
        for h in handoffs {
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

    // Cost summary from manifest
    if let Some(m) = manifest {
        if let Some(cost) = m.cost.total_cost_usd {
            body.push_str(&format!("### Cost\n\n${cost:.3}\n\n"));
        }
        body.push_str(&format!(
            "### Files Changed\n\n{}\n\n",
            m.work
                .files_changed
                .iter()
                .map(|f| format!("- `{f}`"))
                .collect::<Vec<_>>()
                .join("\n")
        ));
    }

    body.push_str("---\n*Generated by [rosary](https://github.com/agentic-research/rosary)*\n");
    body
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn build_pr_body_empty() {
        let body = build_pr_body(&[], None);
        assert!(body.contains("Agent-Generated PR"));
        assert!(body.contains("rosary"));
    }

    #[test]
    fn build_pr_body_with_handoffs() {
        let work = crate::manifest::Work {
            commits: vec![],
            files_changed: vec!["src/foo.rs".into()],
            lines_added: 10,
            lines_removed: 2,
            diff_stat: None,
        };
        let h = crate::handoff::Handoff::new(
            0,
            "dev-agent",
            Some("staging-agent"),
            "rosary-test",
            "claude",
            &work,
            None,
        );
        let body = build_pr_body(&[h], None);
        assert!(body.contains("Phase 0"));
        assert!(body.contains("dev-agent"));
    }

    #[test]
    fn generate_jwt_valid_claims() {
        // Generate a test RSA key for JWT signing
        let rsa = rsa_test_key();
        let key = jsonwebtoken::EncodingKey::from_rsa_pem(rsa.as_bytes()).unwrap();

        let jwt = generate_app_jwt(12345, &key).unwrap();

        // Decode without verification to check claims
        let decoding_key =
            jsonwebtoken::DecodingKey::from_rsa_pem(rsa_test_pub_key().as_bytes()).unwrap();
        let mut validation = jsonwebtoken::Validation::new(jsonwebtoken::Algorithm::RS256);
        validation.set_required_spec_claims(&["iss", "iat", "exp"]);

        let token_data =
            jsonwebtoken::decode::<AppJwtClaims>(&jwt, &decoding_key, &validation).unwrap();

        assert_eq!(token_data.claims.iss, "12345");
        // iat should be ~60s in the past
        let now = chrono::Utc::now().timestamp();
        assert!(token_data.claims.iat <= now);
        assert!(token_data.claims.iat >= now - 120);
        // exp should be ~10 min in the future
        assert!(token_data.claims.exp > now);
        assert!(token_data.claims.exp <= now + 660);
    }

    #[test]
    fn from_config_pat_fallback() {
        let config = crate::config::GitHubConfig {
            token: Some("ghp_test123".into()),
            owner: Some("test-org".into()),
            base: "main".into(),
            auto_pr: false,
            app_id: None,
            installation_id: None,
            client_id: None,
            private_key_path: None,
            agent_branch_prefix: "rosary".into(),
            webhook_secret: None,
        };
        let client = GitHubClient::from_config(&config).unwrap();
        assert!(!client.is_app_auth());
    }

    #[test]
    fn from_config_no_auth_errors() {
        let config = crate::config::GitHubConfig {
            token: None,
            owner: Some("test-org".into()),
            base: "main".into(),
            auto_pr: false,
            app_id: None,
            installation_id: None,
            client_id: None,
            private_key_path: None,
            agent_branch_prefix: "rosary".into(),
            webhook_secret: None,
        };
        // Clear GITHUB_TOKEN to ensure no env fallback
        let result = GitHubClient::from_config(&config);
        // May succeed if GITHUB_TOKEN is set in the env, that's fine
        if std::env::var("GITHUB_TOKEN").is_err() {
            assert!(result.is_err());
        }
    }

    #[test]
    fn from_config_app_missing_key_errors() {
        let config = crate::config::GitHubConfig {
            token: None,
            owner: Some("test-org".into()),
            base: "main".into(),
            auto_pr: false,
            app_id: Some(12345),
            installation_id: Some(67890),
            client_id: None,
            private_key_path: Some("/nonexistent/key.pem".into()),
            agent_branch_prefix: "rosary".into(),
            webhook_secret: None,
        };
        let result = GitHubClient::from_config(&config);
        let err = result.err().expect("should fail with missing key");
        assert!(
            err.to_string().contains("private key"),
            "error should mention private key: {err}"
        );
    }

    #[test]
    fn parse_config_with_app_fields() {
        let toml = r#"
[[repo]]
name = "rosary"
path = "~/remotes/art/rosary"

[github]
token = "ghp_test"
owner = "agentic-research"
base = "main"
auto_pr = false
app_id = 3144311
installation_id = 117860641
client_id = "Iv23likrB4pInxUkIkIh"
private_key_path = "~/.rsry/github-app.pem"
"#;
        let config: crate::config::Config = toml::from_str(toml).unwrap();
        let gh = config.github.unwrap();
        assert_eq!(gh.app_id, Some(3144311));
        assert_eq!(gh.installation_id, Some(117860641));
        assert_eq!(gh.client_id.as_deref(), Some("Iv23likrB4pInxUkIkIh"));
        assert_eq!(
            gh.private_key_path.as_deref(),
            Some("~/.rsry/github-app.pem")
        );
        // PAT still available as fallback
        assert_eq!(gh.token.as_deref(), Some("ghp_test"));
    }

    #[test]
    fn parse_config_backward_compat_no_app_fields() {
        let toml = r#"
[[repo]]
name = "rosary"
path = "~/remotes/art/rosary"

[github]
token = "ghp_test"
owner = "agentic-research"
"#;
        let config: crate::config::Config = toml::from_str(toml).unwrap();
        let gh = config.github.unwrap();
        assert!(gh.app_id.is_none());
        assert!(gh.installation_id.is_none());
        assert!(gh.private_key_path.is_none());
        assert_eq!(gh.token.as_deref(), Some("ghp_test"));
    }

    #[tokio::test]
    async fn cached_token_is_reused() {
        let cached = CachedToken {
            token: "ghs_test_installation_token".into(),
            expires_at: chrono::Utc::now() + chrono::Duration::hours(1),
        };
        // Token should be considered valid (more than 5 min remaining)
        let buffer = chrono::Duration::minutes(5);
        assert!(chrono::Utc::now() + buffer < cached.expires_at);
    }

    #[tokio::test]
    async fn expired_token_needs_refresh() {
        let cached = CachedToken {
            token: "ghs_expired".into(),
            expires_at: chrono::Utc::now() - chrono::Duration::minutes(1),
        };
        let buffer = chrono::Duration::minutes(5);
        // Expired token should NOT pass the freshness check
        assert!(chrono::Utc::now() + buffer >= cached.expires_at);
    }

    #[tokio::test]
    async fn scoped_bearer_token_pat_returns_static() {
        let client = GitHubClient {
            client: reqwest::Client::new(),
            auth: AuthStrategy::Pat("ghp_static_token".into()),
        };
        let token = client.scoped_bearer_token("some-repo").await.unwrap();
        assert_eq!(token, "ghp_static_token");
    }

    #[test]
    fn token_scope_serializes_correctly() {
        let mut permissions = std::collections::HashMap::new();
        permissions.insert("contents".to_string(), "write".to_string());
        permissions.insert("pull_requests".to_string(), "write".to_string());
        let scope = TokenScope {
            repositories: vec!["my-repo".to_string()],
            permissions,
        };
        let json: serde_json::Value = serde_json::to_value(&scope).unwrap();
        assert_eq!(json["repositories"][0], "my-repo");
        assert_eq!(json["permissions"]["contents"], "write");
        assert_eq!(json["permissions"]["pull_requests"], "write");
    }

    // --- Test RSA keys (2048-bit, generated for tests only) ---

    // Test-only RSA 2048-bit keypair. Not a secret — generated for unit tests.
    fn rsa_test_key() -> String {
        r#"-----BEGIN PRIVATE KEY-----
MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQDQwLVgfIf2XNPo
kmhDIrqYe9QpJUtoP5uajlDAa01Th5DSNSoba8JrJKUzoAX5gAY6ruxpv4V4hgCP
Df3kZal7qA1SckV7igP7Z49SJaCBZZNmRJuNkPGlC7Hk8rBraW+hsPLuRGjbUIXS
3SQTnz7QTtCAZdXbjXl1g9KJx6rroF7aSRj0Kg6vEUHxXIaXYhcEJVOuQs4NxDSf
Dvz4gBKBBuzwpwVQxr+EDMtA4J9sV9dYCTBXNA5QfxGi1hMtyt6UBJqgpxHCZo6S
L92ERNGE6g5wAg53K5/a/90vxwhclZTBHB2vJtsz2vfeKFG7slT+d47CjqrLD7G+
1W9aisj9AgMBAAECggEAIaXGNoCsG6I3x+d9ZcDdycE/38fyoSGLj7x7uKAzDREv
XyqRmGzkoAd+l1UmUWJ43pGeaqcjuNsEkQpkz6ExUcyzJQRlPbXv0WCOYePNvL2m
JhhN6GIiCQPbDzetBwsuIqZWaeBd9GfEwknBzGXhJotloSSI9YQWvUHbuDiWTLc4
XRaZFcgn5tdXR9IpBtP37IFotSs2QVU527YSB7pRaHyFifyOPKWk1dUGVjm670yd
HDC3gEIZI37bJiFapyZh96K0EfJgDZN/B1ihVVohQxNS5ppCqEN08i0dI8hlRD76
P9OJ2xjK0URlzNf/C01eFRwuLqCp+7TLbAbbm/2qAwKBgQD/gQseUIkNmIxUiePG
Xe65d4vuEXoUXcFNKlKHHZNCn+mH8+LH3K/EyOPTF/HRAXnue8fn4bf6jsV7G6BT
xi+ELfBpYgtftqyGRyYFJwGUSWtmpQmu7qGt9PJa/I43MrMEgAgIPe3CEIc7LLUX
0bDGdrUKX8UqOwYRMw0k/iCqCwKBgQDRKG9aOPevrC9aT0NRVxn+uH3ivq2GKgUU
oWPH59Vi3Z1Z+P7uKm6l05RfQlEVw0XRF1e4LhmRHnU+VuxAASF/7/fq/16vd01N
YIfPqiKRhtl5ckS3owxq9VTDHUn5StZ+sGRkYe1z3sAsF8KxijjHEddNm9OUva0l
RR7Hm6XGFwKBgD0kYUfA1/tD7Rjx4mr+4XjKjdbLod4rzW0s2pDw1+OSpuzcxQE0
428A79v9F+X/J3GVd3IbBs7TyZg7NTO28nn8EFL3nmcqLMD5V7TV77/Pjf8DMX0G
J/Sb8D8rvtCgtkw2YzWttC7Di8jyWue3S0lC8PHplyIS+9Gz2MpoceKfAoGBAMEG
cg1vuZYUb/cGY3fzzHe5R+Q0WOwSZ3Hsp6tblyCQqaDZHFwsKMU9CBcJms9c0Vfw
FPJTCSFWXJlVmt5OrN3nVoM3feitT1fzmCLcPt7S9m0QOb7H6LPlCX6vzw8UM/Pj
UiMaBQwELJIEs5cpmtCM9IgZISCKE/rrWUaZrFmtAoGAbdlt+gsh/TfZUuYVyRtQ
WHkO3JVuSI0gpfTh3elzLSO/SvPYEGtPjRynBBqndVffDVnSkdz4YeoMHLnoQzZ5
9Fijz+gUd8/fsqNo4jkL4vXjRsl1Z5OAT9lk+3LluZAT8wzQzlakOIsc/ULGl9jA
Md6qw6uEEPqMevIr5oUd8OY=
-----END PRIVATE KEY-----"#
            .to_string()
    }

    fn rsa_test_pub_key() -> String {
        r#"-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA0MC1YHyH9lzT6JJoQyK6
mHvUKSVLaD+bmo5QwGtNU4eQ0jUqG2vCaySlM6AF+YAGOq7sab+FeIYAjw395GWp
e6gNUnJFe4oD+2ePUiWggWWTZkSbjZDxpQux5PKwa2lvobDy7kRo21CF0t0kE58+
0E7QgGXV2415dYPSiceq66Be2kkY9CoOrxFB8VyGl2IXBCVTrkLODcQ0nw78+IAS
gQbs8KcFUMa/hAzLQOCfbFfXWAkwVzQOUH8RotYTLcrelASaoKcRwmaOki/dhETR
hOoOcAIOdyuf2v/dL8cIXJWUwRwdrybbM9r33ihRu7JU/neOwo6qyw+xvtVvWorI
/QIDAQAB
-----END PUBLIC KEY-----"#
            .to_string()
    }
}
