//! IPC transport: capnp ToolCall/ToolResult over Unix Domain Socket.
//!
//! Wire contract (cloister ADR-0005 intra-cluster amendment, rosary-6371e3):
//!
//! - Each connection carries a stream of capnp messages, one ToolCall per
//!   inbound message, one ToolResult per outbound message. Standard capnp
//!   segment-table framing — no Manifest envelope, no AEAD.
//! - We EMIT canonical capnp (single segment, unpacked) so cloister's
//!   hand-rolled TS decoder accepts our bytes.
//! - We ACCEPT any valid capnp on read — tolerant of segment shapes a peer
//!   might produce — and rely on schema-level field access for validation.
//! - `ToolCall.upstreamId` is informational only; we are the upstream.
//! - Result content is a single `text` variant carrying the JSON-stringified
//!   handler return value, matching the shape today's JSON-RPC transport
//!   already returns from `handle_tools_call`.
//! - Stale socket files are removed before bind and on graceful shutdown.
//!
//! Trust boundary is the filesystem: only processes the cluster operator
//! has granted access to `<ipc_socket>` can issue ToolCalls. No additional
//! authentication here.

use std::path::{Path, PathBuf};
use std::sync::Arc;

use anyhow::{Context, Result};
use capnp::message::{Builder, HeapAllocator, ReaderOptions};
use capnp_futures::serialize;
use tokio::net::{UnixListener, UnixStream};
use tokio::signal::unix::{SignalKind, signal};
use tokio_util::compat::{TokioAsyncReadCompatExt, TokioAsyncWriteCompatExt};

use super::{CallerIdentity, handlers};
use crate::cloister_capnp::{tool_call, tool_result};
use crate::config;
use crate::pool::RepoPool;
use crate::store::BackendStore;

/// Run the MCP server over a Unix Domain Socket.
///
/// Binds at `ipc_socket`, accepts connections, and serves capnp ToolCall →
/// ToolResult until SIGTERM or SIGHUP.
pub async fn run(ipc_socket: &Path, config_path: &str) -> Result<()> {
    // Discriminate "stale socket from prior crash" (safe to unlink) from
    // "another rosary instance is already serving here" (refuse to bind,
    // surface the conflict instead of silently hijacking the path).
    //
    // Probe by connecting first: ECONNREFUSED / ENOENT → previous process
    // died without unlinking → ours to clean up; success → live server, bail.
    if ipc_socket.exists() {
        match tokio::net::UnixStream::connect(ipc_socket).await {
            Ok(_) => anyhow::bail!(
                "refusing to bind: another process is already serving at {} — \
                 stop that instance first, or pick a different --ipc-socket path",
                ipc_socket.display()
            ),
            Err(e)
                if matches!(
                    e.kind(),
                    std::io::ErrorKind::ConnectionRefused | std::io::ErrorKind::NotFound,
                ) =>
            {
                // Stale socket — unlink and continue. Verify it's actually a
                // socket before removing so a misplaced regular file doesn't
                // get accidentally deleted.
                let meta = std::fs::symlink_metadata(ipc_socket).with_context(|| {
                    format!("stat'ing {} for stale-socket check", ipc_socket.display())
                })?;
                #[cfg(unix)]
                {
                    use std::os::unix::fs::FileTypeExt;
                    if !meta.file_type().is_socket() {
                        anyhow::bail!(
                            "refusing to remove non-socket file at {} — please clean it up manually",
                            ipc_socket.display()
                        );
                    }
                }
                let _ = meta; // suppress unused warning on non-unix
                std::fs::remove_file(ipc_socket).with_context(|| {
                    format!("removing stale socket at {}", ipc_socket.display())
                })?;
            }
            Err(e) => {
                return Err(anyhow::anyhow!(e)).with_context(|| {
                    format!(
                        "probing existing path at {} (couldn't decide stale vs live)",
                        ipc_socket.display()
                    )
                });
            }
        }
    }

    let listener = UnixListener::bind(ipc_socket)
        .with_context(|| format!("binding UDS at {}", ipc_socket.display()))?;

    // Shared state (mirrors run_stdio). Backend is optional — if [backend] is
    // not configured, tools that need it will error per-call; the server still
    // serves the rest.
    let cfg = config::load(config_path).ok();
    let backend: Option<Arc<dyn BackendStore>> = match cfg.as_ref().and_then(|c| c.backend.as_ref())
    {
        Some(bc) => match bc.connect().await {
            Ok(b) => {
                eprintln!("[rsry-mcp] backend store connected ({})", bc.path.display());
                Some(Arc::from(b))
            }
            Err(e) => {
                eprintln!("[rsry-mcp] backend store unavailable, continuing without it: {e}");
                None
            }
        },
        None => None,
    };
    let pool = Arc::new(RepoPool::from_config(config_path).await?);
    let repo_cache = Arc::new(crate::repo_cache::RepoCache::new());
    let config_path: Arc<str> = Arc::from(config_path);

    eprintln!(
        "[rsry-mcp] server started (ipc-socket transport at {}, {} repos: {}, build {})",
        ipc_socket.display(),
        pool.len(),
        pool.repo_names().join(", "),
        env!("RSRY_BUILD_HASH"),
    );

    let mut sighup = signal(SignalKind::hangup()).context("registering SIGHUP")?;
    let mut sigterm = signal(SignalKind::terminate()).context("registering SIGTERM")?;
    let socket_path = PathBuf::from(ipc_socket);

    loop {
        tokio::select! {
            biased;
            _ = sighup.recv() => {
                eprintln!("[rsry-mcp] SIGHUP received, shutting down");
                break;
            }
            _ = sigterm.recv() => {
                eprintln!("[rsry-mcp] SIGTERM received, shutting down");
                break;
            }
            accepted = listener.accept() => {
                let (stream, _addr) = accepted.context("accepting on UDS")?;
                let pool = Arc::clone(&pool);
                let backend = backend.clone();
                let repo_cache = Arc::clone(&repo_cache);
                let config_path = Arc::clone(&config_path);
                tokio::spawn(async move {
                    if let Err(e) = serve_connection(
                        stream, pool, backend, repo_cache, config_path,
                    ).await {
                        eprintln!("[rsry-mcp] connection error: {e}");
                    }
                });
            }
        }
    }

    // Best-effort socket cleanup on shutdown. Don't propagate errors —
    // the unlink can race the OS or a packaging-cleanup hook.
    let _ = std::fs::remove_file(&socket_path);
    Ok(())
}

/// Client-side: connect to a rosary IPC server, send one `ToolCall`, read
/// the `ToolResult`. Returns `(text, is_error)` — the JSON-stringified
/// payload from the first `text` content variant plus the `isError` flag.
///
/// Intended for smoke tests (`task image:smoke`, the docker e2e test) and
/// operator debugging (`rsry ipc-call`). Production traffic goes through
/// cloister-companion, not this function.
pub async fn call_once(
    ipc_socket: &Path,
    tool_name: &str,
    args_json: &[u8],
) -> Result<(String, bool)> {
    let stream = tokio::net::UnixStream::connect(ipc_socket)
        .await
        .with_context(|| format!("connecting to UDS at {}", ipc_socket.display()))?;
    let (read_half, write_half) = stream.into_split();
    let mut read = read_half.compat();
    let mut write = write_half.compat_write();

    // Same canonical-encoding discipline as the server side — see
    // `encode_tool_result` for rationale.
    let mut staging = Builder::new_default();
    {
        let mut root = staging.init_root::<tool_call::Builder>();
        root.set_upstream_id("rosary");
        root.set_tool_name(tool_name);
        root.set_arguments_json(args_json);
    }
    let staging_reader = staging
        .get_root_as_reader::<tool_call::Reader>()
        .context("staging ToolCall reader")?;
    let mut builder = Builder::new_default();
    builder
        .set_root_canonical(staging_reader)
        .context("canonicalizing ToolCall")?;
    serialize::write_message(&mut write, builder)
        .await
        .context("writing ToolCall")?;

    let reader = serialize::try_read_message(&mut read, ReaderOptions::new())
        .await
        .context("reading ToolResult")?
        .ok_or_else(|| anyhow::anyhow!("server closed connection without responding"))?;

    let result: tool_result::Reader = reader
        .get_root::<tool_result::Reader>()
        .context("decoding ToolResult root")?;
    let is_error = result.get_is_error();
    let content_list = result.get_content().context("ToolResult.content")?;
    if content_list.is_empty() {
        anyhow::bail!("ToolResult.content is empty");
    }
    let body = content_list.get(0).get_body();
    let text = match body.which().context("Content.body union")? {
        crate::cloister_capnp::content::body::Which::Text(t) => t
            .context("Content.text bytes")?
            .to_str()
            .context("Content.text not utf-8")?
            .to_string(),
        crate::cloister_capnp::content::body::Which::Binary(_) => {
            anyhow::bail!("binary ToolResult content not supported by ipc-call")
        }
        crate::cloister_capnp::content::body::Which::Resource(_) => {
            anyhow::bail!("resource ToolResult content not supported by ipc-call")
        }
    };
    Ok((text, is_error))
}

async fn serve_connection(
    stream: UnixStream,
    pool: Arc<RepoPool>,
    backend: Option<Arc<dyn BackendStore>>,
    repo_cache: Arc<crate::repo_cache::RepoCache>,
    config_path: Arc<str>,
) -> Result<()> {
    let (read_half, write_half) = stream.into_split();
    let mut read = read_half.compat();
    let mut write = write_half.compat_write();

    loop {
        let reader = match serialize::try_read_message(&mut read, ReaderOptions::new()).await {
            Ok(Some(r)) => r,
            Ok(None) => return Ok(()), // peer closed
            Err(e) => return Err(anyhow::anyhow!("reading ToolCall: {e}")),
        };

        let (tool_name, args_value) = decode_tool_call(&reader)?;

        let result = handlers::call_tool(
            &tool_name,
            &args_value,
            &config_path,
            &pool,
            backend.as_deref(),
            &CallerIdentity::Anonymous,
            &repo_cache,
        )
        .await;

        let response = encode_tool_result(result)?;
        serialize::write_message(&mut write, response)
            .await
            .context("writing ToolResult")?;
    }
}

fn decode_tool_call(
    reader: &capnp::message::Reader<capnp::serialize::OwnedSegments>,
) -> Result<(String, serde_json::Value)> {
    let call: tool_call::Reader = reader
        .get_root::<tool_call::Reader>()
        .context("decoding ToolCall root")?;
    let tool_name = call
        .get_tool_name()
        .context("ToolCall.toolName")?
        .to_str()
        .context("ToolCall.toolName not utf-8")?
        .to_string();
    let args_bytes = call
        .get_arguments_json()
        .context("ToolCall.argumentsJson")?;
    let args_value: serde_json::Value = if args_bytes.is_empty() {
        serde_json::json!({})
    } else {
        serde_json::from_slice(args_bytes).context("ToolCall.argumentsJson not valid JSON")?
    };
    Ok((tool_name, args_value))
}

fn encode_tool_result(result: Result<serde_json::Value>) -> Result<Builder<HeapAllocator>> {
    let (text, is_error) = match result {
        Ok(value) => (
            serde_json::to_string(&value).unwrap_or_else(|_| "null".to_string()),
            false,
        ),
        Err(e) => (format!("Error: {e}"), true),
    };

    // Build the message field-by-field in a staging builder, then copy it
    // canonically into the output builder. `set_root_canonical` as the
    // first action on a fresh builder guarantees single-segment unpacked
    // output, which is what cloister's hand-rolled TS decoder requires
    // (see schemas/cloister.capnp canonicalization notes + ADR-0005).
    let mut staging = Builder::new_default();
    {
        let mut root = staging.init_root::<tool_result::Builder>();
        root.set_is_error(is_error);
        let mut content_list = root.init_content(1);
        let entry = content_list.reborrow().get(0);
        let mut body = entry.init_body();
        body.set_text(text.as_str());
    }
    let reader = staging
        .get_root_as_reader::<tool_result::Reader>()
        .context("staging ToolResult reader")?;
    let mut canonical = Builder::new_default();
    canonical
        .set_root_canonical(reader)
        .context("canonicalizing ToolResult")?;
    Ok(canonical)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::cloister_capnp::content;
    use capnp::message::Builder;
    use tokio::io::AsyncWriteExt;
    use tokio::net::UnixStream;

    /// Build a canonical capnp ToolCall and return the framed bytes.
    fn build_call(tool: &str, args_json: &[u8]) -> Vec<u8> {
        let mut builder = Builder::new_default();
        {
            let mut root = builder.init_root::<tool_call::Builder>();
            root.set_upstream_id("rosary");
            root.set_tool_name(tool);
            root.set_arguments_json(args_json);
        }
        let words = capnp::serialize::write_message_to_words(&builder);
        // write_message_to_words returns a Vec<u8> with the standard segment-table
        // framing already prepended — the same bytes a peer would read with
        // `serialize::try_read_message`.
        words
    }

    fn build_call_typed(tool: &str, args: serde_json::Value) -> Vec<u8> {
        let json = serde_json::to_vec(&args).unwrap();
        build_call(tool, &json)
    }

    #[tokio::test]
    async fn roundtrip_rsry_status() {
        // End-to-end: bind a UDS, spawn the server, write a ToolCall for
        // rsry_status (no args, fastest tool), read the ToolResult back.
        let dir = tempfile::tempdir().unwrap();
        let sock = dir.path().join("rsry.sock");
        let cfg_path = dir.path().join("rosary.toml");
        std::fs::write(&cfg_path, "").unwrap();

        let sock_for_server = sock.clone();
        let cfg_for_server = cfg_path.to_string_lossy().to_string();
        let server = tokio::spawn(async move {
            let _ = super::run(&sock_for_server, &cfg_for_server).await;
        });

        // Poll for socket to appear (server is async).
        for _ in 0..50 {
            if sock.exists() {
                break;
            }
            tokio::time::sleep(std::time::Duration::from_millis(20)).await;
        }
        assert!(sock.exists(), "server never bound socket");

        let mut client = UnixStream::connect(&sock).await.unwrap();
        let frame = build_call_typed("rsry_status", serde_json::json!({}));
        client.write_all(&frame).await.unwrap();

        let (read, _write) = client.into_split();
        let mut compat = read.compat();
        let reader = capnp_futures::serialize::try_read_message(&mut compat, ReaderOptions::new())
            .await
            .unwrap()
            .expect("ToolResult");

        let result: tool_result::Reader = reader.get_root().unwrap();
        let content_list = result.get_content().unwrap();
        assert_eq!(content_list.len(), 1, "exactly one content item");
        let body = content_list.get(0).get_body();
        match body.which().unwrap() {
            content::body::Which::Text(text) => {
                let s = text.unwrap().to_str().unwrap();
                // rsry_status returns a JSON object with at least `total`.
                let v: serde_json::Value = serde_json::from_str(s).expect("text payload is JSON");
                assert!(v.is_object(), "result is an object: {s}");
            }
            content::body::Which::Binary(_) => panic!("expected text content, got binary"),
            content::body::Which::Resource(_) => panic!("expected text content, got resource"),
        }

        server.abort();
    }
}
