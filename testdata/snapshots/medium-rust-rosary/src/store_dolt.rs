//! Dolt-backed implementation of the backend store traits.
//!
//! Single struct implementing [`HierarchyStore`], [`DispatchStore`], and
//! [`LinkageStore`] against a Dolt MySQL database at `~/.rsry/dolt/rosary/`.
//!
//! Follows the same auto-start pattern as [`DoltClient::connect()`](crate::dolt::DoltClient):
//! check port file → start `dolt sql-server` if needed → wait for TCP → connect.

use anyhow::{Context, Result};
use async_trait::async_trait;
use sqlx_core::query::query;
use sqlx_core::row::Row;
use sqlx_mysql::MySqlPool;
use std::path::{Path, PathBuf};

use crate::config::BackendConfig;
use crate::store::*;

/// Dolt-backed orchestrator state store.
pub struct DoltBackend {
    pool: MySqlPool,
}

impl DoltBackend {
    /// Connect to the rosary backend Dolt server, auto-starting if needed.
    ///
    /// If the database directory doesn't exist, initializes it with `dolt init`.
    /// After connecting, runs `ensure_schema()` to create tables idempotently.
    pub async fn connect(config: &BackendConfig) -> Result<Self> {
        let data_dir = expand_path(&config.path);
        let database = data_dir
            .file_name()
            .map(|n| n.to_string_lossy().to_string())
            .unwrap_or_else(|| "rosary".to_string());

        // Ensure the database directory exists and is initialized.
        // Delegates to the shared helper so this site can't drift from the
        // per-repo `.beads/` init path — both preflight the dolt identity,
        // capture stdout+stderr, and roll back a half-init on failure.
        if !data_dir.join(".dolt").exists() {
            crate::dolt::dolt_init_dir(&data_dir)
                .await
                .with_context(|| {
                    format!("initializing rosary backend at {}", data_dir.display())
                })?;
        }

        // State files live next to the database dir
        let state_dir = data_dir.parent().unwrap_or(&data_dir);
        let port_file = state_dir.join("backend.port");
        let pid_file = state_dir.join("backend.pid");

        // Read port if server already running
        let existing_port: u16 = if port_file.exists() {
            std::fs::read_to_string(&port_file)
                .ok()
                .and_then(|s| s.trim().parse().ok())
                .unwrap_or(0)
        } else {
            0
        };

        // Fast path — try connecting to existing server
        if existing_port > 0 {
            let url = format!("mysql://root@127.0.0.1:{existing_port}/{database}");
            if let Ok(Ok(pool)) =
                tokio::time::timeout(std::time::Duration::from_secs(3), MySqlPool::connect(&url))
                    .await
            {
                let backend = Self { pool };
                backend.ensure_schema().await?;
                return Ok(backend);
            }
        }

        // Auto-start server
        eprintln!("[backend] auto-starting Dolt server for rosary backend...");

        let port = {
            let listener = std::net::TcpListener::bind("127.0.0.1:0")
                .context("allocating ephemeral port for backend")?;
            let p = listener.local_addr()?.port();
            drop(listener);
            p
        };

        let mut cmd = tokio::process::Command::new("dolt");
        cmd.args(["sql-server", "-H", "127.0.0.1", "-P", &port.to_string()]);
        cmd.current_dir(&data_dir);
        cmd.stdin(std::process::Stdio::null());
        cmd.stdout(std::process::Stdio::null());
        cmd.stderr(std::process::Stdio::null());

        let child = cmd.spawn().with_context(|| {
            format!(
                "starting dolt sql-server in {} (is dolt installed?)",
                data_dir.display()
            )
        })?;

        let _ = std::fs::write(&pid_file, child.id().unwrap_or(0).to_string());
        let _ = std::fs::write(&port_file, port.to_string());

        // Wait for server to accept connections (up to 10s)
        let addr = format!("127.0.0.1:{port}");
        let deadline = tokio::time::Instant::now() + std::time::Duration::from_secs(10);
        loop {
            if tokio::time::Instant::now() > deadline {
                anyhow::bail!(
                    "backend Dolt server not accepting connections on port {port} after 10s"
                );
            }
            if tokio::net::TcpStream::connect(&addr).await.is_ok() {
                break;
            }
            tokio::time::sleep(std::time::Duration::from_millis(500)).await;
        }

        let url = format!("mysql://root@127.0.0.1:{port}/{database}");
        let pool =
            tokio::time::timeout(std::time::Duration::from_secs(5), MySqlPool::connect(&url))
                .await
                .with_context(|| format!("timeout connecting to backend Dolt on port {port}"))?
                .with_context(|| format!("connecting to backend Dolt at {url}"))?;

        eprintln!("[backend] Dolt server started on port {port}");

        let backend = Self { pool };
        backend.ensure_schema().await?;
        Ok(backend)
    }

    /// Create all backend tables idempotently.
    async fn ensure_schema(&self) -> Result<()> {
        let statements = [
            "CREATE TABLE IF NOT EXISTS decades (
                id VARCHAR(128) PRIMARY KEY,
                title VARCHAR(512) NOT NULL,
                source_path VARCHAR(1024) NOT NULL,
                status VARCHAR(32) NOT NULL DEFAULT 'proposed',
                created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
                updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
            )",
            "CREATE TABLE IF NOT EXISTS threads (
                id VARCHAR(256) PRIMARY KEY,
                name VARCHAR(512) NOT NULL,
                decade_id VARCHAR(128) NOT NULL,
                feature_branch VARCHAR(256),
                created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
            )",
            "CREATE TABLE IF NOT EXISTS thread_members (
                thread_id VARCHAR(256) NOT NULL,
                repo VARCHAR(128) NOT NULL,
                bead_id VARCHAR(128) NOT NULL,
                scope VARCHAR(255) NOT NULL DEFAULT '',
                ordinal INT UNSIGNED NOT NULL DEFAULT 0,
                PRIMARY KEY (thread_id, repo, bead_id)
            )",
            "CREATE TABLE IF NOT EXISTS pipeline_state (
                repo VARCHAR(128) NOT NULL,
                bead_id VARCHAR(128) NOT NULL,
                pipeline_phase TINYINT UNSIGNED NOT NULL DEFAULT 0,
                pipeline_agent VARCHAR(64) NOT NULL,
                phase_status VARCHAR(32) NOT NULL DEFAULT 'pending',
                retries INT UNSIGNED NOT NULL DEFAULT 0,
                consecutive_reverts INT UNSIGNED NOT NULL DEFAULT 0,
                highest_verify_tier TINYINT UNSIGNED,
                last_generation BIGINT UNSIGNED NOT NULL DEFAULT 0,
                backoff_until DATETIME,
                updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
                PRIMARY KEY (repo, bead_id)
            )",
            "CREATE TABLE IF NOT EXISTS dispatches (
                id VARCHAR(64) PRIMARY KEY,
                repo VARCHAR(128) NOT NULL,
                bead_id VARCHAR(128) NOT NULL,
                agent VARCHAR(64) NOT NULL,
                provider VARCHAR(32) NOT NULL,
                started_at DATETIME NOT NULL,
                completed_at DATETIME,
                outcome VARCHAR(32),
                work_dir VARCHAR(1024),
                session_id VARCHAR(256),
                workspace_path VARCHAR(1024),
                chain_hash VARCHAR(64),
                INDEX idx_bead (repo, bead_id),
                INDEX idx_active (completed_at)
            )",
            "CREATE TABLE IF NOT EXISTS cross_repo_deps (
                from_repo VARCHAR(128) NOT NULL,
                from_bead VARCHAR(128) NOT NULL,
                to_repo VARCHAR(128) NOT NULL,
                to_bead VARCHAR(128) NOT NULL,
                dep_type VARCHAR(32) NOT NULL DEFAULT 'blocks',
                evidence_tier ENUM('asserted','derived','conjectured') NOT NULL DEFAULT 'asserted',
                source TEXT NOT NULL DEFAULT 'human',
                observed_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
                PRIMARY KEY (from_repo, from_bead, to_repo, to_bead)
            )",
            "CREATE TABLE IF NOT EXISTS linear_links (
                repo VARCHAR(128) NOT NULL,
                bead_id VARCHAR(128) NOT NULL,
                linear_id VARCHAR(32) NOT NULL,
                linear_type VARCHAR(32) NOT NULL DEFAULT 'issue',
                PRIMARY KEY (repo, bead_id),
                UNIQUE INDEX idx_linear_id (linear_id)
            )",
            "CREATE TABLE IF NOT EXISTS user_repos (
                user_id VARCHAR(128) NOT NULL,
                repo_url VARCHAR(1024) NOT NULL,
                repo_name VARCHAR(128) NOT NULL,
                github_token_ref VARCHAR(256),
                registered_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
                last_used DATETIME,
                PRIMARY KEY (user_id, repo_name)
            )",
        ];

        for sql in statements {
            query(sql)
                .execute(&self.pool)
                .await
                .with_context(|| format!("creating schema: {}", &sql[..sql.len().min(60)]))?;
        }

        // Additive migrations — only run if the column doesn't already exist.
        // Attempting ALTER TABLE on an existing column causes a DDL error that
        // can corrupt sqlx connection pool state even when silently discarded.
        let migrations: &[(&str, &str, &str)] = &[
            (
                "dispatches",
                "chain_hash",
                "ALTER TABLE dispatches ADD COLUMN chain_hash VARCHAR(64)",
            ),
            (
                "thread_members",
                "scope",
                "ALTER TABLE thread_members ADD COLUMN scope VARCHAR(255) NOT NULL DEFAULT ''",
            ),
            // ADR-0009: modal evidence tier columns on cross_repo_deps
            (
                "cross_repo_deps",
                "evidence_tier",
                "ALTER TABLE cross_repo_deps ADD COLUMN evidence_tier ENUM('asserted','derived','conjectured') NOT NULL DEFAULT 'asserted'",
            ),
            (
                "cross_repo_deps",
                "source",
                "ALTER TABLE cross_repo_deps ADD COLUMN source TEXT NOT NULL DEFAULT 'human'",
            ),
            (
                "cross_repo_deps",
                "observed_at",
                "ALTER TABLE cross_repo_deps ADD COLUMN observed_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP",
            ),
        ];
        for (table, column, sql) in migrations {
            let exists = query(
                "SELECT COUNT(*) FROM information_schema.columns \
                 WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?",
            )
            .bind(table)
            .bind(column)
            .fetch_one(&self.pool)
            .await
            .map(|r| r.try_get::<i64, _>(0).unwrap_or(0) > 0)
            .unwrap_or(true); // default true = skip, safe fallback
            if !exists && let Err(e) = query(sql).execute(&self.pool).await {
                eprintln!("[backend] migration warning ({table}.{column}): {e}");
            }
        }

        Ok(())
    }
}

// ── HierarchyStore ──────────────────────────────────────

#[async_trait]
impl HierarchyStore for DoltBackend {
    async fn upsert_decade(&self, decade: &DecadeRecord) -> Result<()> {
        query(
            "INSERT INTO decades (id, title, source_path, status, created_at, updated_at)
             VALUES (?, ?, ?, ?, NOW(), NOW())
             ON DUPLICATE KEY UPDATE
               title = VALUES(title), source_path = VALUES(source_path),
               status = VALUES(status), updated_at = NOW()",
        )
        .bind(&decade.id)
        .bind(&decade.title)
        .bind(&decade.source_path)
        .bind(&decade.status)
        .execute(&self.pool)
        .await
        .with_context(|| format!("upserting decade {}", decade.id))?;
        Ok(())
    }

    async fn get_decade(&self, id: &str) -> Result<Option<DecadeRecord>> {
        let row = query("SELECT id, title, source_path, status FROM decades WHERE id = ?")
            .bind(id)
            .fetch_optional(&self.pool)
            .await
            .with_context(|| format!("getting decade {id}"))?;

        Ok(row.map(|r| DecadeRecord {
            id: r.get("id"),
            title: r.get("title"),
            source_path: r.get("source_path"),
            status: r.get("status"),
        }))
    }

    async fn list_decades(&self, status: Option<&str>) -> Result<Vec<DecadeRecord>> {
        let rows = match status {
            Some(s) => query(
                "SELECT id, title, source_path, status FROM decades WHERE status = ? ORDER BY id",
            )
            .bind(s)
            .fetch_all(&self.pool)
            .await?,
            None => {
                query("SELECT id, title, source_path, status FROM decades ORDER BY id")
                    .fetch_all(&self.pool)
                    .await?
            }
        };

        Ok(rows
            .iter()
            .map(|r| DecadeRecord {
                id: r.get("id"),
                title: r.get("title"),
                source_path: r.get("source_path"),
                status: r.get("status"),
            })
            .collect())
    }

    async fn upsert_thread(&self, thread: &ThreadRecord) -> Result<()> {
        query(
            "INSERT INTO threads (id, name, decade_id, feature_branch, created_at)
             VALUES (?, ?, ?, ?, NOW())
             ON DUPLICATE KEY UPDATE
               name = VALUES(name),
               decade_id = VALUES(decade_id),
               feature_branch = VALUES(feature_branch)",
        )
        .bind(&thread.id)
        .bind(&thread.name)
        .bind(&thread.decade_id)
        .bind(&thread.feature_branch)
        .execute(&self.pool)
        .await
        .with_context(|| format!("upserting thread {}", thread.id))?;
        Ok(())
    }

    async fn list_threads(&self, decade_id: &str) -> Result<Vec<ThreadRecord>> {
        let rows = query(
            "SELECT id, name, decade_id, feature_branch FROM threads WHERE decade_id = ? ORDER BY id",
        )
        .bind(decade_id)
        .fetch_all(&self.pool)
        .await
        .with_context(|| format!("listing threads for decade {decade_id}"))?;

        Ok(rows
            .iter()
            .map(|r| ThreadRecord {
                id: r.get("id"),
                name: r.get("name"),
                decade_id: r.get("decade_id"),
                feature_branch: r.try_get("feature_branch").ok(),
            })
            .collect())
    }

    async fn add_bead_to_thread(&self, thread_id: &str, bead: &WorkRef) -> Result<()> {
        // Insert with next ordinal, no-op on duplicate
        query(
            "INSERT INTO thread_members (thread_id, repo, scope, bead_id, ordinal)
             VALUES (?, ?, ?, ?, (
                 SELECT COALESCE(MAX(t.ordinal), 0) + 1
                 FROM (SELECT ordinal FROM thread_members WHERE thread_id = ?) t
             ))
             ON DUPLICATE KEY UPDATE thread_id = thread_id",
        )
        .bind(thread_id)
        .bind(&bead.repo)
        .bind(&bead.scope)
        .bind(&bead.bead_id)
        .bind(thread_id)
        .execute(&self.pool)
        .await
        .with_context(|| format!("adding bead to thread {thread_id}"))?;
        Ok(())
    }

    async fn list_beads_in_thread(&self, thread_id: &str) -> Result<Vec<WorkRef>> {
        let rows = query(
            "SELECT repo, scope, bead_id FROM thread_members WHERE thread_id = ? ORDER BY ordinal",
        )
        .bind(thread_id)
        .fetch_all(&self.pool)
        .await
        .with_context(|| format!("listing beads in thread {thread_id}"))?;

        Ok(rows
            .iter()
            .map(|r| WorkRef {
                repo: r.get("repo"),
                scope: r.try_get("scope").unwrap_or_default(),
                bead_id: r.get("bead_id"),
            })
            .collect())
    }

    async fn find_thread_for_bead(&self, bead: &WorkRef) -> Result<Option<String>> {
        let row = query(
            "SELECT thread_id FROM thread_members \
             WHERE repo = ? AND bead_id = ? AND scope = ? LIMIT 1",
        )
        .bind(&bead.repo)
        .bind(&bead.bead_id)
        .bind(&bead.scope)
        .fetch_optional(&self.pool)
        .await
        .with_context(|| format!("finding thread for bead {}/{}", bead.repo, bead.bead_id))?;

        Ok(row.map(|r| r.get("thread_id")))
    }
}

// ── DispatchStore ───────────────────────────────────────

#[async_trait]
impl DispatchStore for DoltBackend {
    async fn upsert_pipeline(&self, state: &PipelineState) -> Result<()> {
        let backoff_naive = state.backoff_until.map(|dt| dt.naive_utc());

        query(
            "INSERT INTO pipeline_state
               (repo, bead_id, pipeline_phase, pipeline_agent, phase_status,
                retries, consecutive_reverts, highest_verify_tier, last_generation,
                backoff_until, updated_at)
             VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW())
             ON DUPLICATE KEY UPDATE
               pipeline_phase = VALUES(pipeline_phase),
               pipeline_agent = VALUES(pipeline_agent),
               phase_status = VALUES(phase_status),
               retries = VALUES(retries),
               consecutive_reverts = VALUES(consecutive_reverts),
               highest_verify_tier = VALUES(highest_verify_tier),
               last_generation = VALUES(last_generation),
               backoff_until = VALUES(backoff_until),
               updated_at = NOW()",
        )
        .bind(&state.bead_ref.repo)
        .bind(&state.bead_ref.bead_id)
        .bind(state.pipeline_phase)
        .bind(&state.pipeline_agent)
        .bind(&state.phase_status)
        .bind(state.retries)
        .bind(state.consecutive_reverts)
        .bind(state.highest_verify_tier)
        .bind(state.last_generation)
        .bind(backoff_naive)
        .execute(&self.pool)
        .await
        .with_context(|| {
            format!(
                "upserting pipeline state for {}/{}",
                state.bead_ref.repo, state.bead_ref.bead_id
            )
        })?;
        Ok(())
    }

    async fn get_pipeline(&self, bead: &WorkRef) -> Result<Option<PipelineState>> {
        let row = query(
            "SELECT repo, bead_id, pipeline_phase, pipeline_agent, phase_status,
                    retries, consecutive_reverts, highest_verify_tier, last_generation, backoff_until
             FROM pipeline_state WHERE repo = ? AND bead_id = ?",
        )
        .bind(&bead.repo)
        .bind(&bead.bead_id)
        .fetch_optional(&self.pool)
        .await
        .with_context(|| format!("getting pipeline for {}/{}", bead.repo, bead.bead_id))?;

        Ok(row.map(|r| row_to_pipeline_state(&r)))
    }

    async fn list_active_pipelines(&self) -> Result<Vec<PipelineState>> {
        let rows = query(
            "SELECT repo, bead_id, pipeline_phase, pipeline_agent, phase_status,
                    retries, consecutive_reverts, highest_verify_tier, last_generation, backoff_until
             FROM pipeline_state",
        )
        .fetch_all(&self.pool)
        .await
        .context("listing active pipelines")?;

        Ok(rows.iter().map(row_to_pipeline_state).collect())
    }

    async fn clear_pipeline(&self, bead: &WorkRef) -> Result<()> {
        query("DELETE FROM pipeline_state WHERE repo = ? AND bead_id = ?")
            .bind(&bead.repo)
            .bind(&bead.bead_id)
            .execute(&self.pool)
            .await
            .with_context(|| format!("clearing pipeline for {}/{}", bead.repo, bead.bead_id))?;
        Ok(())
    }

    async fn record_dispatch(&self, record: &DispatchRecord) -> Result<()> {
        query(
            "INSERT INTO dispatches (id, repo, bead_id, agent, provider, started_at, work_dir, session_id, workspace_path, chain_hash)
             VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
        )
        .bind(&record.id)
        .bind(&record.bead_ref.repo)
        .bind(&record.bead_ref.bead_id)
        .bind(&record.agent)
        .bind(&record.provider)
        .bind(record.started_at.naive_utc())
        .bind(&record.work_dir)
        .bind(&record.session_id)
        .bind(&record.workspace_path)
        .bind(&record.chain_hash)
        .execute(&self.pool)
        .await
        .with_context(|| format!("recording dispatch {}", record.id))?;
        Ok(())
    }

    async fn complete_dispatch(&self, id: &str, outcome: &str) -> Result<()> {
        query("UPDATE dispatches SET completed_at = NOW(), outcome = ? WHERE id = ?")
            .bind(outcome)
            .bind(id)
            .execute(&self.pool)
            .await
            .with_context(|| format!("completing dispatch {id}"))?;
        Ok(())
    }

    async fn update_dispatch_session(&self, id: &str, session_id: &str) -> Result<()> {
        query("UPDATE dispatches SET session_id = ? WHERE id = ?")
            .bind(session_id)
            .bind(id)
            .execute(&self.pool)
            .await
            .with_context(|| format!("updating session_id for dispatch {id}"))?;
        Ok(())
    }

    async fn upsert_dispatch(&self, record: &DispatchRecord) -> Result<()> {
        query(
            "INSERT INTO dispatches (id, repo, bead_id, agent, provider, started_at, completed_at, outcome, work_dir, session_id, workspace_path, chain_hash)
             VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
             ON DUPLICATE KEY UPDATE
                 completed_at = VALUES(completed_at), outcome = VALUES(outcome),
                 session_id = VALUES(session_id), workspace_path = VALUES(workspace_path)",
        )
        .bind(&record.id)
        .bind(&record.bead_ref.repo)
        .bind(&record.bead_ref.bead_id)
        .bind(&record.agent)
        .bind(&record.provider)
        .bind(record.started_at.naive_utc())
        .bind(record.completed_at.map(|dt| dt.naive_utc()))
        .bind(&record.outcome)
        .bind(&record.work_dir)
        .bind(&record.session_id)
        .bind(&record.workspace_path)
        .bind(&record.chain_hash)
        .execute(&self.pool)
        .await
        .with_context(|| format!("upserting dispatch {}", record.id))?;
        Ok(())
    }

    async fn active_dispatches(&self) -> Result<Vec<DispatchRecord>> {
        let rows = query(
            "SELECT id, repo, bead_id, agent, provider, started_at,
                    completed_at, outcome, work_dir, session_id, workspace_path, chain_hash
             FROM dispatches WHERE completed_at IS NULL",
        )
        .fetch_all(&self.pool)
        .await
        .context("listing active dispatches")?;

        Ok(rows.iter().map(row_to_dispatch_record).collect())
    }
}

// ── LinkageStore ────────────────────────────────────────

#[async_trait]
impl LinkageStore for DoltBackend {
    async fn add_dependency(&self, dep: &CrossRepoDep) -> Result<()> {
        // Per-repo cycle check (ADR-0009): same-repo subgraphs must be DAGs.
        if dep.from.repo == dep.to.repo {
            let existing = query(
                "SELECT from_bead, to_bead FROM cross_repo_deps
                 WHERE from_repo = ? AND to_repo = ?",
            )
            .bind(&dep.from.repo)
            .bind(&dep.to.repo)
            .fetch_all(&self.pool)
            .await
            .unwrap_or_default();
            // DFS: can we reach dep.from from dep.to using existing edges?
            if dfs_can_reach(&existing, &dep.to.bead_id, &dep.from.bead_id) {
                anyhow::bail!(
                    "cycle: adding {}/{} → {}/{} would create a per-repo cycle",
                    dep.from.repo,
                    dep.from.bead_id,
                    dep.to.repo,
                    dep.to.bead_id
                );
            }
        }

        query(
            "INSERT INTO cross_repo_deps
               (from_repo, from_bead, to_repo, to_bead, dep_type, evidence_tier, source, observed_at)
             VALUES (?, ?, ?, ?, ?, ?, ?, ?)
             ON DUPLICATE KEY UPDATE
               dep_type = VALUES(dep_type),
               evidence_tier = VALUES(evidence_tier),
               source = VALUES(source),
               observed_at = VALUES(observed_at)",
        )
        .bind(&dep.from.repo)
        .bind(&dep.from.bead_id)
        .bind(&dep.to.repo)
        .bind(&dep.to.bead_id)
        .bind(&dep.dep_type)
        .bind(dep.evidence_tier.as_str())
        .bind(&dep.source)
        .bind(dep.observed_at)
        .execute(&self.pool)
        .await
        .with_context(|| {
            format!(
                "adding dependency {}/{}→{}/{}",
                dep.from.repo, dep.from.bead_id, dep.to.repo, dep.to.bead_id
            )
        })?;
        Ok(())
    }

    async fn dependencies_of(&self, bead: &WorkRef) -> Result<Vec<CrossRepoDep>> {
        let rows = query(
            "SELECT from_repo, from_bead, to_repo, to_bead, dep_type,
                    evidence_tier, source, observed_at
             FROM cross_repo_deps WHERE from_repo = ? AND from_bead = ?",
        )
        .bind(&bead.repo)
        .bind(&bead.bead_id)
        .fetch_all(&self.pool)
        .await
        .with_context(|| format!("dependencies of {}/{}", bead.repo, bead.bead_id))?;

        Ok(rows.iter().map(row_to_cross_repo_dep).collect())
    }

    async fn dependents_of(&self, bead: &WorkRef) -> Result<Vec<CrossRepoDep>> {
        let rows = query(
            "SELECT from_repo, from_bead, to_repo, to_bead, dep_type,
                    evidence_tier, source, observed_at
             FROM cross_repo_deps WHERE to_repo = ? AND to_bead = ?",
        )
        .bind(&bead.repo)
        .bind(&bead.bead_id)
        .fetch_all(&self.pool)
        .await
        .with_context(|| format!("dependents of {}/{}", bead.repo, bead.bead_id))?;

        Ok(rows.iter().map(row_to_cross_repo_dep).collect())
    }

    async fn all_blocking_deps(&self) -> Result<Vec<CrossRepoDep>> {
        let rows = query(
            "SELECT from_repo, from_bead, to_repo, to_bead, dep_type,
                    evidence_tier, source, observed_at
             FROM cross_repo_deps
             WHERE dep_type = 'blocks'
               AND evidence_tier IN ('asserted', 'derived')",
        )
        .fetch_all(&self.pool)
        .await
        .context("loading all blocking deps")?;

        Ok(rows.iter().map(row_to_cross_repo_dep).collect())
    }

    async fn remove_cross_repo_dep(&self, from: &WorkRef, to: &WorkRef) -> Result<()> {
        query(
            "DELETE FROM cross_repo_deps
             WHERE from_repo = ? AND from_bead = ? AND to_repo = ? AND to_bead = ?",
        )
        .bind(&from.repo)
        .bind(&from.bead_id)
        .bind(&to.repo)
        .bind(&to.bead_id)
        .execute(&self.pool)
        .await
        .context("removing cross-repo dep")?;
        Ok(())
    }

    async fn demote_stale_derived(&self, ttl_days: u32) -> Result<u64> {
        let result = query(
            "UPDATE cross_repo_deps
             SET evidence_tier = 'conjectured'
             WHERE evidence_tier = 'derived'
               AND observed_at < DATE_SUB(NOW(), INTERVAL ? DAY)",
        )
        .bind(i64::from(ttl_days))
        .execute(&self.pool)
        .await
        .context("demoting stale derived deps")?;

        Ok(result.rows_affected())
    }

    async fn upsert_linear_link(&self, link: &LinearLink) -> Result<()> {
        query(
            "INSERT INTO linear_links (repo, bead_id, linear_id, linear_type)
             VALUES (?, ?, ?, ?)
             ON DUPLICATE KEY UPDATE
               linear_id = VALUES(linear_id), linear_type = VALUES(linear_type)",
        )
        .bind(&link.bead_ref.repo)
        .bind(&link.bead_ref.bead_id)
        .bind(&link.linear_id)
        .bind(&link.linear_type)
        .execute(&self.pool)
        .await
        .with_context(|| format!("upserting linear link for {}", link.linear_id))?;
        Ok(())
    }

    async fn find_by_linear_id(&self, linear_id: &str) -> Result<Option<LinearLink>> {
        let row = query(
            "SELECT repo, bead_id, linear_id, linear_type FROM linear_links WHERE linear_id = ?",
        )
        .bind(linear_id)
        .fetch_optional(&self.pool)
        .await
        .with_context(|| format!("finding bead by linear ID {linear_id}"))?;

        Ok(row.map(|r| LinearLink {
            bead_ref: WorkRef {
                repo: r.get("repo"),
                scope: String::new(),
                bead_id: r.get("bead_id"),
            },
            linear_id: r.get("linear_id"),
            linear_type: r.get("linear_type"),
        }))
    }
}

#[async_trait]
impl UserRepoStore for DoltBackend {
    async fn register_repo(&self, repo: &UserRepo) -> Result<()> {
        query(
            "INSERT INTO user_repos (user_id, repo_url, repo_name, github_token_ref, registered_at)
             VALUES (?, ?, ?, ?, NOW())
             ON DUPLICATE KEY UPDATE repo_url = VALUES(repo_url), github_token_ref = VALUES(github_token_ref)",
        )
        .bind(&repo.user_id)
        .bind(&repo.repo_url)
        .bind(&repo.repo_name)
        .bind(&repo.github_token_ref)
        .execute(&self.pool)
        .await
        .with_context(|| format!("registering repo {} for {}", repo.repo_name, repo.user_id))?;
        Ok(())
    }

    async fn list_user_repos(&self, user_id: &str) -> Result<Vec<UserRepo>> {
        let rows = query("SELECT user_id, repo_url, repo_name, github_token_ref FROM user_repos WHERE user_id = ?")
            .bind(user_id)
            .fetch_all(&self.pool)
            .await
            .context("listing user repos")?;

        Ok(rows
            .iter()
            .map(|r| UserRepo {
                user_id: r.get("user_id"),
                repo_url: r.get("repo_url"),
                repo_name: r.get("repo_name"),
                github_token_ref: r.try_get("github_token_ref").ok(),
            })
            .collect())
    }

    async fn unregister_repo(&self, user_id: &str, repo_name: &str) -> Result<()> {
        query("DELETE FROM user_repos WHERE user_id = ? AND repo_name = ?")
            .bind(user_id)
            .bind(repo_name)
            .execute(&self.pool)
            .await
            .with_context(|| format!("unregistering {repo_name} for {user_id}"))?;
        Ok(())
    }
}

// ── Row conversion helpers ──────────────────────────────

fn row_to_pipeline_state(r: &sqlx_mysql::MySqlRow) -> PipelineState {
    let backoff_naive: Option<chrono::NaiveDateTime> = r.try_get("backoff_until").ok();
    PipelineState {
        bead_ref: WorkRef {
            repo: r.get("repo"),
            scope: String::new(),
            bead_id: r.get("bead_id"),
        },
        pipeline_phase: r.try_get::<u8, _>("pipeline_phase").unwrap_or(0),
        pipeline_agent: r.get("pipeline_agent"),
        phase_status: r
            .try_get("phase_status")
            .unwrap_or_else(|_| "pending".to_string()),
        retries: r.try_get::<u32, _>("retries").unwrap_or(0),
        consecutive_reverts: r.try_get::<u32, _>("consecutive_reverts").unwrap_or(0),
        highest_verify_tier: r.try_get::<u8, _>("highest_verify_tier").ok(),
        last_generation: r.try_get::<u64, _>("last_generation").unwrap_or(0),
        backoff_until: backoff_naive
            .map(|n| chrono::DateTime::<chrono::Utc>::from_naive_utc_and_offset(n, chrono::Utc)),
    }
}

fn row_to_dispatch_record(r: &sqlx_mysql::MySqlRow) -> DispatchRecord {
    let started_naive: chrono::NaiveDateTime = r.try_get("started_at").unwrap_or_default();
    let completed_naive: Option<chrono::NaiveDateTime> = r.try_get("completed_at").ok();

    DispatchRecord {
        id: r.get("id"),
        bead_ref: WorkRef {
            repo: r.get("repo"),
            scope: String::new(),
            bead_id: r.get("bead_id"),
        },
        agent: r.get("agent"),
        provider: r.get("provider"),
        started_at: chrono::DateTime::<chrono::Utc>::from_naive_utc_and_offset(
            started_naive,
            chrono::Utc,
        ),
        completed_at: completed_naive
            .map(|n| chrono::DateTime::<chrono::Utc>::from_naive_utc_and_offset(n, chrono::Utc)),
        outcome: r.try_get("outcome").ok(),
        work_dir: r.try_get("work_dir").unwrap_or_default(),
        session_id: r.try_get("session_id").ok().flatten(),
        workspace_path: r.try_get("workspace_path").ok().flatten(),
        chain_hash: r.try_get("chain_hash").ok().flatten(),
    }
}

fn row_to_cross_repo_dep(r: &sqlx_mysql::MySqlRow) -> CrossRepoDep {
    let tier_str: String = r.try_get("evidence_tier").unwrap_or_default();
    let observed_naive: Option<chrono::NaiveDateTime> = r.try_get("observed_at").ok();
    CrossRepoDep {
        from: WorkRef {
            repo: r.get("from_repo"),
            scope: String::new(),
            bead_id: r.get("from_bead"),
        },
        to: WorkRef {
            repo: r.get("to_repo"),
            scope: String::new(),
            bead_id: r.get("to_bead"),
        },
        dep_type: r.get("dep_type"),
        evidence_tier: tier_str
            .parse()
            .unwrap_or(crate::store::EvidenceTier::Asserted),
        source: r.try_get("source").unwrap_or_else(|_| "human".into()),
        observed_at: observed_naive
            .map(|n| chrono::DateTime::from_naive_utc_and_offset(n, chrono::Utc))
            .unwrap_or_else(chrono::Utc::now),
    }
}

/// DFS: can we reach `target_bead` starting from `start_bead` using the given
/// (from_bead, to_bead) pair rows from the same-repo subgraph?
fn dfs_can_reach(edges: &[sqlx_mysql::MySqlRow], start_bead: &str, target_bead: &str) -> bool {
    use std::collections::HashSet;
    let mut visited = HashSet::new();
    let mut stack = vec![start_bead.to_string()];
    while let Some(node) = stack.pop() {
        if node == target_bead {
            return true;
        }
        if !visited.insert(node.clone()) {
            continue;
        }
        for r in edges {
            let from: String = r.try_get("from_bead").unwrap_or_default();
            if from == node {
                let to: String = r.try_get("to_bead").unwrap_or_default();
                stack.push(to);
            }
        }
    }
    false
}

// ── BackendExport ──────────────────────────────────────

#[async_trait]
impl BackendExport for DoltBackend {
    async fn all_threads(&self) -> Result<Vec<ThreadRecord>> {
        let rows = query("SELECT id, name, decade_id, feature_branch FROM threads ORDER BY id")
            .fetch_all(&self.pool)
            .await
            .context("listing all threads")?;
        Ok(rows
            .iter()
            .map(|r| ThreadRecord {
                id: r.get("id"),
                name: r.get("name"),
                decade_id: r.get("decade_id"),
                feature_branch: r.try_get("feature_branch").ok(),
            })
            .collect())
    }

    async fn all_thread_members(&self) -> Result<Vec<(String, WorkRef)>> {
        let rows = query(
            "SELECT thread_id, repo, scope, bead_id FROM thread_members ORDER BY thread_id, ordinal",
        )
        .fetch_all(&self.pool)
        .await
        .context("listing all thread members")?;
        Ok(rows
            .iter()
            .map(|r| {
                (
                    r.get::<String, _>("thread_id"),
                    WorkRef {
                        repo: r.get("repo"),
                        scope: r.try_get("scope").unwrap_or_default(),
                        bead_id: r.get("bead_id"),
                    },
                )
            })
            .collect())
    }

    async fn all_dispatches(&self) -> Result<Vec<DispatchRecord>> {
        let rows = query(
            "SELECT id, repo, bead_id, agent, provider, started_at,
                    completed_at, outcome, work_dir, session_id, workspace_path, chain_hash
             FROM dispatches ORDER BY started_at ASC",
        )
        .fetch_all(&self.pool)
        .await
        .context("listing all dispatches")?;
        Ok(rows.iter().map(row_to_dispatch_record).collect())
    }

    async fn all_dependencies(&self) -> Result<Vec<CrossRepoDep>> {
        let rows = query(
            "SELECT from_repo, from_bead, to_repo, to_bead, dep_type,
                    evidence_tier, source, observed_at FROM cross_repo_deps",
        )
        .fetch_all(&self.pool)
        .await
        .context("listing all dependencies")?;
        Ok(rows.iter().map(row_to_cross_repo_dep).collect())
    }

    async fn all_linear_links(&self) -> Result<Vec<LinearLink>> {
        let rows = query("SELECT repo, bead_id, linear_id, linear_type FROM linear_links")
            .fetch_all(&self.pool)
            .await
            .context("listing all linear links")?;
        Ok(rows
            .iter()
            .map(|r| LinearLink {
                bead_ref: WorkRef {
                    repo: r.get("repo"),
                    scope: String::new(),
                    bead_id: r.get("bead_id"),
                },
                linear_id: r.get("linear_id"),
                linear_type: r.get("linear_type"),
            })
            .collect())
    }

    async fn all_user_repos(&self) -> Result<Vec<UserRepo>> {
        let rows = query("SELECT user_id, repo_url, repo_name, github_token_ref FROM user_repos")
            .fetch_all(&self.pool)
            .await
            .context("listing all user repos")?;
        Ok(rows
            .iter()
            .map(|r| UserRepo {
                user_id: r.get("user_id"),
                repo_url: r.get("repo_url"),
                repo_name: r.get("repo_name"),
                github_token_ref: r.try_get("github_token_ref").ok(),
            })
            .collect())
    }
}

/// Delegate to scanner::expand_path (single source of truth for path expansion).
fn expand_path(path: &Path) -> PathBuf {
    crate::scanner::expand_path(path)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn expand_tilde_path() {
        let path = PathBuf::from("~/.rsry/dolt/rosary");
        let expanded = expand_path(&path);
        assert!(!expanded.to_string_lossy().contains('~'));
        assert!(expanded.to_string_lossy().contains(".rsry/dolt/rosary"));
    }

    #[test]
    fn expand_absolute_path_unchanged() {
        let path = PathBuf::from("/tmp/test/rosary");
        let expanded = expand_path(&path);
        assert_eq!(expanded, path);
    }

    /// Integration test — creates a temp Dolt database, starts a server,
    /// and validates the full store lifecycle.
    ///
    /// Requires `dolt` to be installed. Automatically skips if not available.
    #[tokio::test]
    async fn dolt_backend_lifecycle() {
        // Check if dolt is installed
        // nosemgrep: blocking-subprocess-in-async — test availability probe, sync is fine in tests
        if std::process::Command::new("dolt")
            .arg("version")
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::null())
            .status()
            .is_err()
        {
            eprintln!("skipping: dolt not installed");
            return;
        }

        let tmp = tempfile::TempDir::new().unwrap();
        let db_dir = tmp.path().join("rosary");

        let config = BackendConfig {
            provider: "dolt".to_string(),
            path: db_dir.clone(),
        };

        // Connect (auto-inits + auto-starts)
        let backend = match DoltBackend::connect(&config).await {
            Ok(b) => b,
            Err(e) => {
                eprintln!("skipping: failed to connect to backend: {e}");
                return;
            }
        };

        // -- HierarchyStore --
        let decade = DecadeRecord {
            id: "ADR-003".into(),
            title: "Linear hierarchy mapping".into(),
            source_path: "docs/adr/0003.md".into(),
            status: "proposed".into(),
        };
        backend.upsert_decade(&decade).await.unwrap();

        let got = backend.get_decade("ADR-003").await.unwrap().unwrap();
        assert_eq!(got.title, "Linear hierarchy mapping");

        let thread = ThreadRecord {
            id: "ADR-003/impl".into(),
            name: "Implementation".into(),
            decade_id: "ADR-003".into(),
            feature_branch: Some("feat/hierarchy".into()),
        };
        backend.upsert_thread(&thread).await.unwrap();

        let threads = backend.list_threads("ADR-003").await.unwrap();
        assert_eq!(threads.len(), 1);

        let bead = WorkRef {
            repo: "rosary".into(),
            bead_id: "rsry-001".into(),
            scope: String::new(),
        };
        backend
            .add_bead_to_thread("ADR-003/impl", &bead)
            .await
            .unwrap();
        let members = backend.list_beads_in_thread("ADR-003/impl").await.unwrap();
        assert_eq!(members.len(), 1);
        assert_eq!(members[0], bead);

        let found = backend.find_thread_for_bead(&bead).await.unwrap();
        assert_eq!(found, Some("ADR-003/impl".into()));

        // -- DispatchStore --
        let pipeline = PipelineState {
            bead_ref: bead.clone(),
            pipeline_phase: 0,
            pipeline_agent: "dev-agent".into(),
            phase_status: "executing".into(),
            retries: 0,
            consecutive_reverts: 0,
            highest_verify_tier: None,
            last_generation: 42,
            backoff_until: None,
        };
        backend.upsert_pipeline(&pipeline).await.unwrap();

        let got = backend.get_pipeline(&bead).await.unwrap().unwrap();
        assert_eq!(got.last_generation, 42);
        assert_eq!(got.pipeline_agent, "dev-agent");

        let active = backend.list_active_pipelines().await.unwrap();
        assert_eq!(active.len(), 1);

        backend.clear_pipeline(&bead).await.unwrap();
        assert!(backend.get_pipeline(&bead).await.unwrap().is_none());

        let dispatch = DispatchRecord {
            id: "d-001".into(),
            bead_ref: bead.clone(),
            agent: "dev-agent".into(),
            provider: "claude".into(),
            started_at: chrono::Utc::now(),
            completed_at: None,
            outcome: None,
            work_dir: "/tmp/work".into(),
            session_id: Some("test-session-123".into()),
            workspace_path: Some("/tmp/ws/test".into()),
            chain_hash: None,
        };
        backend.record_dispatch(&dispatch).await.unwrap();

        let active = backend.active_dispatches().await.unwrap();
        assert_eq!(active.len(), 1);

        backend.complete_dispatch("d-001", "success").await.unwrap();
        let active = backend.active_dispatches().await.unwrap();
        assert!(active.is_empty());

        // -- LinkageStore --
        let from = WorkRef {
            repo: "rosary".into(),
            bead_id: "rsry-001".into(),
            scope: String::new(),
        };
        let to = WorkRef {
            repo: "mache".into(),
            bead_id: "mch-001".into(),
            scope: String::new(),
        };
        let dep = CrossRepoDep {
            from: from.clone(),
            to: to.clone(),
            dep_type: "blocks".into(),
            evidence_tier: crate::store::EvidenceTier::Asserted,
            source: "human".into(),
            observed_at: chrono::Utc::now(),
        };
        backend.add_dependency(&dep).await.unwrap();

        let deps = backend.dependencies_of(&from).await.unwrap();
        assert_eq!(deps.len(), 1);
        let dependents = backend.dependents_of(&to).await.unwrap();
        assert_eq!(dependents.len(), 1);

        let link = LinearLink {
            bead_ref: bead.clone(),
            linear_id: "AGE-330".into(),
            linear_type: "issue".into(),
        };
        backend.upsert_linear_link(&link).await.unwrap();

        let found = backend.find_by_linear_id("AGE-330").await.unwrap().unwrap();
        assert_eq!(found.bead_ref, bead);

        // Cleanup: kill the Dolt server
        let pid_file = tmp.path().join("backend.pid");
        if let Ok(pid_str) = std::fs::read_to_string(&pid_file)
            && let Ok(pid) = pid_str.trim().parse::<i32>()
        {
            unsafe {
                libc::kill(pid, libc::SIGTERM);
            }
        }
    }
}
