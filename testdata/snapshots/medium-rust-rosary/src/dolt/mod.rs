//! Native MySQL client for Dolt-backed beads databases.
//!
//! Reads connection info from `.beads/dolt-server.port` and `.beads/metadata.json`,
//! then queries the Dolt server directly over MySQL wire protocol via sqlx.

mod bead_crud;
mod deps;
pub(crate) mod migrate;
#[allow(dead_code)] // API surface — wired in step 2 (reconciler integration)
pub(crate) mod observations;
mod query;
mod util;

#[cfg(test)]
mod tests;

use anyhow::{Context, Result};
use sqlx_core::pool::PoolOptions;
use sqlx_mysql::{MySql, MySqlPool};
use std::path::Path;

/// Connection details for a Dolt beads server.
#[derive(Debug, Clone)]
pub struct DoltConfig {
    pub host: String,
    pub port: u16,
    pub database: String,
    /// Path to the .beads/ directory (for auto-start + state files).
    pub beads_dir: std::path::PathBuf,
}

impl DoltConfig {
    /// Path to the Dolt database directory.
    pub fn dolt_dir(&self) -> std::path::PathBuf {
        self.beads_dir.join("dolt").join(&self.database)
    }

    /// Discover connection details from a repo's `.beads/` directory.
    pub fn from_beads_dir(beads_dir: &Path) -> Result<Self> {
        let port_file = beads_dir.join("dolt-server.port");
        let pid_file = beads_dir.join("dolt-server.pid");

        // Clean stale PID/port files before reading — a dead server's port file
        // causes a 10s timeout on every connect attempt.
        if pid_file.exists()
            && port_file.exists()
            && let Ok(pid_str) = std::fs::read_to_string(&pid_file)
            && let Ok(pid) = pid_str.trim().parse::<u32>()
            && !crate::session::is_pid_alive(pid)
        {
            eprintln!("[dolt] cleaning stale server files (pid {pid} dead)");
            let _ = std::fs::remove_file(&pid_file);
            let _ = std::fs::remove_file(&port_file);
            let _ = std::fs::remove_file(beads_dir.join("dolt-server.lock"));
        }

        let port: u16 = if port_file.exists() {
            let port_str = std::fs::read_to_string(&port_file)
                .with_context(|| format!("reading {}", port_file.display()))?;
            port_str
                .trim()
                .parse()
                .with_context(|| format!("parsing port from {}", port_file.display()))?
        } else {
            0 // No server running — connect() will auto-start
        };

        let meta_file = beads_dir.join("metadata.json");
        let database = if meta_file.exists() {
            let meta_str = std::fs::read_to_string(&meta_file)
                .with_context(|| format!("reading {}", meta_file.display()))?;
            let meta: serde_json::Value = serde_json::from_str(&meta_str)?;
            meta["dolt_database"]
                .as_str()
                .or_else(|| meta["database"].as_str())
                .unwrap_or("beads")
                .to_string()
        } else {
            "beads".to_string()
        };

        Ok(DoltConfig {
            host: "127.0.0.1".to_string(),
            port,
            database,
            beads_dir: beads_dir.to_path_buf(),
        })
    }

    /// Build a MySQL connection URL.
    pub fn url(&self) -> String {
        format!("mysql://root@{}:{}/{}", self.host, self.port, self.database)
    }
}

/// Build a connection pool with timeouts that prevent MCP server hangs.
///
/// Without these, a hung Dolt query blocks the entire stdio MCP server
/// (single-threaded), freezing the Claude Code UI including statusline.
fn pool_options() -> PoolOptions<MySql> {
    PoolOptions::<MySql>::new()
        // How long to wait for a connection from the pool before erroring.
        .acquire_timeout(std::time::Duration::from_secs(5))
        // Close idle connections after 5 minutes (prevents stale connections).
        .idle_timeout(Some(std::time::Duration::from_secs(300)))
        // Small pool: multiple rsry processes may share the same Dolt server
        // (e.g., MCP stdio + HTTP + agent-spawned MCP). Each needs its own
        // connection. dolt_transaction_commit is set per-connection at
        // connect time via enable_auto_dolt_commit().
        .max_connections(3)
}

/// Initialize a fresh Dolt database directory by running `dolt init`.
///
/// Shared by `init_beads_db` (per-repo `.beads/`) and the orchestrator
/// backend in `store_dolt.rs`. Both paths previously had their own copy
/// of this logic that swallowed dolt's stderr, skipped the identity
/// preflight, and left a half-initialized directory behind on failure —
/// the worst combination because the partial directory made later
/// commands behave as if init had succeeded.
///
/// Behavior:
///   1. Pre-flight `dolt config --global --get user.{name,email}`. If
///      either is unset, bail with an actionable hint before touching
///      the filesystem (`dolt init` would otherwise fail with
///      `fatal: empty ident name not allowed`).
///   2. `mkdir -p <dir>` then run `dolt init` with stdout+stderr
///      captured.
///   3. On non-zero exit, `rm -rf <dir>` so a retry sees a clean slate,
///      and surface dolt's stdout/stderr verbatim in the error.
pub async fn dolt_init_dir(dir: &Path) -> Result<()> {
    for key in ["user.name", "user.email"] {
        let out = tokio::process::Command::new("dolt")
            .args(["config", "--global", "--get", key])
            .output()
            .await
            .context("running `dolt config` — is dolt installed?")?;
        if !out.status.success() {
            anyhow::bail!(
                "dolt has no global {key} set — `dolt init` would abort.\n\
                 Fix:\n  \
                 dolt config --global --add user.email \"you@example.com\"\n  \
                 dolt config --global --add user.name  \"Your Name\""
            );
        }
    }

    std::fs::create_dir_all(dir).with_context(|| format!("creating {}", dir.display()))?;

    let output = tokio::process::Command::new("dolt")
        .args(["init"])
        .current_dir(dir)
        .output()
        .await
        .context("running dolt init")?;
    if !output.status.success() {
        // Roll back so a retry can recreate cleanly. Without this, the
        // next caller finds an existing directory and skips `dolt init`,
        // then breaks on lookups that assume the database was populated.
        let _ = std::fs::remove_dir_all(dir);
        let stderr = String::from_utf8_lossy(&output.stderr);
        let stdout = String::from_utf8_lossy(&output.stdout);
        anyhow::bail!(
            "dolt init failed in {} (exit {}):\n--- stderr ---\n{}--- stdout ---\n{}",
            dir.display(),
            output.status.code().unwrap_or(-1),
            stderr,
            stdout
        );
    }
    Ok(())
}

/// Initialize a `.beads/` directory with Dolt database and schema.
/// Called by `rsry enable` when a repo has no `.beads/` yet.
pub async fn init_beads_db(repo_path: &Path) -> Result<()> {
    let beads_dir = repo_path.join(".beads");
    let db_name = repo_path
        .file_name()
        .map(|n| n.to_string_lossy().to_string())
        .unwrap_or_else(|| "beads".into());
    let db_dir = beads_dir.join("dolt").join(&db_name);

    if beads_dir.exists() {
        let config = DoltConfig::from_beads_dir(&beads_dir)?;
        if let Ok(_client) = DoltClient::connect(&config).await {
            eprintln!("[dolt] .beads/ already initialized for {db_name}");
            return Ok(());
        }
    }

    dolt_init_dir(&db_dir).await?;

    std::fs::write(
        beads_dir.join("metadata.json"),
        format!(r#"{{"dolt_database": "{db_name}"}}"#),
    )?;

    let config = DoltConfig::from_beads_dir(&beads_dir)?;
    let client = DoltClient::connect(&config).await?;

    for sql in BEADS_SCHEMA {
        client.execute_raw(sql).await?;
    }

    client
        .execute_raw("CALL DOLT_COMMIT('-Am', 'init schema', '--allow-empty')")
        .await?;

    eprintln!("[dolt] initialized .beads/ for {db_name}");
    Ok(())
}

/// Schema for a beads database — used by init and tests.
const BEADS_SCHEMA: &[&str] = &[
    "CREATE TABLE IF NOT EXISTS issues (
        id VARCHAR(128) PRIMARY KEY,
        title VARCHAR(512) NOT NULL,
        description TEXT,
        design TEXT DEFAULT '',
        acceptance_criteria TEXT DEFAULT '',
        notes TEXT DEFAULT '',
        status VARCHAR(32) NOT NULL DEFAULT 'open',
        priority INT NOT NULL DEFAULT 2,
        issue_type VARCHAR(32) NOT NULL DEFAULT 'task',
        assignee VARCHAR(128),
        external_ref VARCHAR(128),
        user_id VARCHAR(128),
        created_by VARCHAR(255),
        scope VARCHAR(255) NOT NULL DEFAULT '',
        created_at DATETIME NOT NULL,
        updated_at DATETIME NOT NULL
    )",
    // `id` is the stable comment_id used by update/delete (rosary-a96b06).
    // Char(36) UUID with default uuid() — Dolt-native function.
    "CREATE TABLE IF NOT EXISTS comments (
        id CHAR(36) NOT NULL DEFAULT (uuid()) PRIMARY KEY,
        issue_id VARCHAR(128) NOT NULL,
        text TEXT NOT NULL,
        author VARCHAR(128) NOT NULL,
        created_at DATETIME NOT NULL,
        edited_at DATETIME NULL,
        edit_reason TEXT NULL,
        original_text TEXT NULL,
        deleted_at DATETIME NULL,
        delete_reason TEXT NULL
    )",
    "CREATE TABLE IF NOT EXISTS dependencies (
        issue_id VARCHAR(128) NOT NULL,
        depends_on_id VARCHAR(128) NOT NULL,
        PRIMARY KEY (issue_id, depends_on_id)
    )",
    "CREATE TABLE IF NOT EXISTS events (
        issue_id VARCHAR(128) NOT NULL,
        event_type VARCHAR(64) NOT NULL,
        actor VARCHAR(128) NOT NULL,
        comment TEXT,
        created_at DATETIME NOT NULL
    )",
];

/// Client for querying beads from a Dolt server.
pub struct DoltClient {
    pool: MySqlPool,
}

impl DoltClient {
    /// Connect to a Dolt server, auto-starting if not running.
    ///
    /// Follows the same pattern as beads' `EnsureRunning()`:
    /// 1. Try connecting (3s timeout)
    /// 2. If fails, start `dolt sql-server` from the db directory
    /// 3. Wait for it to accept connections
    /// 4. Retry the MySQL connection
    pub async fn connect(config: &DoltConfig) -> Result<Self> {
        // If a port file specified a non-zero port, the server should be running.
        // Use a longer timeout (10s) and don't auto-start — connecting to a
        // fresh empty server instead of the existing one causes silent data loss.
        let has_known_server = config.port > 0;
        let connect_timeout = if has_known_server { 10 } else { 3 };

        if let Ok(Ok(pool)) = tokio::time::timeout(
            std::time::Duration::from_secs(connect_timeout),
            pool_options().connect(&config.url()),
        )
        .await
        {
            let client = DoltClient { pool };
            client.enable_auto_dolt_commit().await;
            return Ok(client);
        }

        // If we had a known server port but couldn't connect, error out
        // instead of auto-starting a fresh empty server.
        if has_known_server {
            anyhow::bail!(
                "Dolt server on port {} not responding ({}s timeout). \
                 Kill stale servers with: pkill -f 'dolt sql-server'",
                config.port,
                connect_timeout
            );
        }

        // No port file (port=0) — auto-start from the dolt data directory
        let dolt_dir = config.dolt_dir();
        if !dolt_dir.exists() {
            anyhow::bail!(
                "Dolt database not initialized for this repo.\n\
                 Expected database at: {}\n\
                 \n\
                 To initialize, run:\n  rsry enable <repo-path>",
                dolt_dir.display()
            );
        }

        eprintln!(
            "[dolt] auto-starting server for {} on port {}...",
            config.database, config.port
        );

        // Allocate ephemeral port if configured port is 0
        let port = if config.port == 0 {
            let listener =
                std::net::TcpListener::bind("127.0.0.1:0").context("allocating ephemeral port")?;
            let port = listener.local_addr()?.port();
            drop(listener);
            port
        } else {
            config.port
        };

        // Start dolt sql-server as detached process
        let mut cmd = tokio::process::Command::new("dolt");
        cmd.args(["sql-server", "-H", "127.0.0.1", "-P", &port.to_string()]);
        cmd.current_dir(&dolt_dir);
        cmd.stdin(std::process::Stdio::null());
        cmd.stdout(std::process::Stdio::null());
        cmd.stderr(std::process::Stdio::null());

        let child = cmd.spawn().with_context(|| {
            format!(
                "starting dolt sql-server in {} (is dolt installed?)",
                dolt_dir.display()
            )
        })?;

        // Write PID + port files so bd/rsry can find this server later
        let beads_dir = &config.beads_dir;
        let _ = std::fs::write(
            beads_dir.join("dolt-server.pid"),
            child.id().unwrap_or(0).to_string(),
        );
        let _ = std::fs::write(beads_dir.join("dolt-server.port"), port.to_string());

        // Wait for server to accept connections (up to 10s)
        let addr = format!("127.0.0.1:{port}");
        let deadline = tokio::time::Instant::now() + std::time::Duration::from_secs(10);
        loop {
            if tokio::time::Instant::now() > deadline {
                anyhow::bail!(
                    "dolt sql-server started but not accepting connections on port {port}"
                );
            }
            if tokio::net::TcpStream::connect(&addr).await.is_ok() {
                break;
            }
            tokio::time::sleep(std::time::Duration::from_millis(500)).await;
        }

        // Connect via MySQL
        let url = format!("mysql://root@127.0.0.1:{port}/{}", config.database);
        let pool = tokio::time::timeout(
            std::time::Duration::from_secs(5),
            pool_options().connect(&url),
        )
        .await
        .with_context(|| format!("timeout connecting after auto-start on port {port}"))?
        .with_context(|| format!("connecting to Dolt at {url}"))?;

        eprintln!("[dolt] server started on port {port}");
        let client = DoltClient { pool };
        client.enable_auto_dolt_commit().await;
        Ok(client)
    }
}
