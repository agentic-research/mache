//! MCP server for rosary — exposes beads capabilities as tools.
//!
//! Three transports, all dispatching through the same `handlers::call_tool`
//! surface:
//! - **stdio** (`run_stdio`): line-delimited JSON-RPC over stdin/stdout
//!   (default; used by Claude Code via `rsry serve --transport stdio`)
//! - **http** (`run_http`): MCP Streamable HTTP transport on `/mcp`
//!   (used by `rsry serve --transport http --port 8383`)
//! - **ipc** (`run_ipc`): capnp `ToolCall` / `ToolResult` over a Unix
//!   Domain Socket, intra-cluster wire per cloister ADR-0005 amendment
//!   (used by `rsry mcp --ipc-socket <path>`, the cluster.capnp invocation)

mod github_webhook;
mod handlers;
mod ipc;
mod landing;
// `scope_args` — boundary parser for `scope` vs `repo_path` MCP arg pair.
// Step 3 of the rosary-b5da2f scope-abstraction series. Consumed by
// `tool_bead_link` as of PR 4; other handlers adopt incrementally.
pub(crate) mod scope_args;
// `ticket_load` — Phase 0 of rosary-5d7141 (Linear-escalation-triage).
// RED phase landed under rosary-5dc9b0 — stubbed helpers + 5 failing tests
// pin the shape; GREEN phase wires Linear/GH/bead fetches through them.
pub(crate) mod ticket_load;
mod tools;
mod webhook;

use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

use std::collections::HashSet;
use std::sync::Arc;
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::sync::RwLock;

use crate::config;
use crate::pool::RepoPool;
use crate::store::BackendStore;

// ---------------------------------------------------------------------------
// Caller identity — extracted from CF client cert or Authorization header
// ---------------------------------------------------------------------------

/// Who is making this MCP request.
#[derive(Debug, Clone)]
#[allow(dead_code)] // Variants constructed as identity types are wired
pub(crate) enum CallerIdentity {
    /// Human user (CF client cert CN=user-{id})
    User(String),
    /// Machine acting on behalf of a user (API key → user_id)
    MachineAsUser { user_id: String, service: String },
    /// System-level service (signet cert CN=service-{name})
    Machine(String),
    /// No identity — localhost / development mode (single-tenant)
    Anonymous,
}

impl CallerIdentity {
    /// The user_id to scope queries by, or None for machine/anonymous.
    pub fn user_scope(&self) -> Option<&str> {
        match self {
            Self::User(id) => Some(id),
            Self::MachineAsUser { user_id, .. } => Some(user_id),
            Self::Machine(_) => None, // system-level, no user scoping
            Self::Anonymous => None,  // single-tenant, no scoping
        }
    }
}

/// Extract caller identity from request headers.
///
/// Priority:
/// 1. CF client cert headers (Cf-Client-Cert-Subject-Dn) — user or machine
/// 2. Authorization: Bearer <api_key> — machine-as-user (future: KV lookup)
/// 3. Anonymous — localhost/development
fn extract_identity(headers: &axum::http::HeaderMap) -> CallerIdentity {
    // Try CF client cert first
    let verified = headers
        .get("cf-client-cert-verified")
        .and_then(|v| v.to_str().ok());
    let dn = headers
        .get("cf-client-cert-subject-dn")
        .and_then(|v| v.to_str().ok());

    if let (Some("SUCCESS"), Some(dn)) = (verified, dn) {
        // Parse CN from DN (e.g., "CN=user-abc123" or "CN=service-conductor")
        let cn = dn
            .split(',')
            .find_map(|part| part.trim().strip_prefix("CN="))
            .unwrap_or(dn);

        if let Some(user_id) = cn.strip_prefix("user-") {
            return CallerIdentity::User(user_id.to_string());
        }
        if let Some(service) = cn.strip_prefix("service-") {
            return CallerIdentity::Machine(service.to_string());
        }
        // Unknown CN format — treat as user
        return CallerIdentity::User(cn.to_string());
    }

    // TODO: Authorization: Bearer <api_key> → KV lookup for machine-as-user

    CallerIdentity::Anonymous
}

// ---------------------------------------------------------------------------
// JSON-RPC types
// ---------------------------------------------------------------------------

#[derive(Debug, Deserialize)]
struct JsonRpcRequest {
    jsonrpc: String,
    id: Option<Value>,
    method: String,
    #[serde(default)]
    params: Value,
}

#[derive(Debug, Serialize)]
struct JsonRpcResponse {
    jsonrpc: String,
    id: Value,
    #[serde(skip_serializing_if = "Option::is_none")]
    result: Option<Value>,
    #[serde(skip_serializing_if = "Option::is_none")]
    error: Option<JsonRpcError>,
}

#[derive(Debug, Serialize)]
struct JsonRpcError {
    code: i32,
    message: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    data: Option<Value>,
}

impl JsonRpcResponse {
    fn success(id: Value, result: Value) -> Self {
        JsonRpcResponse {
            jsonrpc: "2.0".to_string(),
            id,
            result: Some(result),
            error: None,
        }
    }

    fn error(id: Value, code: i32, message: String) -> Self {
        JsonRpcResponse {
            jsonrpc: "2.0".to_string(),
            id,
            result: None,
            error: Some(JsonRpcError {
                code,
                message,
                data: None,
            }),
        }
    }

    fn method_not_found(id: Value, method: &str) -> Self {
        Self::error(id, -32601, format!("Method not found: {method}"))
    }
}

// ---------------------------------------------------------------------------
// Message handling
// ---------------------------------------------------------------------------

fn handle_initialize(id: Value) -> JsonRpcResponse {
    JsonRpcResponse::success(
        id,
        json!({
            "protocolVersion": "2024-11-05",
            "capabilities": {
                "tools": {}
            },
            "serverInfo": {
                "name": "rosary",
                "version": env!("CARGO_PKG_VERSION"),
                "buildHash": env!("RSRY_BUILD_HASH")
            },
            "instructions": SERVER_INSTRUCTIONS
        }),
    )
}

/// Instructions injected into every Claude session via MCP initialize.
/// This is how rsry teaches new users/agents what it does and how to use it.
const SERVER_INSTRUCTIONS: &str = "\
Rosary (rsry) is an autonomous work orchestrator. It tracks work as **beads** \
(small, actionable items stored in Dolt databases per repo) and dispatches AI agents to complete them.\n\
\n\
## On session start\n\
- Run `rsry_status` to see open/in-progress/blocked bead counts\n\
- Run `rsry_list_beads` to see what needs doing\n\
\n\
## Before starting work\n\
- Search for existing beads with `rsry_bead_search` — don't duplicate work\n\
- If no bead exists for your task, create one with `rsry_bead_create`\n\
\n\
## During work\n\
- Comment progress with `rsry_bead_comment` — other agents and humans read these\n\
- Commit with `[bead-id] type(scope): description` format (Golden Rule 11)\n\
\n\
## When done\n\
- Close the bead with `rsry_bead_close` after tests pass and work is committed\n\
- If you can't finish, comment explaining what you tried — don't close incomplete beads\n\
\n\
## Key concepts\n\
- **Beads**: work items (bugs, tasks, features, epics) with file scopes for parallel dispatch\n\
- **Threads**: ordered groups of related beads\n\
- **Decades**: ADR-level groupings of threads\n\
- **Dispatch**: `rsry_dispatch` assigns an agent to work on a bead in an isolated workspace\n\
";

fn handle_tools_list(id: Value) -> JsonRpcResponse {
    JsonRpcResponse::success(id, tools::tool_definitions())
}

async fn handle_tools_call(
    id: Value,
    params: &Value,
    config_path: &str,
    pool: &RepoPool,
    backend: Option<&dyn BackendStore>,
    caller: &CallerIdentity,
    repo_cache: &crate::repo_cache::RepoCache,
) -> JsonRpcResponse {
    let name = params.get("name").and_then(|v| v.as_str()).unwrap_or("");

    let args = params.get("arguments").cloned().unwrap_or(json!({}));

    match handlers::call_tool(name, &args, config_path, pool, backend, caller, repo_cache).await {
        Ok(result) => JsonRpcResponse::success(
            id,
            json!({
                "content": [
                    {
                        "type": "text",
                        "text": serde_json::to_string_pretty(&result).unwrap_or_default()
                    }
                ]
            }),
        ),
        Err(e) => JsonRpcResponse::success(
            id,
            json!({
                "content": [
                    {
                        "type": "text",
                        "text": format!("Error: {e}")
                    }
                ],
                "isError": true
            }),
        ),
    }
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

/// Run the MCP server.
pub async fn run(transport: &str, port: u16) -> Result<()> {
    match transport {
        "stdio" => run_stdio(&crate::config::resolve_config_path()).await,
        "http" => run_http(&crate::config::resolve_config_path(), port).await,
        other => {
            anyhow::bail!("Unknown transport: {other}. Supported: stdio, http");
        }
    }
}

/// Run the MCP server over a Unix Domain Socket (capnp ToolCall/Result).
///
/// Entry point for the `rsry mcp --ipc-socket <path>` cluster invocation
/// declared in cloister's `cluster.capnp` (rosary-6371e3, ADR-0005
/// intra-cluster amendment).
pub async fn run_ipc(ipc_socket: &std::path::Path) -> Result<()> {
    ipc::run(ipc_socket, &crate::config::resolve_config_path()).await
}

/// Client-side single-shot call against an IPC server (smoke + ops).
///
/// Exposes `serve::ipc::call_once` to the binary so `rsry ipc-call` can
/// invoke it. Returns `(text, is_error)`; the binary picks an exit code
/// based on `is_error` and prints `text` verbatim.
pub async fn run_ipc_call(
    ipc_socket: &std::path::Path,
    tool: &str,
    args_json: &[u8],
) -> Result<(String, bool)> {
    ipc::call_once(ipc_socket, tool, args_json).await
}

// ---------------------------------------------------------------------------
// HTTP transport (MCP Streamable HTTP)
// ---------------------------------------------------------------------------

/// Shared state for the HTTP transport.
#[derive(Clone)]
pub(crate) struct AppState {
    pub pool: Arc<RepoPool>,
    pub config_path: Arc<str>,
    pub sessions: Arc<RwLock<HashSet<String>>>,
    /// Linear webhook signing secret (from config or env).
    pub webhook_secret: Option<Arc<str>>,
    /// GitHub webhook signing secret (from config or env).
    pub github_webhook_secret: Option<Arc<str>>,
    /// Backend store for cross-repo orchestrator state (pipeline, dispatches, linkage).
    /// None when `[backend]` is not configured — existing functionality is unaffected.
    pub backend: Option<Arc<dyn BackendStore>>,
    /// On-demand repo cache for wasteland remote dispatch.
    pub repo_cache: Arc<crate::repo_cache::RepoCache>,
}

/// Validate Origin header to prevent DNS rebinding attacks.
#[allow(clippy::result_large_err)]
fn validate_origin(
    headers: &axum::http::HeaderMap,
) -> std::result::Result<(), axum::response::Response> {
    use axum::response::IntoResponse;
    if let Some(origin) = headers.get("origin") {
        let o = origin.to_str().unwrap_or("");
        let extra_origins = std::env::var("RSRY_ALLOWED_ORIGINS").unwrap_or_default();
        let allowed = o.starts_with("http://localhost")
            || o.starts_with("http://127.0.0.1")
            || o.starts_with("https://localhost")
            || o.starts_with("https://127.0.0.1")
            || extra_origins
                .split(',')
                .any(|a| !a.trim().is_empty() && o.starts_with(a.trim()));
        if !allowed {
            return Err((axum::http::StatusCode::FORBIDDEN, "Origin not allowed").into_response());
        }
    }
    Ok(())
}

/// Validate Accept header includes both required MIME types.
#[allow(clippy::result_large_err)]
fn validate_accept(
    headers: &axum::http::HeaderMap,
) -> std::result::Result<(), axum::response::Response> {
    use axum::response::IntoResponse;
    let accept = headers
        .get("accept")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("");
    if !accept.contains("application/json") || !accept.contains("text/event-stream") {
        return Err((
            axum::http::StatusCode::NOT_ACCEPTABLE,
            "Accept must include application/json and text/event-stream",
        )
            .into_response());
    }
    Ok(())
}

/// Validate session ID is present and known.
async fn validate_session(
    headers: &axum::http::HeaderMap,
    sessions: &RwLock<HashSet<String>>,
) -> std::result::Result<(), axum::response::Response> {
    use axum::response::IntoResponse;
    let session_id = headers.get("mcp-session-id").and_then(|v| v.to_str().ok());
    match session_id {
        None => Err((
            axum::http::StatusCode::BAD_REQUEST,
            "Missing Mcp-Session-Id header",
        )
            .into_response()),
        Some(id) => {
            if sessions.read().await.contains(id) {
                Ok(())
            } else {
                Err((axum::http::StatusCode::NOT_FOUND, "Unknown session").into_response())
            }
        }
    }
}

/// Build a JSON response with optional extra header.
fn json_response(
    status: axum::http::StatusCode,
    response: &JsonRpcResponse,
    extra_header: Option<(&str, &str)>,
) -> axum::response::Response {
    use axum::response::IntoResponse;
    let body = serde_json::to_value(response).unwrap_or(Value::Null);
    let mut resp = (status, axum::Json(body)).into_response();
    if let Some((key, val)) = extra_header
        && let (Ok(k), Ok(v)) = (
            key.parse::<axum::http::header::HeaderName>(),
            val.parse::<axum::http::header::HeaderValue>(),
        )
    {
        resp.headers_mut().insert(k, v);
    }
    resp
}

/// POST /mcp — main JSON-RPC handler.
///
/// Edition 2024 changes `impl Future` lifetime capture, which can make
/// async handler futures `!Send`. The explicit return type ensures Send.
#[allow(clippy::manual_async_fn)]
fn handle_mcp_post(
    axum::extract::State(state): axum::extract::State<AppState>,
    headers: axum::http::HeaderMap,
    body: String,
) -> impl std::future::Future<Output = axum::response::Response> + Send {
    async move {
        use axum::response::IntoResponse;

        if let Err(resp) = validate_origin(&headers) {
            return resp;
        }
        if let Err(resp) = validate_accept(&headers) {
            return resp;
        }

        let request: JsonRpcRequest = match serde_json::from_str(&body) {
            Ok(r) => r,
            Err(e) => {
                let err = JsonRpcResponse::error(Value::Null, -32700, format!("Parse error: {e}"));
                return json_response(axum::http::StatusCode::OK, &err, None);
            }
        };

        if request.jsonrpc != "2.0" {
            if let Some(id) = request.id {
                let resp = JsonRpcResponse::error(
                    id,
                    -32600,
                    "Invalid Request: jsonrpc must be \"2.0\"".into(),
                );
                return json_response(axum::http::StatusCode::OK, &resp, None);
            }
            return axum::http::StatusCode::BAD_REQUEST.into_response();
        }

        // Notifications (no id) — accept silently
        if request.id.is_none() {
            return axum::http::StatusCode::ACCEPTED.into_response();
        }

        let id = request.id.unwrap();
        let is_initialize = request.method == "initialize";

        // Session validation (skip for initialize).
        // Split into two ifs intentionally — collapsing with `&&` makes the future !Send
        // in edition 2024 due to temporary lifetime capture rules.
        #[allow(clippy::collapsible_if)]
        if !is_initialize {
            if let Err(resp) = validate_session(&headers, &state.sessions).await {
                return resp;
            }
        }

        // Extract caller identity from CF cert headers or auth token
        let caller = extract_identity(&headers);

        let response = match request.method.as_str() {
            "initialize" => handle_initialize(id.clone()),
            "tools/list" => handle_tools_list(id),
            "tools/call" => {
                handle_tools_call(
                    id,
                    &request.params,
                    &state.config_path,
                    &state.pool,
                    state.backend.as_deref(),
                    &caller,
                    &state.repo_cache,
                )
                .await
            }
            _ => JsonRpcResponse::method_not_found(id, &request.method),
        };

        if is_initialize {
            let session_id = uuid::Uuid::new_v4().to_string();
            state.sessions.write().await.insert(session_id.clone());
            eprintln!("[rsry-mcp] new session: {session_id}");
            return json_response(
                axum::http::StatusCode::OK,
                &response,
                Some(("mcp-session-id", &session_id)),
            );
        }

        json_response(axum::http::StatusCode::OK, &response, None)
    } // async move
}

/// GET /mcp — SSE stream for server→client notifications (v1: not supported).
async fn handle_mcp_get() -> axum::http::StatusCode {
    axum::http::StatusCode::METHOD_NOT_ALLOWED
}

/// DELETE /mcp — terminate session.
async fn handle_mcp_delete(
    axum::extract::State(state): axum::extract::State<AppState>,
    headers: axum::http::HeaderMap,
) -> axum::http::StatusCode {
    let session_id = headers.get("mcp-session-id").and_then(|v| v.to_str().ok());
    match session_id {
        Some(id) => {
            if state.sessions.write().await.remove(id) {
                eprintln!("[rsry-mcp] session terminated: {id}");
                axum::http::StatusCode::OK
            } else {
                axum::http::StatusCode::NOT_FOUND
            }
        }
        None => axum::http::StatusCode::BAD_REQUEST,
    }
}

async fn run_http(config_path: &str, port: u16) -> Result<()> {
    let cfg = config::load(config_path)?;
    let webhook_secret = cfg
        .linear
        .as_ref()
        .and_then(|l| l.webhook_secret.clone())
        .or_else(|| std::env::var("LINEAR_WEBHOOK_SECRET").ok())
        .filter(|s| !s.is_empty());

    let github_webhook_secret = cfg
        .github
        .as_ref()
        .and_then(|g| g.webhook_secret.clone())
        .or_else(|| std::env::var("GITHUB_WEBHOOK_SECRET").ok())
        .filter(|s| !s.is_empty());

    // Connect backend store if [backend] is configured
    let backend = if let Some(ref backend_cfg) = cfg.backend {
        match backend_cfg.connect().await {
            Ok(b) => {
                eprintln!(
                    "[rsry-mcp] backend store connected ({})",
                    backend_cfg.path.display()
                );
                Some(Arc::from(b))
            }
            Err(e) => {
                eprintln!("[rsry-mcp] backend store unavailable, continuing without it: {e}");
                None
            }
        }
    } else {
        None
    };

    let pool = Arc::new(RepoPool::from_config(config_path).await?);

    // Schema migrations are handled at connect time by each BeadStore backend
    // (SQLite runs CREATE TABLE IF NOT EXISTS; Dolt runs its migration list).

    let state = AppState {
        pool: pool.clone(),
        config_path: Arc::from(config_path),
        sessions: Arc::new(RwLock::new(HashSet::new())),
        webhook_secret: webhook_secret.map(|s| Arc::from(s.as_str())),
        github_webhook_secret: github_webhook_secret.map(|s| Arc::from(s.as_str())),
        backend,
        repo_cache: Arc::new(crate::repo_cache::RepoCache::new()),
    };

    let app = axum::Router::new()
        .route("/", axum::routing::get(landing::handle_landing))
        .route(
            "/mcp",
            axum::routing::post(handle_mcp_post)
                .get(handle_mcp_get)
                .delete(handle_mcp_delete),
        )
        .route("/webhook", axum::routing::post(webhook::handle_webhook))
        .route(
            "/webhook/github",
            axum::routing::post(github_webhook::handle_github_webhook),
        )
        .layer(axum::extract::DefaultBodyLimit::max(1024 * 1024))
        .with_state(state);

    let bind = std::env::var("RSRY_BIND").unwrap_or_else(|_| "127.0.0.1".into());
    // SO_REUSEADDR: allow binding immediately after previous process exits.
    // Without this, launchd restarts hit "Address already in use" for ~30s.
    let addr: std::net::SocketAddr = format!("{bind}:{port}").parse()?;
    let socket = socket2::Socket::new(
        socket2::Domain::for_address(addr),
        socket2::Type::STREAM,
        None,
    )?;
    socket.set_reuse_address(true)?;
    socket.set_nonblocking(true)?;
    socket.bind(&addr.into())?;
    socket.listen(128)?;
    let listener = tokio::net::TcpListener::from_std(socket.into())?;
    eprintln!(
        "[rsry-mcp] HTTP server listening on http://{bind}:{port}/mcp ({} repos: {})",
        pool.len(),
        pool.repo_names().join(", ")
    );

    // Graceful shutdown on SIGTERM or SIGINT.
    // launchd sends SIGTERM before SIGKILL — without handling it, the port
    // stays bound until ExitTimeOut expires, causing "Address already in use".
    axum::serve(listener, app)
        .with_graceful_shutdown(shutdown_signal())
        .await?;
    Ok(())
}

async fn shutdown_signal() {
    let ctrl_c = tokio::signal::ctrl_c();
    #[cfg(unix)]
    {
        let mut sigterm = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
            .expect("SIGTERM handler");
        tokio::select! {
            _ = ctrl_c => eprintln!("[rsry-mcp] SIGINT received, shutting down"),
            _ = sigterm.recv() => eprintln!("[rsry-mcp] SIGTERM received, shutting down"),
        }
    }
    #[cfg(not(unix))]
    {
        ctrl_c.await.ok();
        eprintln!("[rsry-mcp] shutdown signal received");
    }
}

async fn run_stdio(config_path: &str) -> Result<()> {
    let stdin = tokio::io::stdin();
    let mut stdout = tokio::io::stdout();
    let reader = BufReader::new(stdin);
    let mut lines = reader.lines();

    // SIGHUP → graceful exit so Claude Code restarts with the new binary after `task install`
    let mut sighup = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::hangup())
        .context("registering SIGHUP handler")?;

    // Connect backend store if [backend] is configured
    let cfg = config::load(config_path).ok();
    let backend = if let Some(backend_cfg) = cfg.as_ref().and_then(|c| c.backend.as_ref()) {
        match backend_cfg.connect().await {
            Ok(b) => {
                eprintln!(
                    "[rsry-mcp] backend store connected ({})",
                    backend_cfg.path.display()
                );
                Some(Arc::from(b))
            }
            Err(e) => {
                eprintln!("[rsry-mcp] backend store unavailable, continuing without it: {e}");
                None
            }
        }
    } else {
        None
    };

    // Create connection pool and repo cache on startup — reused across all tool calls
    let pool = RepoPool::from_config(config_path).await?;
    let repo_cache = crate::repo_cache::RepoCache::new();
    eprintln!(
        "[rsry-mcp] server started (stdio transport, {} repos: {}, build {})",
        pool.len(),
        pool.repo_names().join(", "),
        env!("RSRY_BUILD_HASH"),
    );

    loop {
        let line = tokio::select! {
            result = lines.next_line() => {
                match result.context("reading stdin")? {
                    Some(line) => line,
                    None => break, // stdin closed
                }
            }
            _ = sighup.recv() => {
                eprintln!("[rsry-mcp] received SIGHUP, exiting for binary upgrade");
                break;
            }
        };
        let line = line.trim().to_string();
        if line.is_empty() {
            continue;
        }

        let request: JsonRpcRequest = match serde_json::from_str(&line) {
            Ok(r) => r,
            Err(e) => {
                let err_resp =
                    JsonRpcResponse::error(Value::Null, -32700, format!("Parse error: {e}"));
                let mut out = serde_json::to_string(&err_resp)?;
                out.push('\n');
                stdout.write_all(out.as_bytes()).await?;
                stdout.flush().await?;
                continue;
            }
        };

        // Validate jsonrpc version
        if request.jsonrpc != "2.0" {
            if let Some(id) = request.id {
                let resp = JsonRpcResponse::error(
                    id,
                    -32600,
                    "Invalid Request: jsonrpc must be \"2.0\"".to_string(),
                );
                let mut out = serde_json::to_string(&resp)?;
                out.push('\n');
                stdout.write_all(out.as_bytes()).await?;
                stdout.flush().await?;
            }
            continue;
        }

        // Notifications (no id) — handle but don't respond
        if request.id.is_none() {
            // "notifications/initialized" is expected after initialize — just acknowledge silently
            continue;
        }

        let id = request.id.unwrap();

        let response = match request.method.as_str() {
            "initialize" => handle_initialize(id),
            "tools/list" => handle_tools_list(id),
            "tools/call" => {
                handle_tools_call(
                    id,
                    &request.params,
                    config_path,
                    &pool,
                    backend.as_deref(),
                    &CallerIdentity::Anonymous,
                    &repo_cache,
                )
                .await
            }
            _ => JsonRpcResponse::method_not_found(id, &request.method),
        };

        let mut out = serde_json::to_string(&response)?;
        out.push('\n');
        stdout.write_all(out.as_bytes()).await?;
        stdout.flush().await?;
    }

    eprintln!("[rsry-mcp] stdin closed, shutting down");
    Ok(())
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn initialize_response_format() {
        let resp = handle_initialize(json!(1));
        let result = resp.result.unwrap();
        assert_eq!(result["protocolVersion"], "2024-11-05");
        assert!(result["capabilities"]["tools"].is_object());
        assert_eq!(result["serverInfo"]["name"], "rosary");
        assert!(
            result["serverInfo"]["buildHash"].is_string(),
            "serverInfo should include buildHash"
        );
    }

    #[test]
    fn tools_list_response_format() {
        let resp = handle_tools_list(json!(2));
        let result = resp.result.unwrap();
        assert!(result["tools"].is_array());
        assert!(result["tools"].as_array().unwrap().len() >= 15);
    }

    #[test]
    fn method_not_found_response() {
        let resp = JsonRpcResponse::method_not_found(json!(99), "bogus/method");
        assert!(resp.error.is_some());
        assert_eq!(resp.error.as_ref().unwrap().code, -32601);
        assert!(
            resp.error
                .as_ref()
                .unwrap()
                .message
                .contains("bogus/method")
        );
        assert!(resp.result.is_none());
    }

    #[test]
    fn parse_request_with_params() {
        let raw = r#"{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"rsry_list_beads","arguments":{"status":"open"}}}"#;
        let req: JsonRpcRequest = serde_json::from_str(raw).unwrap();
        assert_eq!(req.method, "tools/call");
        assert_eq!(req.params["name"], "rsry_list_beads");
        assert_eq!(req.params["arguments"]["status"], "open");
    }

    #[test]
    fn parse_notification_no_id() {
        let raw = r#"{"jsonrpc":"2.0","method":"notifications/initialized"}"#;
        let req: JsonRpcRequest = serde_json::from_str(raw).unwrap();
        assert!(req.id.is_none());
        assert_eq!(req.method, "notifications/initialized");
    }

    #[test]
    fn success_response_serialization() {
        let resp = JsonRpcResponse::success(json!(1), json!({"ok": true}));
        let serialized = serde_json::to_string(&resp).unwrap();
        let parsed: Value = serde_json::from_str(&serialized).unwrap();
        assert_eq!(parsed["jsonrpc"], "2.0");
        assert_eq!(parsed["id"], 1);
        assert!(parsed["result"]["ok"].as_bool().unwrap());
        // error field should be absent (skip_serializing_if)
        assert!(parsed.get("error").is_none());
    }

    #[test]
    fn error_response_serialization() {
        let resp = JsonRpcResponse::error(json!(2), -32600, "bad request".to_string());
        let serialized = serde_json::to_string(&resp).unwrap();
        let parsed: Value = serde_json::from_str(&serialized).unwrap();
        assert_eq!(parsed["jsonrpc"], "2.0");
        assert_eq!(parsed["id"], 2);
        assert_eq!(parsed["error"]["code"], -32600);
        assert_eq!(parsed["error"]["message"], "bad request");
        // result field should be absent
        assert!(parsed.get("result").is_none());
    }

    // --- HTTP transport tests ---

    #[test]
    fn validate_origin_allows_localhost() {
        let mut headers = axum::http::HeaderMap::new();
        headers.insert("origin", "http://localhost:3000".parse().unwrap());
        assert!(validate_origin(&headers).is_ok());

        headers.insert("origin", "http://127.0.0.1:8080".parse().unwrap());
        assert!(validate_origin(&headers).is_ok());
    }

    #[test]
    fn validate_origin_rejects_external() {
        let mut headers = axum::http::HeaderMap::new();
        headers.insert("origin", "http://evil.com".parse().unwrap());
        assert!(validate_origin(&headers).is_err());
    }

    #[test]
    fn validate_origin_allows_no_origin() {
        let headers = axum::http::HeaderMap::new();
        assert!(validate_origin(&headers).is_ok());
    }

    #[test]
    fn validate_origin_allows_extra_origins_from_env() {
        // SAFETY: test is single-threaded; no other code reads this env var concurrently
        unsafe {
            std::env::set_var(
                "RSRY_ALLOWED_ORIGINS",
                "https://mcp.rosary.bot,https://other.example.com",
            );
        }
        let mut headers = axum::http::HeaderMap::new();
        headers.insert("origin", "https://mcp.rosary.bot".parse().unwrap());
        assert!(validate_origin(&headers).is_ok());

        headers.insert("origin", "https://other.example.com/path".parse().unwrap());
        assert!(validate_origin(&headers).is_ok());

        headers.insert("origin", "https://evil.com".parse().unwrap());
        assert!(validate_origin(&headers).is_err());
        // SAFETY: test is single-threaded; no other code reads this env var concurrently
        unsafe {
            std::env::remove_var("RSRY_ALLOWED_ORIGINS");
        }
    }

    #[test]
    fn validate_accept_requires_both_types() {
        let mut headers = axum::http::HeaderMap::new();
        headers.insert(
            "accept",
            "application/json, text/event-stream".parse().unwrap(),
        );
        assert!(validate_accept(&headers).is_ok());

        headers.insert("accept", "application/json".parse().unwrap());
        assert!(validate_accept(&headers).is_err());

        headers.insert("accept", "text/event-stream".parse().unwrap());
        assert!(validate_accept(&headers).is_err());
    }

    #[tokio::test]
    async fn validate_session_rejects_missing() {
        let sessions = RwLock::new(HashSet::new());
        let headers = axum::http::HeaderMap::new();
        assert!(validate_session(&headers, &sessions).await.is_err());
    }

    #[tokio::test]
    async fn validate_session_rejects_unknown() {
        let sessions = RwLock::new(HashSet::new());
        let mut headers = axum::http::HeaderMap::new();
        headers.insert("mcp-session-id", "unknown-id".parse().unwrap());
        assert!(validate_session(&headers, &sessions).await.is_err());
    }

    #[tokio::test]
    async fn validate_session_accepts_known() {
        let sessions = RwLock::new(HashSet::from(["abc-123".to_string()]));
        let mut headers = axum::http::HeaderMap::new();
        headers.insert("mcp-session-id", "abc-123".parse().unwrap());
        assert!(validate_session(&headers, &sessions).await.is_ok());
    }

    #[tokio::test]
    async fn http_initialize_returns_session_id() {
        use axum::body::Body;
        use axum::http::Request;
        use tower::ServiceExt;

        let state = AppState {
            pool: Arc::new(RepoPool::empty()),
            config_path: Arc::from("test.toml"),
            sessions: Arc::new(RwLock::new(HashSet::new())),
            webhook_secret: None,
            github_webhook_secret: None,
            backend: None,
            repo_cache: Arc::new(crate::repo_cache::RepoCache::new()),
        };

        let app = axum::Router::new()
            .route("/mcp", axum::routing::post(handle_mcp_post))
            .with_state(state.clone());

        let body = r#"{"jsonrpc":"2.0","id":1,"method":"initialize"}"#;
        let req = Request::builder()
            .method("POST")
            .uri("/mcp")
            .header("content-type", "application/json")
            .header("accept", "application/json, text/event-stream")
            .body(Body::from(body))
            .unwrap();

        let resp = app.oneshot(req).await.unwrap();
        assert_eq!(resp.status(), axum::http::StatusCode::OK);
        assert!(resp.headers().get("mcp-session-id").is_some());

        let sessions = state.sessions.read().await;
        assert_eq!(sessions.len(), 1);
    }

    #[tokio::test]
    async fn http_tools_list_requires_session() {
        use axum::body::Body;
        use axum::http::Request;
        use tower::ServiceExt;

        let state = AppState {
            pool: Arc::new(RepoPool::empty()),
            config_path: Arc::from("test.toml"),
            sessions: Arc::new(RwLock::new(HashSet::new())),
            webhook_secret: None,
            github_webhook_secret: None,
            backend: None,
            repo_cache: Arc::new(crate::repo_cache::RepoCache::new()),
        };

        let app = axum::Router::new()
            .route("/mcp", axum::routing::post(handle_mcp_post))
            .with_state(state);

        let body = r#"{"jsonrpc":"2.0","id":2,"method":"tools/list"}"#;
        let req = Request::builder()
            .method("POST")
            .uri("/mcp")
            .header("content-type", "application/json")
            .header("accept", "application/json, text/event-stream")
            .body(Body::from(body))
            .unwrap();

        let resp = app.oneshot(req).await.unwrap();
        // No session ID → 400
        assert_eq!(resp.status(), axum::http::StatusCode::BAD_REQUEST);
    }

    #[tokio::test]
    async fn http_delete_terminates_session() {
        use axum::body::Body;
        use axum::http::Request;
        use tower::ServiceExt;

        let state = AppState {
            pool: Arc::new(RepoPool::empty()),
            config_path: Arc::from("test.toml"),
            sessions: Arc::new(RwLock::new(HashSet::from(["sess-1".to_string()]))),
            webhook_secret: None,
            github_webhook_secret: None,
            backend: None,
            repo_cache: Arc::new(crate::repo_cache::RepoCache::new()),
        };

        let app = axum::Router::new()
            .route(
                "/mcp",
                axum::routing::post(handle_mcp_post).delete(handle_mcp_delete),
            )
            .with_state(state.clone());

        let req = Request::builder()
            .method("DELETE")
            .uri("/mcp")
            .header("mcp-session-id", "sess-1")
            .body(Body::empty())
            .unwrap();

        let resp = app.oneshot(req).await.unwrap();
        assert_eq!(resp.status(), axum::http::StatusCode::OK);
        assert!(state.sessions.read().await.is_empty());
    }
}
