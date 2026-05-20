//! HTTP client for the sprites.dev compute API.
//!
//! API base: `https://api.sprites.dev/v1`
//! Auth: `Authorization: Bearer {token}`
//! Docs: https://sprites.dev/api/sprites
//!
//! Built against real API docs fetched 2026-03-14. Each endpoint references
//! the doc page it was validated against.

use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};
use serde_json::json;

const DEFAULT_BASE_URL: &str = "https://api.sprites.dev/v1";

// ---------------------------------------------------------------------------
// API types — from https://sprites.dev/api/sprites#create
// ---------------------------------------------------------------------------

/// Sprite resource.
/// Ref: https://sprites.dev/api/sprites#create (response schema)
#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct Sprite {
    pub id: String,
    pub name: String,
    #[serde(default)]
    pub status: String,
    #[serde(default)]
    pub url: Option<String>,
    #[serde(default)]
    pub organization: Option<String>,
    #[serde(default)]
    pub created_at: Option<String>,
    #[serde(default)]
    pub updated_at: Option<String>,
}

/// Options for creating a sprite.
/// Ref: https://sprites.dev/api/sprites#create (request body)
/// Only `name` is sent; `url_settings` optional.
#[derive(Debug, Clone, Default, Serialize)]
pub struct CreateOpts {
    /// URL auth setting: "sprite" (default) or "public".
    #[serde(skip_serializing_if = "Option::is_none")]
    pub url_auth: Option<String>,
}

/// Checkpoint.
/// Ref: https://sprites.dev/api/sprites/checkpoints (list response)
#[derive(Debug, Clone, Deserialize)]
pub struct Checkpoint {
    pub id: String,
    #[serde(default)]
    pub create_time: Option<String>,
    #[serde(default)]
    pub comment: Option<String>,
}

/// Exec output — assembled from the HTTP POST exec response.
/// Ref: https://sprites.dev/api/sprites/exec (HTTP POST)
///
/// The HTTP POST endpoint takes `?cmd=...` query params and returns
/// `application/json`. For streaming/exit codes, WebSocket is needed.
#[derive(Debug, Clone)]
pub struct ExecOutput {
    pub exit_code: i32,
    pub stdout: String,
    pub stderr: String,
}

/// NDJSON streaming event from checkpoint/service endpoints.
/// Ref: https://sprites.dev/api/sprites/checkpoints
#[derive(Debug, Clone, Deserialize)]
pub struct StreamEvent {
    #[serde(rename = "type")]
    pub event_type: String,
    #[serde(default)]
    pub data: Option<String>,
    #[serde(default)]
    pub error: Option<String>,
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

/// HTTP client for sprites.dev REST API.
#[derive(Debug)]
pub struct SpritesClient {
    client: reqwest::Client,
    base_url: String,
}

impl SpritesClient {
    pub fn new(token: &str) -> Result<Self> {
        Self::with_base_url(token, DEFAULT_BASE_URL)
    }

    pub fn with_base_url(token: &str, base_url: &str) -> Result<Self> {
        use reqwest::header::{AUTHORIZATION, HeaderMap, HeaderValue};

        anyhow::ensure!(!token.is_empty(), "sprites API token is empty");

        let bearer = format!("Bearer {token}");
        let mut headers = HeaderMap::new();
        headers.insert(
            AUTHORIZATION,
            HeaderValue::from_str(&bearer).context("invalid token characters")?,
        );

        let client = reqwest::Client::builder()
            .default_headers(headers)
            .timeout(std::time::Duration::from_secs(30))
            .build()
            .context("building HTTP client")?;

        Ok(Self {
            client,
            base_url: base_url.trim_end_matches('/').to_string(),
        })
    }

    fn url(&self, path: &str) -> String {
        format!("{}{}", self.base_url, path)
    }

    // -- Sprite CRUD --
    // Ref: https://sprites.dev/api/sprites#create

    /// POST /v1/sprites — create a sprite.
    pub async fn create_sprite(&self, name: &str, opts: &CreateOpts) -> Result<Sprite> {
        let url = self.url("/sprites");
        let mut body = json!({ "name": name });
        if let Some(ref auth) = opts.url_auth {
            body["url_settings"] = json!({ "auth": auth });
        }

        let resp = self
            .client
            .post(&url)
            .json(&body)
            .send()
            .await
            .context("creating sprite")?;

        let status = resp.status();
        let text = resp.text().await.context("reading create response")?;
        if !status.is_success() {
            anyhow::bail!("create sprite failed ({status}): {text}");
        }

        serde_json::from_str(&text).context("parsing create sprite response")
    }

    /// GET /v1/sprites/{name}
    pub async fn get_sprite(&self, name: &str) -> Result<Sprite> {
        let url = self.url(&format!("/sprites/{name}"));
        let resp = self
            .client
            .get(&url)
            .send()
            .await
            .context("getting sprite")?;

        let status = resp.status();
        let text = resp.text().await.context("reading get response")?;
        if !status.is_success() {
            anyhow::bail!("get sprite failed ({status}): {text}");
        }

        serde_json::from_str(&text).context("parsing get sprite response")
    }

    /// DELETE /v1/sprites/{name}
    pub async fn delete_sprite(&self, name: &str) -> Result<()> {
        let url = self.url(&format!("/sprites/{name}"));
        let resp = self
            .client
            .delete(&url)
            .send()
            .await
            .context("deleting sprite")?;

        if !resp.status().is_success() {
            let text = resp.text().await.unwrap_or_default();
            anyhow::bail!("delete sprite failed: {text}");
        }

        Ok(())
    }

    // -- Exec (HTTP POST) --
    // Ref: https://sprites.dev/api/sprites/exec
    // Query params: ?cmd=...&dir=...&env=KEY=VALUE
    // Response: application/json (for HTTP POST variant)

    /// POST /v1/sprites/{name}/exec?cmd=...
    ///
    /// Uses the HTTP POST variant. For exit codes + streaming, the WebSocket
    /// variant is needed (future: tokio-tungstenite).
    pub async fn exec(&self, name: &str, cmd: &str) -> Result<ExecOutput> {
        let url = self.url(&format!("/sprites/{name}/exec"));

        let resp = self
            .client
            .post(&url)
            .query(&[("cmd", cmd)])
            .send()
            .await
            .with_context(|| format!("exec in {name}: {cmd}"))?;

        let status = resp.status();
        let text = resp.text().await.context("reading exec response")?;

        if !status.is_success() {
            return Ok(ExecOutput {
                exit_code: 1,
                stdout: String::new(),
                stderr: text,
            });
        }

        // HTTP POST exec returns stdout as the body.
        // No structured exit_code — infer from HTTP status (200 = success).
        // For real exit codes, use the WebSocket variant.
        Ok(ExecOutput {
            exit_code: 0,
            stdout: text,
            stderr: String::new(),
        })
    }

    /// POST /v1/sprites/{name}/exec?cmd=...
    /// Wraps the command to capture the real exit code via sentinel.
    pub async fn exec_with_exit_code(&self, name: &str, cmd: &str) -> Result<ExecOutput> {
        // Wrap: run cmd, capture exit code, echo sentinel
        let wrapped = format!(
            r#"sh -c '{cmd}; __ec=$?; echo ""; echo "RSRY_EXIT:$__ec"; exit 0'"#,
            cmd = cmd.replace('\'', "'\\''")
        );

        let result = self.exec(name, &wrapped).await?;

        // Parse sentinel from stdout
        let (stdout, exit_code) = if let Some(pos) = result.stdout.rfind("RSRY_EXIT:") {
            let code_str = result.stdout[pos + 10..].trim();
            let code = code_str.parse::<i32>().unwrap_or(-1);
            let clean_stdout = result.stdout[..pos].trim_end().to_string();
            (clean_stdout, code)
        } else {
            (result.stdout, result.exit_code)
        };

        Ok(ExecOutput {
            exit_code,
            stdout,
            stderr: result.stderr,
        })
    }

    // -- Checkpoints --
    // Ref: https://sprites.dev/api/sprites/checkpoints
    // NOTE: create is POST /checkpoint (singular), list is GET /checkpoints (plural)

    /// POST /v1/sprites/{name}/checkpoint — create checkpoint.
    /// Returns streaming NDJSON; we parse the checkpoint ID from it.
    pub async fn create_checkpoint(&self, name: &str, comment: Option<&str>) -> Result<Checkpoint> {
        let url = self.url(&format!("/sprites/{name}/checkpoint"));
        let body = if let Some(c) = comment {
            json!({ "comment": c })
        } else {
            json!({})
        };

        let resp = self
            .client
            .post(&url)
            .json(&body)
            .send()
            .await
            .context("creating checkpoint")?;

        if !resp.status().is_success() {
            let text = resp.text().await.unwrap_or_default();
            anyhow::bail!("create checkpoint failed: {text}");
        }

        // Response is streaming NDJSON. Parse lines for the checkpoint ID.
        let text = resp.text().await.context("reading checkpoint response")?;
        let mut checkpoint_id = None;

        for line in text.lines() {
            let line = line.trim();
            if line.is_empty() {
                continue;
            }
            if let Ok(event) = serde_json::from_str::<StreamEvent>(line) {
                // Look for "ID: v3" in info events
                if let Some(ref data) = event.data
                    && let Some(id_str) = data.strip_prefix("  ID: ")
                {
                    checkpoint_id = Some(id_str.trim().to_string());
                }
            }
        }

        Ok(Checkpoint {
            id: checkpoint_id.unwrap_or_else(|| "v0".to_string()),
            create_time: None,
            comment: comment.map(|c| c.to_string()),
        })
    }

    /// GET /v1/sprites/{name}/checkpoints — list checkpoints.
    pub async fn list_checkpoints(&self, name: &str) -> Result<Vec<Checkpoint>> {
        let url = self.url(&format!("/sprites/{name}/checkpoints"));
        let resp = self
            .client
            .get(&url)
            .send()
            .await
            .context("listing checkpoints")?;

        if !resp.status().is_success() {
            let text = resp.text().await.unwrap_or_default();
            anyhow::bail!("list checkpoints failed: {text}");
        }

        let text = resp.text().await.context("reading checkpoints response")?;
        serde_json::from_str(&text).context("parsing checkpoints")
    }

    /// POST /v1/sprites/{name}/checkpoints/{id}/restore
    pub async fn restore_checkpoint(&self, name: &str, checkpoint_id: &str) -> Result<()> {
        let url = self.url(&format!(
            "/sprites/{name}/checkpoints/{checkpoint_id}/restore"
        ));
        let resp = self
            .client
            .post(&url)
            .send()
            .await
            .context("restoring checkpoint")?;

        if !resp.status().is_success() {
            let text = resp.text().await.unwrap_or_default();
            anyhow::bail!("restore checkpoint failed: {text}");
        }

        Ok(())
    }

    // -- Filesystem --
    // Ref: https://sprites.dev/api/sprites/filesystem
    // Paths are query params, not URL path segments.

    /// PUT /v1/sprites/{name}/fs/write?path=...&workingDir=/
    pub async fn write_file(&self, name: &str, path: &str, content: &[u8]) -> Result<()> {
        let url = self.url(&format!("/sprites/{name}/fs/write"));
        let resp = self
            .client
            .put(&url)
            .query(&[("path", path), ("workingDir", "/")])
            .body(content.to_vec())
            .send()
            .await
            .context("writing file")?;

        if !resp.status().is_success() {
            let text = resp.text().await.unwrap_or_default();
            anyhow::bail!("write file failed: {text}");
        }

        Ok(())
    }

    /// GET /v1/sprites/{name}/fs/read?path=...&workingDir=/
    pub async fn read_file(&self, name: &str, path: &str) -> Result<Vec<u8>> {
        let url = self.url(&format!("/sprites/{name}/fs/read"));
        let resp = self
            .client
            .get(&url)
            .query(&[("path", path), ("workingDir", "/")])
            .send()
            .await
            .context("reading file")?;

        if !resp.status().is_success() {
            let text = resp.text().await.unwrap_or_default();
            anyhow::bail!("read file failed: {text}");
        }

        Ok(resp.bytes().await.context("reading file bytes")?.to_vec())
    }
}

// ---------------------------------------------------------------------------
// Tests — deserialization against real API response shapes from docs
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;

    // -- Construction --

    #[test]
    fn new_requires_token() {
        let result = SpritesClient::new("");
        assert!(result.is_err());
        assert!(result.unwrap_err().to_string().contains("token is empty"));
    }

    #[test]
    fn new_with_valid_token() {
        assert!(SpritesClient::new("test-token-123").is_ok());
    }

    #[test]
    fn with_base_url_overrides_default() {
        let client = SpritesClient::with_base_url("tok", "http://localhost:9999").unwrap();
        assert_eq!(client.url("/sprites"), "http://localhost:9999/sprites");
    }

    #[test]
    fn url_strips_trailing_slash() {
        let client = SpritesClient::with_base_url("tok", "http://localhost:9999/").unwrap();
        assert_eq!(client.url("/sprites"), "http://localhost:9999/sprites");
    }

    // -- Sprite deserialization (real response from 2026-03-14) --

    #[test]
    fn sprite_deserializes_real_response() {
        let json = r#"{
            "id": "sprite-091278d4-467a-4e40-b3d2-4fbda56ff365",
            "name": "rsry-test",
            "status": "warm",
            "version": null,
            "url": "https://rsry-test-wrsa.sprites.app",
            "url_settings": {"auth": "sprite"},
            "organization": "james-gardner-570",
            "last_running_at": "2026-03-14T21:32:03Z",
            "last_warming_at": "2026-03-14T21:32:05Z",
            "updated_at": "2026-03-14T21:31:57.855138Z",
            "created_at": "2026-03-14T21:31:57.855138Z",
            "environment_version": null
        }"#;
        let sprite: Sprite = serde_json::from_str(json).unwrap();
        assert_eq!(sprite.name, "rsry-test");
        assert_eq!(sprite.status, "warm");
        assert_eq!(sprite.id, "sprite-091278d4-467a-4e40-b3d2-4fbda56ff365");
        assert_eq!(
            sprite.url.as_deref(),
            Some("https://rsry-test-wrsa.sprites.app")
        );
        assert_eq!(sprite.organization.as_deref(), Some("james-gardner-570"));
    }

    // -- Checkpoint deserialization (real response from 2026-03-14) --

    #[test]
    fn checkpoint_list_deserializes() {
        let json = r#"[
            {"id":"Current","create_time":"2026-03-14T21:35:30Z","is_auto":false},
            {"id":"v1","create_time":"2026-03-14T21:31:53Z","is_auto":false},
            {"id":"v0","create_time":"2026-03-14T21:31:53Z","is_auto":false}
        ]"#;
        let cps: Vec<Checkpoint> = serde_json::from_str(json).unwrap();
        assert_eq!(cps.len(), 3);
        assert_eq!(cps[0].id, "Current");
        assert_eq!(cps[1].id, "v1");
        assert_eq!(cps[1].create_time.as_deref(), Some("2026-03-14T21:31:53Z"));
    }

    // -- Checkpoint create NDJSON stream parsing --

    #[test]
    fn stream_event_deserializes_info() {
        let json =
            r#"{"type":"info","data":"Creating checkpoint...","time":"2026-03-14T22:41:25Z"}"#;
        let event: StreamEvent = serde_json::from_str(json).unwrap();
        assert_eq!(event.event_type, "info");
        assert_eq!(event.data.as_deref(), Some("Creating checkpoint..."));
    }

    #[test]
    fn stream_event_deserializes_id_line() {
        let json = r#"{"type":"info","data":"  ID: v3","time":"2026-03-14T22:41:26Z"}"#;
        let event: StreamEvent = serde_json::from_str(json).unwrap();
        let id = event
            .data
            .as_deref()
            .and_then(|d| d.strip_prefix("  ID: "))
            .map(|s| s.trim());
        assert_eq!(id, Some("v3"));
    }

    // -- Exec output --

    #[test]
    fn exec_output_fields() {
        let out = ExecOutput {
            exit_code: 0,
            stdout: "hello\n".into(),
            stderr: String::new(),
        };
        assert_eq!(out.exit_code, 0);
        assert_eq!(out.stdout, "hello\n");
    }

    #[test]
    fn parse_exit_code_sentinel() {
        let raw_stdout = "hello world\n\nRSRY_EXIT:42\n";
        let pos = raw_stdout.rfind("RSRY_EXIT:").unwrap();
        let code: i32 = raw_stdout[pos + 10..].trim().parse().unwrap();
        let clean = raw_stdout[..pos].trim_end();
        assert_eq!(code, 42);
        assert_eq!(clean, "hello world");
    }

    // -- CreateOpts --

    #[test]
    fn create_opts_default() {
        let opts = CreateOpts::default();
        let json = serde_json::to_value(&opts).unwrap();
        // url_auth=None should be skipped
        assert!(json.get("url_auth").is_none());
    }
}
