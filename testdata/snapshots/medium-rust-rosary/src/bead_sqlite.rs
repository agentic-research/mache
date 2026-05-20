//! SQLite-backed per-repo bead store.
//!
//! Each repo gets its own `.beads/beads.db` file. No server process needed.
//! Schema mirrors the Dolt `issues`/`dependencies`/`comments`/`events` tables.

use anyhow::{Context, Result};
use async_trait::async_trait;
use chrono::{NaiveDateTime, TimeZone, Utc};
use rusqlite::{Connection, OptionalExtension, params};
use std::path::{Path, PathBuf};
use std::sync::Mutex;

use crate::bead::{Bead, BeadUpdate};
use crate::store::BeadStore;

/// Parse files and test_files from the notes JSON column.
/// Shared between SQLite and Dolt implementations.
pub fn parse_files_from_notes(notes: Option<&str>) -> (Vec<String>, Vec<String>) {
    let parsed: Option<serde_json::Value> = notes.and_then(|s| serde_json::from_str(s).ok());
    let files = parsed
        .as_ref()
        .and_then(|v| v.get("files"))
        .and_then(|v| v.as_array())
        .map(|arr| {
            arr.iter()
                .filter_map(|v| v.as_str().map(|s| s.to_string()))
                .collect()
        })
        .unwrap_or_default();
    let test_files = parsed
        .as_ref()
        .and_then(|v| v.get("test_files"))
        .and_then(|v| v.as_array())
        .map(|arr| {
            arr.iter()
                .filter_map(|v| v.as_str().map(|s| s.to_string()))
                .collect()
        })
        .unwrap_or_default();
    (files, test_files)
}

/// Parse derived_from provenance chain from the notes JSON column.
pub fn parse_derived_from_notes(notes: Option<&str>) -> Vec<bdr::provenance::ProvenanceRef> {
    let parsed: Option<serde_json::Value> = notes.and_then(|s| serde_json::from_str(s).ok());
    parsed
        .as_ref()
        .and_then(|v| v.get("derived_from"))
        .and_then(|v| serde_json::from_value(v.clone()).ok())
        .unwrap_or_default()
}

/// Parse a datetime string from SQLite into a chrono DateTime<Utc>.
fn parse_datetime(s: &str) -> chrono::DateTime<Utc> {
    // Try ISO 8601 formats: "2024-01-15 12:34:56" or "2024-01-15T12:34:56"
    NaiveDateTime::parse_from_str(s, "%Y-%m-%d %H:%M:%S")
        .or_else(|_| NaiveDateTime::parse_from_str(s, "%Y-%m-%dT%H:%M:%S"))
        .map(|ndt| Utc.from_utc_datetime(&ndt))
        .unwrap_or_else(|_| Utc::now())
}

/// Read a Bead from a rusqlite Row.
fn bead_from_row(row: &rusqlite::Row<'_>, repo_name: &str) -> rusqlite::Result<Bead> {
    let notes: Option<String> = row.get("notes")?;
    let (files, test_files) = parse_files_from_notes(notes.as_deref());
    let derived_from = parse_derived_from_notes(notes.as_deref());
    let created_str: String = row.get("created_at")?;
    let updated_str: String = row.get("updated_at")?;

    Ok(Bead {
        id: row.get("id")?,
        title: row.get("title")?,
        description: row
            .get::<_, Option<String>>("description")?
            .unwrap_or_default(),
        status: row.get("status")?,
        priority: row.get::<_, i32>("priority").unwrap_or(2) as u8,
        issue_type: row
            .get::<_, Option<String>>("issue_type")?
            .unwrap_or_else(|| "task".into()),
        owner: row.get("assignee")?,
        repo: repo_name.to_string(),
        created_at: parse_datetime(&created_str),
        updated_at: parse_datetime(&updated_str),
        dependency_count: row.get::<_, i64>("dependency_count").unwrap_or(0) as u32,
        dependent_count: row.get::<_, i64>("dep_count").unwrap_or(0) as u32,
        comment_count: row.get::<_, i64>("comment_count").unwrap_or(0) as u32,
        branch: None,
        pr_url: None,
        jj_change_id: None,
        external_ref: row.get("external_ref")?,
        files,
        test_files,
        created_by: row.get::<_, Option<String>>("created_by").unwrap_or(None),
        scope: row.get::<_, String>("scope").unwrap_or_default(),
        derived_from,
    })
}

/// Connect to the bead store for a repo's `.beads/` directory.
///
/// Default: Dolt (per-repo version-controlled database — branch-per-agent,
/// cell-level merge, commit history). Falls back to SQLite if Dolt is
/// unavailable (no server, no port file) or if `beads.db` exists and Dolt
/// doesn't.
///
/// SQLite is useful for: tests, offline/lightweight repos, portable exports.
/// Open a bead store for the given `.beads/` directory.
///
/// Two paths, no fallback:
///   1. `dolt/` exists → Dolt is canonical. Rosary is a consumer of the bead
///      store, not the owner. Falling back to SQLite when Dolt is unreachable
///      creates a shadow store that diverges silently. Fail loudly instead —
///      `DoltClient::connect` already auto-starts the server if it's not running.
///   2. No `dolt/` → repo is not yet initialized; use SQLite for bootstrapping.
pub async fn connect_bead_store(beads_dir: &Path) -> Result<Box<dyn BeadStore>> {
    let dolt_dir = beads_dir.join("dolt");
    if dolt_dir.exists() {
        let config = crate::dolt::DoltConfig::from_beads_dir(beads_dir)?;
        let client = crate::dolt::DoltClient::connect(&config).await?;
        if let Err(e) = client.migrate().await {
            eprintln!("[bead] migration warning for {}: {e}", beads_dir.display());
        }

        // One-shot migration: if a stale beads.db exists and hasn't been migrated yet,
        // drain it into Dolt and rename it so we don't run again.
        let sqlite_path = beads_dir.join("beads.db");
        let migrated_path = beads_dir.join("beads.db.migrated");
        if sqlite_path.exists() && !migrated_path.exists() {
            match migrate_sqlite_to_dolt(&sqlite_path, &client).await {
                Ok(n) if n > 0 => {
                    eprintln!("[bead] migrated {n} beads from beads.db → Dolt");
                    if let Err(e) = std::fs::rename(&sqlite_path, &migrated_path) {
                        eprintln!("[bead] could not rename beads.db after migration: {e}");
                    }
                }
                Ok(_) => {
                    // Nothing to migrate — still rename to avoid re-checking every time.
                    let _ = std::fs::rename(&sqlite_path, &migrated_path);
                }
                Err(e) => eprintln!("[bead] SQLite→Dolt migration failed: {e}"),
            }
        }

        return Ok(Box::new(crate::bead_dolt::DoltBeadStore::new(client)));
    }

    // No dolt/ dir — repo not yet initialized. SQLite for bootstrapping only.
    let sqlite_path = beads_dir.join("beads.db");
    let store = SqliteBeadStore::connect(&sqlite_path)?;
    Ok(Box::new(store))
}

/// Drain all beads from a stale SQLite file into the connected Dolt client.
/// Skips beads whose ID already exists in Dolt (idempotent).
/// Returns the number of beads actually migrated.
async fn migrate_sqlite_to_dolt(
    sqlite_path: &Path,
    client: &crate::dolt::DoltClient,
) -> Result<usize> {
    // rusqlite::Connection is !Send — read all rows synchronously into a Vec
    // before the first .await, then drop the connection.
    struct Row {
        id: String,
        title: String,
        description: String,
        priority: u8,
        issue_type: String,
        owner: String,
        status: String,
        created_by: Option<String>,
        scope: String,
    }

    let rows: Vec<Row> = {
        let conn = Connection::open(sqlite_path)?;
        let mut stmt = conn.prepare(
            "SELECT id, title, description, priority, issue_type, assignee, notes, status, created_by, scope FROM issues",
        )?;
        stmt.query_map([], |r| {
            Ok(Row {
                id: r.get(0)?,
                title: r.get(1)?,
                description: r.get::<_, Option<String>>(2)?.unwrap_or_default(),
                priority: r.get::<_, i64>(3).unwrap_or(2) as u8,
                issue_type: r
                    .get::<_, Option<String>>(4)?
                    .unwrap_or_else(|| "task".to_string()),
                owner: r
                    .get::<_, Option<String>>(5)?
                    .unwrap_or_else(|| "dev-agent".to_string()),
                status: r
                    .get::<_, Option<String>>(7)?
                    .unwrap_or_else(|| "open".to_string()),
                created_by: r.get(8)?,
                scope: r.get::<_, Option<String>>(9)?.unwrap_or_default(),
            })
        })?
        .filter_map(|r| r.ok())
        .collect()
        // conn dropped here — no non-Send value crosses any .await below
    };

    let mut migrated = 0usize;
    for row in rows {
        // Skip if already in Dolt (re-run safety).
        if client.get_bead(&row.id, "").await.unwrap_or(None).is_some() {
            continue;
        }

        // Scrub before writing into version-controlled history.
        let title = crate::secrets::scrub_and_warn(&row.title, &format!("migrating {}", row.id));
        let description =
            crate::secrets::scrub_and_warn(&row.description, &format!("migrating {}", row.id));

        client
            .create_bead_full(
                &row.id,
                &title,
                &description,
                row.priority,
                &row.issue_type,
                &row.owner,
                &[],
                &[],
                &[],
                row.created_by.as_deref(),
                &row.scope,
                &[],
            )
            .await?;

        // Preserve status if not open.
        if row.status != "open" {
            let _ = client.update_status(&row.id, &row.status).await;
        }

        migrated += 1;
    }

    Ok(migrated)
}

pub struct SqliteBeadStore {
    conn: Mutex<Connection>,
    #[allow(dead_code)]
    path: PathBuf,
}

impl SqliteBeadStore {
    /// Resolve a short or full bead ID to the canonical full ID stored in the DB.
    ///
    /// Accepts either an exact match or a suffix match (e.g. "2a3970" → "ley-line-open-2a3970").
    /// Returns an error if zero or multiple beads match — ambiguous short IDs must be lengthened.
    fn resolve_id(conn: &Connection, id: &str) -> Result<String> {
        let exact: Option<String> = conn
            .query_row("SELECT id FROM issues WHERE id = ?1", params![id], |r| {
                r.get(0)
            })
            .optional()?;
        if let Some(full) = exact {
            return Ok(full);
        }
        // Suffix match: "abc123" matches "repo-abc123"
        let suffix_pattern = format!("%-{id}");
        let mut stmt = conn.prepare("SELECT id FROM issues WHERE id LIKE ?1")?;
        let matches: Vec<String> = stmt
            .query_map(params![suffix_pattern], |r| r.get(0))?
            .filter_map(|r| r.ok())
            .collect();
        match matches.len() {
            0 => anyhow::bail!("bead not found: {id}"),
            1 => Ok(matches.into_iter().next().unwrap()),
            _ => anyhow::bail!("ambiguous bead ID {id:?} — matches: {}", matches.join(", ")),
        }
    }

    /// Open or create the bead database at the given path.
    pub fn connect(path: &Path) -> Result<Self> {
        if let Some(parent) = path.parent() {
            std::fs::create_dir_all(parent)?;
        }
        let conn = Connection::open(path).context("opening sqlite bead store")?;
        conn.execute_batch("PRAGMA journal_mode=WAL;")?;
        conn.execute_batch("PRAGMA foreign_keys=ON;")?;
        conn.execute_batch(SCHEMA)?;
        // Additive migrations: safe on existing databases.
        // Fail silently if the column already exists.
        let _ = conn.execute_batch("ALTER TABLE issues ADD COLUMN created_by TEXT");
        let _ = conn.execute_batch("ALTER TABLE issues ADD COLUMN scope TEXT NOT NULL DEFAULT ''");
        // rosary-a96b06: comment audit-trail columns. Idempotent on existing DBs.
        let _ = conn.execute_batch("ALTER TABLE comments ADD COLUMN edited_at TEXT");
        let _ = conn.execute_batch("ALTER TABLE comments ADD COLUMN edit_reason TEXT");
        let _ = conn.execute_batch("ALTER TABLE comments ADD COLUMN original_text TEXT");
        let _ = conn.execute_batch("ALTER TABLE comments ADD COLUMN deleted_at TEXT");
        let _ = conn.execute_batch("ALTER TABLE comments ADD COLUMN delete_reason TEXT");
        let _ = conn.execute_batch(
            "CREATE INDEX IF NOT EXISTS idx_comments_issue_id ON comments(issue_id)",
        );
        let _ = conn.execute_batch(
            "CREATE INDEX IF NOT EXISTS idx_comments_deleted_at ON comments(deleted_at)",
        );
        // FTS5 index for full-text search with porter stemmer.
        // Separate from issues table — manually kept in sync on create/update.
        let _ = conn.execute_batch(
            "CREATE VIRTUAL TABLE IF NOT EXISTS bead_fts USING fts5(\
                id UNINDEXED, title, description,\
                tokenize='porter unicode61'\
            );",
        );
        Ok(SqliteBeadStore {
            conn: Mutex::new(conn),
            path: path.to_path_buf(),
        })
    }
}

const SCHEMA: &str = "
CREATE TABLE IF NOT EXISTS issues (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    description TEXT DEFAULT '',
    design TEXT DEFAULT '',
    acceptance_criteria TEXT DEFAULT '',
    notes TEXT DEFAULT '',
    status TEXT NOT NULL DEFAULT 'open',
    priority INTEGER NOT NULL DEFAULT 2,
    issue_type TEXT NOT NULL DEFAULT 'task',
    assignee TEXT,
    external_ref TEXT,
    user_id TEXT,
    created_by TEXT,
    scope TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS dependencies (
    issue_id TEXT NOT NULL,
    depends_on_id TEXT NOT NULL,
    PRIMARY KEY (issue_id, depends_on_id)
);

CREATE TABLE IF NOT EXISTS comments (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id TEXT NOT NULL,
    text TEXT NOT NULL,
    author TEXT NOT NULL,
    created_at TEXT NOT NULL,
    edited_at TEXT,
    edit_reason TEXT,
    original_text TEXT,
    deleted_at TEXT,
    delete_reason TEXT
);
-- Indexes on comments live in the post-ALTER block in `connect()`,
-- not here. On legacy DBs whose comments table predates the audit-trail
-- columns, `CREATE INDEX ON comments(deleted_at)` fails because the
-- column doesn't exist yet — and execute_batch(SCHEMA)? would propagate
-- that. Defining the index after the ALTER ADD COLUMN runs keeps
-- legacy DBs migrating cleanly without spurious no-such-column warnings.

CREATE TABLE IF NOT EXISTS events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    actor TEXT NOT NULL DEFAULT 'rosary',
    comment TEXT,
    created_at TEXT NOT NULL
);
";

/// SQL for listing beads with dependency/comment counts (non-closed).
const LIST_BEADS_SQL: &str = "
SELECT i.id, i.title, i.description, i.status, i.priority, i.issue_type,
       i.assignee, i.external_ref, i.notes, i.created_at, i.updated_at,
       i.created_by, i.scope,
       COALESCE(dep.cnt, 0) as dep_count,
       COALESCE(deps.cnt, 0) as dependency_count,
       COALESCE(cmt.cnt, 0) as comment_count
FROM issues i
LEFT JOIN (SELECT depends_on_id, COUNT(*) as cnt FROM dependencies GROUP BY depends_on_id) dep
     ON dep.depends_on_id = i.id
LEFT JOIN (SELECT d.issue_id, COUNT(*) as cnt
          FROM dependencies d
          JOIN issues dep_i ON dep_i.id = d.depends_on_id
          WHERE dep_i.status NOT IN ('closed', 'done')
          GROUP BY d.issue_id) deps
     ON deps.issue_id = i.id
LEFT JOIN (SELECT issue_id, COUNT(*) as cnt FROM comments GROUP BY issue_id) cmt
     ON cmt.issue_id = i.id
WHERE i.status NOT IN ('closed', 'done')
ORDER BY i.priority ASC, i.created_at DESC
";

#[async_trait]
impl BeadStore for SqliteBeadStore {
    async fn list_beads(&self, repo_name: &str) -> Result<Vec<Bead>> {
        let conn = self.conn.lock().unwrap();
        let mut stmt = conn.prepare(LIST_BEADS_SQL)?;
        let beads = stmt
            .query_map([], |row| bead_from_row(row, repo_name))?
            .filter_map(|r| r.ok())
            .collect();
        Ok(beads)
    }

    async fn list_beads_scoped(&self, repo_name: &str, user_id: Option<&str>) -> Result<Vec<Bead>> {
        match user_id {
            Some(uid) => {
                let conn = self.conn.lock().unwrap();
                let sql_scoped = "
                    SELECT i.id, i.title, i.description, i.status, i.priority, i.issue_type,
                           i.assignee, i.external_ref, i.notes, i.created_at, i.updated_at,
                           i.created_by, i.scope,
                           COALESCE(dep.cnt, 0) as dep_count,
                           COALESCE(deps.cnt, 0) as dependency_count,
                           COALESCE(cmt.cnt, 0) as comment_count
                    FROM issues i
                    LEFT JOIN (SELECT depends_on_id, COUNT(*) as cnt FROM dependencies GROUP BY depends_on_id) dep
                         ON dep.depends_on_id = i.id
                    LEFT JOIN (SELECT d.issue_id, COUNT(*) as cnt
                              FROM dependencies d
                              JOIN issues dep_i ON dep_i.id = d.depends_on_id
                              WHERE dep_i.status NOT IN ('closed', 'done')
                              GROUP BY d.issue_id) deps
                         ON deps.issue_id = i.id
                    LEFT JOIN (SELECT issue_id, COUNT(*) as cnt FROM comments GROUP BY issue_id) cmt
                         ON cmt.issue_id = i.id
                    WHERE i.status NOT IN ('closed', 'done') AND i.user_id = ?1
                    ORDER BY i.priority ASC, i.created_at DESC
                ";
                let mut stmt = conn.prepare(sql_scoped)?;
                let beads = stmt
                    .query_map(params![uid], |row| bead_from_row(row, repo_name))?
                    .filter_map(|r| r.ok())
                    .collect();
                Ok(beads)
            }
            None => self.list_beads(repo_name).await,
        }
    }

    async fn get_bead(&self, id: &str, repo_name: &str) -> Result<Option<Bead>> {
        let conn = self.conn.lock().unwrap();
        let full_id = match Self::resolve_id(&conn, id) {
            Ok(id) => id,
            Err(_) => return Ok(None),
        };
        let mut stmt = conn.prepare(
            "SELECT i.id, i.title, i.description, i.status, i.priority, i.issue_type,
                    i.assignee, i.external_ref, i.notes, i.created_at, i.updated_at,
                    i.created_by, i.scope,
                    (SELECT COUNT(*) FROM dependencies d WHERE d.depends_on_id = i.id) as dep_count,
                    (SELECT COUNT(*) FROM dependencies d
                            JOIN issues dep_i ON dep_i.id = d.depends_on_id
                            WHERE d.issue_id = i.id
                            AND dep_i.status NOT IN ('closed', 'done')) as dependency_count,
                    (SELECT COUNT(*) FROM comments c WHERE c.issue_id = i.id) as comment_count
             FROM issues i
             WHERE i.id = ?1",
        )?;
        let bead = stmt
            .query_row(params![full_id], |row| bead_from_row(row, repo_name))
            .optional()?;
        Ok(bead)
    }

    async fn create_bead(
        &self,
        id: &str,
        title: &str,
        description: &str,
        priority: u8,
        issue_type: &str,
    ) -> Result<()> {
        let conn = self.conn.lock().unwrap();
        conn.execute(
            "INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type, created_at, updated_at)
             VALUES (?1, ?2, ?3, '', '', '', 'open', ?4, ?5, datetime('now'), datetime('now'))",
            params![id, title, description, priority as i32, issue_type],
        )
        .with_context(|| format!("creating bead {id}"))?;
        // Sync FTS index (best-effort)
        let _ = conn.execute(
            "INSERT INTO bead_fts(id, title, description) VALUES (?1, ?2, ?3)",
            params![id, title, description],
        );
        Ok(())
    }

    async fn create_bead_full(
        &self,
        id: &str,
        title: &str,
        description: &str,
        priority: u8,
        issue_type: &str,
        owner: &str,
        files: &[String],
        test_files: &[String],
        depends_on: &[String],
        created_by: Option<&str>,
        scope: &str,
        derived_from: &[bdr::provenance::ProvenanceRef],
    ) -> Result<()> {
        let conn = self.conn.lock().unwrap();
        let tx = conn.unchecked_transaction()?;

        tx.execute(
            "INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type, created_by, scope, created_at, updated_at)
             VALUES (?1, ?2, ?3, '', '', '', 'open', ?4, ?5, ?6, ?7, datetime('now'), datetime('now'))",
            params![id, title, description, priority as i32, issue_type, created_by, scope],
        )?;

        tx.execute(
            "UPDATE issues SET assignee = ?1, updated_at = datetime('now') WHERE id = ?2",
            params![owner, id],
        )?;

        if !files.is_empty() || !test_files.is_empty() || !derived_from.is_empty() {
            let notes_json = serde_json::json!({
                "files": files,
                "test_files": test_files,
                "derived_from": derived_from,
            });
            tx.execute(
                "UPDATE issues SET notes = ?1, updated_at = datetime('now') WHERE id = ?2",
                params![notes_json.to_string(), id],
            )?;
        }

        for dep_id in depends_on {
            tx.execute(
                "INSERT OR IGNORE INTO dependencies (issue_id, depends_on_id) VALUES (?1, ?2)",
                params![id, dep_id],
            )?;
        }

        // Sync FTS index (best-effort — secondary index, never fails the create)
        let _ = tx.execute(
            "INSERT INTO bead_fts(id, title, description) VALUES (?1, ?2, ?3)",
            params![id, title, description],
        );

        tx.commit()?;
        Ok(())
    }

    async fn update_bead_fields(&self, id: &str, update: &BeadUpdate) -> Result<Vec<String>> {
        let conn = self.conn.lock().unwrap();
        let full_id = Self::resolve_id(&conn, id)?;
        let mut updated_fields = Vec::new();

        // Build dynamic SET clauses. We execute each field update separately
        // to avoid dynamic bind complexity with rusqlite.
        if let Some(ref title) = update.title {
            conn.execute(
                "UPDATE issues SET title = ?1, updated_at = datetime('now') WHERE id = ?2",
                params![title, full_id],
            )?;
            updated_fields.push("title".to_string());
        }
        if let Some(ref description) = update.description {
            conn.execute(
                "UPDATE issues SET description = ?1, updated_at = datetime('now') WHERE id = ?2",
                params![description, full_id],
            )?;
            updated_fields.push("description".to_string());
        }
        if let Some(priority) = update.priority {
            conn.execute(
                "UPDATE issues SET priority = ?1, updated_at = datetime('now') WHERE id = ?2",
                params![priority as i32, full_id],
            )?;
            updated_fields.push("priority".to_string());
        }
        if let Some(ref issue_type) = update.issue_type {
            conn.execute(
                "UPDATE issues SET issue_type = ?1, updated_at = datetime('now') WHERE id = ?2",
                params![issue_type, full_id],
            )?;
            updated_fields.push("issue_type".to_string());
        }
        if let Some(ref owner) = update.owner {
            conn.execute(
                "UPDATE issues SET assignee = ?1, updated_at = datetime('now') WHERE id = ?2",
                params![owner, full_id],
            )?;
            updated_fields.push("owner".to_string());
        }
        if update.files.is_some() || update.test_files.is_some() {
            // Read existing notes to preserve fields not being updated
            let existing_notes: serde_json::Value = conn
                .query_row(
                    "SELECT notes FROM issues WHERE id = ?1",
                    params![full_id],
                    |row| row.get::<_, Option<String>>(0),
                )
                .ok()
                .flatten()
                .and_then(|s| serde_json::from_str(&s).ok())
                .unwrap_or_else(|| serde_json::json!({}));

            let files = update
                .files
                .as_ref()
                .map(|f| serde_json::json!(f))
                .unwrap_or_else(|| {
                    existing_notes
                        .get("files")
                        .cloned()
                        .unwrap_or(serde_json::json!([]))
                });
            let test_files_val = update
                .test_files
                .as_ref()
                .map(|f| serde_json::json!(f))
                .unwrap_or_else(|| {
                    existing_notes
                        .get("test_files")
                        .cloned()
                        .unwrap_or(serde_json::json!([]))
                });
            let notes_json = serde_json::json!({ "files": files, "test_files": test_files_val });
            conn.execute(
                "UPDATE issues SET notes = ?1, updated_at = datetime('now') WHERE id = ?2",
                params![notes_json.to_string(), full_id],
            )?;
            if update.files.is_some() {
                updated_fields.push("files".to_string());
            }
            if update.test_files.is_some() {
                updated_fields.push("test_files".to_string());
            }
        }

        // Re-sync FTS if title or description changed (delete + re-insert from issues)
        if updated_fields
            .iter()
            .any(|f| f == "title" || f == "description")
        {
            let _ = conn.execute("DELETE FROM bead_fts WHERE id = ?1", params![full_id]);
            let _ = conn.execute(
                "INSERT INTO bead_fts(id, title, description) \
                 SELECT id, title, description FROM issues WHERE id = ?1",
                params![full_id],
            );
        }

        Ok(updated_fields)
    }

    async fn update_status(&self, id: &str, status: &str) -> Result<()> {
        use crate::bead::BeadState;
        let next = BeadState::from(status);
        let conn = self.conn.lock().unwrap();
        let full_id = Self::resolve_id(&conn, id)?;
        // Validate transition before writing. Read current status under the same lock
        // so the check-then-act is atomic within this process.
        let current_str: Option<String> = conn
            .query_row(
                "SELECT status FROM issues WHERE id = ?1",
                params![full_id],
                |row| row.get(0),
            )
            .optional()?;
        if let Some(ref cs) = current_str {
            let current = BeadState::from(cs.as_str());
            if !current.can_transition_to(next) {
                return Err(anyhow::anyhow!(
                    "invalid state transition: {} -> {} for bead {}",
                    current,
                    next,
                    id
                ));
            }
        }
        conn.execute(
            "UPDATE issues SET status = ?1, updated_at = datetime('now') WHERE id = ?2",
            params![status, full_id],
        )
        .with_context(|| format!("updating status for {id}"))?;
        Ok(())
    }

    async fn get_status(&self, id: &str) -> Result<Option<String>> {
        let conn = self.conn.lock().unwrap();
        let full_id = match Self::resolve_id(&conn, id) {
            Ok(id) => id,
            Err(_) => return Ok(None),
        };
        let status = conn
            .query_row(
                "SELECT status FROM issues WHERE id = ?1",
                params![full_id],
                |row| row.get(0),
            )
            .optional()?;
        Ok(status)
    }

    async fn close_bead(&self, id: &str) -> Result<()> {
        let conn = self.conn.lock().unwrap();
        let full_id = Self::resolve_id(&conn, id)?;
        conn.execute(
            "UPDATE issues SET status = 'closed', updated_at = datetime('now') WHERE id = ?1",
            params![full_id],
        )
        .with_context(|| format!("closing bead {id}"))?;
        Ok(())
    }

    async fn set_assignee(&self, id: &str, assignee: &str) -> Result<()> {
        let conn = self.conn.lock().unwrap();
        let full_id = Self::resolve_id(&conn, id)?;
        conn.execute(
            "UPDATE issues SET assignee = ?1, updated_at = datetime('now') WHERE id = ?2",
            params![assignee, full_id],
        )?;
        Ok(())
    }

    async fn set_user_id(&self, id: &str, user_id: &str) -> Result<()> {
        let conn = self.conn.lock().unwrap();
        let full_id = Self::resolve_id(&conn, id)?;
        conn.execute(
            "UPDATE issues SET user_id = ?1 WHERE id = ?2",
            params![user_id, full_id],
        )?;
        Ok(())
    }

    async fn set_files(&self, id: &str, files: &[String], test_files: &[String]) -> Result<()> {
        let file_json = serde_json::json!({ "files": files, "test_files": test_files });
        let conn = self.conn.lock().unwrap();
        let full_id = Self::resolve_id(&conn, id)?;
        conn.execute(
            "UPDATE issues SET notes = ?1, updated_at = datetime('now') WHERE id = ?2",
            params![file_json.to_string(), full_id],
        )?;
        Ok(())
    }

    async fn search_beads(
        &self,
        query_str: &str,
        repo_name: &str,
        limit: u32,
    ) -> Result<Vec<Bead>> {
        let conn = self.conn.lock().unwrap();
        let words: Vec<String> = query_str
            .split_whitespace()
            .map(|w| format!("%{}%", w.to_lowercase()))
            .collect();

        // Each word must appear in title, description, id, OR a live
        // (non-soft-deleted) comment. Comment-text matching is what
        // rosary-a9bc77 added — most context lives in comments and
        // wasn't previously searchable.
        let where_clause = if words.is_empty() {
            "1=1".to_string()
        } else {
            words
                .iter()
                .enumerate()
                .map(|(i, _)| {
                    let p = (i * 4) + 1;
                    format!(
                        "(LOWER(i.title) LIKE ?{p} OR LOWER(i.description) LIKE ?{} \
                          OR i.id LIKE ?{} \
                          OR EXISTS (SELECT 1 FROM comments c \
                                     WHERE c.issue_id = i.id \
                                       AND c.deleted_at IS NULL \
                                       AND LOWER(c.text) LIKE ?{}))",
                        p + 1,
                        p + 2,
                        p + 3,
                    )
                })
                .collect::<Vec<_>>()
                .join(" AND ")
        };

        let sql = format!(
            "SELECT i.id, i.title, i.description, i.status, i.priority, i.issue_type,
                    i.assignee, i.external_ref, i.notes, i.created_at, i.updated_at,
                    i.created_by, i.scope,
                    COALESCE(dep.cnt, 0) as dep_count,
                    COALESCE(deps.cnt, 0) as dependency_count,
                    COALESCE(cmt.cnt, 0) as comment_count
             FROM issues i
             LEFT JOIN (SELECT depends_on_id, COUNT(*) as cnt FROM dependencies GROUP BY depends_on_id) dep
                  ON dep.depends_on_id = i.id
             LEFT JOIN (SELECT d.issue_id, COUNT(*) as cnt
                       FROM dependencies d
                       LEFT JOIN issues dep_i ON dep_i.id = d.depends_on_id
                       WHERE dep_i.id IS NULL OR dep_i.status NOT IN ('closed', 'done')
                       GROUP BY d.issue_id) deps
                  ON deps.issue_id = i.id
             LEFT JOIN (SELECT issue_id, COUNT(*) as cnt FROM comments GROUP BY issue_id) cmt
                  ON cmt.issue_id = i.id
             WHERE {where_clause}
             ORDER BY i.priority ASC, i.created_at DESC
             LIMIT {limit}"
        );

        let mut stmt = conn.prepare(&sql)?;
        // Build params: each word appears four times — title, description,
        // id, comment text (rosary-a9bc77).
        let param_values: Vec<Box<dyn rusqlite::types::ToSql>> = words
            .iter()
            .flat_map(|w| {
                vec![
                    Box::new(w.clone()) as Box<dyn rusqlite::types::ToSql>,
                    Box::new(w.clone()) as Box<dyn rusqlite::types::ToSql>,
                    Box::new(w.clone()) as Box<dyn rusqlite::types::ToSql>,
                    Box::new(w.clone()) as Box<dyn rusqlite::types::ToSql>,
                ]
            })
            .collect();
        let param_refs: Vec<&dyn rusqlite::types::ToSql> =
            param_values.iter().map(|p| p.as_ref()).collect();

        let beads = stmt
            .query_map(param_refs.as_slice(), |row| bead_from_row(row, repo_name))?
            .filter_map(|r| r.ok())
            .collect();
        Ok(beads)
    }

    async fn search_beads_fts(
        &self,
        query_str: &str,
        repo_name: &str,
        limit: u32,
    ) -> Result<Vec<Bead>> {
        let conn = self.conn.lock().unwrap();
        // Wrap each token in double quotes: "word" prevents FTS5 operator interpretation
        // and gives "all words must appear" semantics (implicit AND).
        let fts_query: String = query_str
            .split_whitespace()
            .map(|w| format!("\"{}\"", w.replace('"', "")))
            .collect::<Vec<_>>()
            .join(" ");

        if fts_query.is_empty() {
            return Ok(vec![]);
        }

        let sql = "SELECT i.id, i.title, i.description, i.status, i.priority, i.issue_type,
                        i.assignee, i.external_ref, i.notes, i.created_at, i.updated_at,
                        i.created_by, i.scope,
                        COALESCE(dep.cnt, 0) as dep_count,
                        COALESCE(deps.cnt, 0) as dependency_count,
                        COALESCE(cmt.cnt, 0) as comment_count
                 FROM bead_fts f
                 JOIN issues i ON i.id = f.id
                 LEFT JOIN (SELECT depends_on_id, COUNT(*) as cnt FROM dependencies GROUP BY depends_on_id) dep
                      ON dep.depends_on_id = i.id
                 LEFT JOIN (SELECT d.issue_id, COUNT(*) as cnt
                           FROM dependencies d
                           LEFT JOIN issues dep_i ON dep_i.id = d.depends_on_id
                           WHERE dep_i.id IS NULL OR dep_i.status NOT IN ('closed', 'done')
                           GROUP BY d.issue_id) deps
                      ON deps.issue_id = i.id
                 LEFT JOIN (SELECT issue_id, COUNT(*) as cnt FROM comments GROUP BY issue_id) cmt
                      ON cmt.issue_id = i.id
                 WHERE bead_fts MATCH ?1
                 ORDER BY rank
                 LIMIT ?2";

        let mut stmt = conn.prepare(sql)?;
        let beads = stmt
            .query_map(params![fts_query, limit as i64], |row| {
                bead_from_row(row, repo_name)
            })?
            .filter_map(|r| r.ok())
            .collect();
        Ok(beads)
    }

    async fn get_external_ref(&self, id: &str) -> Result<Option<String>> {
        let conn = self.conn.lock().unwrap();
        let full_id = match Self::resolve_id(&conn, id) {
            Ok(id) => id,
            Err(_) => return Ok(None),
        };
        let result = conn
            .query_row(
                "SELECT external_ref FROM issues WHERE id = ?1 AND external_ref IS NOT NULL AND external_ref != ''",
                params![full_id],
                |row| row.get(0),
            )
            .optional()?;
        Ok(result)
    }

    async fn set_external_ref(&self, id: &str, external_ref: &str) -> Result<()> {
        let conn = self.conn.lock().unwrap();
        let full_id = Self::resolve_id(&conn, id)?;
        conn.execute(
            "UPDATE issues SET external_ref = ?1, updated_at = datetime('now') WHERE id = ?2",
            params![external_ref, full_id],
        )?;
        Ok(())
    }

    async fn find_by_external_ref(&self, external_ref: &str) -> Result<Option<String>> {
        let conn = self.conn.lock().unwrap();
        let result = conn
            .query_row(
                "SELECT id FROM issues WHERE external_ref = ?1",
                params![external_ref],
                |row| row.get(0),
            )
            .optional()?;
        Ok(result)
    }

    async fn list_closed_linked_beads(&self, repo_name: &str) -> Result<Vec<Bead>> {
        let conn = self.conn.lock().unwrap();
        let mut stmt = conn.prepare(
            "SELECT id, title, description, status, priority, issue_type,
                    assignee, external_ref, '' as notes, created_at, updated_at,
                    0 as dep_count, 0 as dependency_count, 0 as comment_count
             FROM issues
             WHERE status = 'closed' AND external_ref IS NOT NULL AND external_ref != ''
             ORDER BY updated_at DESC
             LIMIT 500",
        )?;
        let beads = stmt
            .query_map([], |row| bead_from_row(row, repo_name))?
            .filter_map(|r| r.ok())
            .collect();
        Ok(beads)
    }

    async fn add_dependency(&self, issue_id: &str, depends_on_id: &str) -> Result<()> {
        let conn = self.conn.lock().unwrap();
        let full_issue = Self::resolve_id(&conn, issue_id)?;
        let full_dep = Self::resolve_id(&conn, depends_on_id)?;
        conn.execute(
            "INSERT OR IGNORE INTO dependencies (issue_id, depends_on_id) VALUES (?1, ?2)",
            params![full_issue, full_dep],
        )?;
        Ok(())
    }

    async fn remove_dependency(&self, issue_id: &str, depends_on_id: &str) -> Result<()> {
        let conn = self.conn.lock().unwrap();
        let full_issue = Self::resolve_id(&conn, issue_id)?;
        let full_dep = Self::resolve_id(&conn, depends_on_id)?;
        conn.execute(
            "DELETE FROM dependencies WHERE issue_id = ?1 AND depends_on_id = ?2",
            params![full_issue, full_dep],
        )?;
        Ok(())
    }

    async fn get_dependencies(&self, issue_id: &str) -> Result<Vec<String>> {
        let conn = self.conn.lock().unwrap();
        let full_id = match Self::resolve_id(&conn, issue_id) {
            Ok(id) => id,
            Err(_) => return Ok(vec![]),
        };
        let mut stmt =
            conn.prepare("SELECT depends_on_id FROM dependencies WHERE issue_id = ?1")?;
        let deps = stmt
            .query_map(params![full_id], |row| row.get(0))?
            .filter_map(|r| r.ok())
            .collect();
        Ok(deps)
    }

    async fn get_dependents(&self, issue_id: &str) -> Result<Vec<String>> {
        let conn = self.conn.lock().unwrap();
        let full_id = match Self::resolve_id(&conn, issue_id) {
            Ok(id) => id,
            Err(_) => return Ok(vec![]),
        };
        let mut stmt =
            conn.prepare("SELECT issue_id FROM dependencies WHERE depends_on_id = ?1")?;
        let deps = stmt
            .query_map(params![full_id], |row| row.get(0))?
            .filter_map(|r| r.ok())
            .collect();
        Ok(deps)
    }

    async fn add_comment(&self, issue_id: &str, body: &str, author: &str) -> Result<()> {
        let conn = self.conn.lock().unwrap();
        let full_id = Self::resolve_id(&conn, issue_id)?;
        conn.execute(
            "INSERT INTO comments (issue_id, text, author, created_at) VALUES (?1, ?2, ?3, datetime('now'))",
            params![full_id, body, author],
        )?;
        Ok(())
    }

    async fn list_comments(
        &self,
        issue_id: &str,
        include_deleted: bool,
    ) -> Result<Vec<crate::bead::Comment>> {
        let conn = self.conn.lock().unwrap();
        let full_id = Self::resolve_id(&conn, issue_id)?;
        let sql = if include_deleted {
            "SELECT id, issue_id, text, author, created_at, edited_at, edit_reason,
                    original_text, deleted_at, delete_reason
             FROM comments WHERE issue_id = ?1 ORDER BY created_at ASC, id ASC"
        } else {
            "SELECT id, issue_id, text, author, created_at, edited_at, edit_reason,
                    original_text, deleted_at, delete_reason
             FROM comments WHERE issue_id = ?1 AND deleted_at IS NULL
             ORDER BY created_at ASC, id ASC"
        };
        let mut stmt = conn.prepare(sql)?;
        let rows = stmt
            .query_map(params![full_id], |row| {
                let created_str: String = row.get("created_at")?;
                let edited_str: Option<String> = row.get("edited_at")?;
                let deleted_str: Option<String> = row.get("deleted_at")?;
                Ok(crate::bead::Comment {
                    id: row.get::<_, i64>("id")?.to_string(),
                    issue_id: row.get("issue_id")?,
                    text: row.get("text")?,
                    author: row.get("author")?,
                    created_at: parse_datetime(&created_str),
                    edited_at: edited_str.map(|s| parse_datetime(&s)),
                    edit_reason: row.get("edit_reason")?,
                    original_text: row.get("original_text")?,
                    deleted_at: deleted_str.map(|s| parse_datetime(&s)),
                    delete_reason: row.get("delete_reason")?,
                })
            })?
            .filter_map(|r| r.ok())
            .collect();
        Ok(rows)
    }

    async fn update_comment(
        &self,
        comment_id: &str,
        body: &str,
        reason: Option<&str>,
    ) -> Result<crate::bead::Comment> {
        let conn = self.conn.lock().unwrap();
        // Read current state to decide whether to capture original_text.
        let (prior_text, has_original): (String, bool) = conn
            .query_row(
                "SELECT text, original_text IS NOT NULL FROM comments WHERE id = ?1",
                params![comment_id],
                |row| Ok((row.get(0)?, row.get::<_, i64>(1)? != 0)),
            )
            .optional()?
            .ok_or_else(|| anyhow::anyhow!("comment {comment_id} not found"))?;

        // First-edit captures original_text; subsequent edits don't rewrite it.
        if has_original {
            conn.execute(
                "UPDATE comments
                 SET text = ?1, edited_at = datetime('now'), edit_reason = ?2
                 WHERE id = ?3",
                params![body, reason, comment_id],
            )?;
        } else {
            conn.execute(
                "UPDATE comments
                 SET text = ?1, edited_at = datetime('now'),
                     edit_reason = ?2, original_text = ?3
                 WHERE id = ?4",
                params![body, reason, prior_text, comment_id],
            )?;
        }

        // Re-fetch to return the canonical post-update row.
        let mut stmt = conn.prepare(
            "SELECT id, issue_id, text, author, created_at, edited_at, edit_reason,
                    original_text, deleted_at, delete_reason
             FROM comments WHERE id = ?1",
        )?;
        let comment = stmt.query_row(params![comment_id], |row| {
            let created_str: String = row.get("created_at")?;
            let edited_str: Option<String> = row.get("edited_at")?;
            let deleted_str: Option<String> = row.get("deleted_at")?;
            Ok(crate::bead::Comment {
                id: row.get::<_, i64>("id")?.to_string(),
                issue_id: row.get("issue_id")?,
                text: row.get("text")?,
                author: row.get("author")?,
                created_at: parse_datetime(&created_str),
                edited_at: edited_str.map(|s| parse_datetime(&s)),
                edit_reason: row.get("edit_reason")?,
                original_text: row.get("original_text")?,
                deleted_at: deleted_str.map(|s| parse_datetime(&s)),
                delete_reason: row.get("delete_reason")?,
            })
        })?;
        Ok(comment)
    }

    async fn delete_comment(&self, comment_id: &str, reason: Option<&str>) -> Result<()> {
        let conn = self.conn.lock().unwrap();
        let exists: i64 = conn.query_row(
            "SELECT COUNT(*) FROM comments WHERE id = ?1",
            params![comment_id],
            |row| row.get(0),
        )?;
        if exists == 0 {
            anyhow::bail!("comment {comment_id} not found");
        }
        // The deletion reason lands in `delete_reason` — its own column —
        // so it survives even when the comment was previously edited (which
        // would have set `edit_reason`). Soft-delete is idempotent: a
        // re-delete refreshes both timestamp and reason.
        conn.execute(
            "UPDATE comments
             SET deleted_at = datetime('now'), delete_reason = ?1
             WHERE id = ?2",
            params![reason, comment_id],
        )?;
        Ok(())
    }

    async fn hard_delete_comment(&self, comment_id: &str) -> Result<()> {
        let conn = self.conn.lock().unwrap();
        let n = conn.execute("DELETE FROM comments WHERE id = ?1", params![comment_id])?;
        if n == 0 {
            anyhow::bail!("comment {comment_id} not found");
        }
        Ok(())
    }

    async fn log_event(&self, issue_id: &str, event_type: &str, detail: &str) {
        let conn = self.conn.lock().unwrap();
        // IDs starting with `_` are synthetic (e.g. `_schema` for migration
        // records) and skip the issues-table resolution step. Without this,
        // every migration logged a noisy "bead not found" warning.
        let full_id = if issue_id.starts_with('_') {
            issue_id.to_string()
        } else {
            match Self::resolve_id(&conn, issue_id) {
                Ok(id) => id,
                Err(e) => {
                    eprintln!("warning: failed to resolve bead ID {issue_id} for event log: {e}");
                    return;
                }
            }
        };
        let result = conn.execute(
            "INSERT INTO events (issue_id, event_type, actor, comment, created_at) VALUES (?1, ?2, 'rosary', ?3, datetime('now'))",
            params![full_id, event_type, detail],
        );
        if let Err(e) = result {
            eprintln!("warning: failed to log event for {issue_id}: {e}");
        }
    }

    async fn get_latest_event(&self, issue_id: &str, event_type: &str) -> Result<Option<String>> {
        let conn = self.conn.lock().unwrap();
        let full_id = match Self::resolve_id(&conn, issue_id) {
            Ok(id) => id,
            Err(_) => return Ok(None),
        };
        let result = conn
            .query_row(
                "SELECT comment FROM events WHERE issue_id = ?1 AND event_type = ?2 ORDER BY created_at DESC LIMIT 1",
                params![full_id, event_type],
                |row| row.get(0),
            )
            .optional()?;
        Ok(result)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn test_store() -> SqliteBeadStore {
        SqliteBeadStore::connect(Path::new(":memory:")).unwrap()
    }

    #[tokio::test]
    async fn create_and_get_bead() {
        let store = test_store();
        store
            .create_bead("test-1", "Test bead", "A description", 2, "task")
            .await
            .unwrap();

        let bead = store.get_bead("test-1", "rosary").await.unwrap().unwrap();
        assert_eq!(bead.id, "test-1");
        assert_eq!(bead.title, "Test bead");
        assert_eq!(bead.status, "open");
        assert_eq!(bead.priority, 2);
    }

    // Regression: short ID (suffix only) must resolve to full prefixed ID.
    // Before fix: close_bead("2a3970") silently succeeded with 0 rows changed
    // when the stored ID was "ley-line-open-2a3970".
    #[tokio::test]
    async fn close_bead_short_id_resolves() {
        let store = test_store();
        store
            .create_bead("repo-2a3970", "Some bead", "", 2, "task")
            .await
            .unwrap();

        // short suffix — must close the right row
        store.close_bead("2a3970").await.unwrap();

        let conn = store.conn.lock().unwrap();
        let status: String = conn
            .query_row(
                "SELECT status FROM issues WHERE id = 'repo-2a3970'",
                [],
                |r| r.get(0),
            )
            .unwrap();
        assert_eq!(status, "closed");
    }

    #[tokio::test]
    async fn close_bead_unknown_id_errors() {
        let store = test_store();
        let err = store.close_bead("doesnotexist").await.unwrap_err();
        assert!(err.to_string().contains("not found"), "{err}");
    }

    #[tokio::test]
    async fn list_beads_excludes_closed() {
        let store = test_store();
        store.create_bead("a", "Open", "", 1, "task").await.unwrap();
        store
            .create_bead("b", "Closed", "", 2, "task")
            .await
            .unwrap();
        store.close_bead("b").await.unwrap();

        let beads = store.list_beads("repo").await.unwrap();
        assert_eq!(beads.len(), 1);
        assert_eq!(beads[0].id, "a");
    }

    #[tokio::test]
    async fn create_bead_full_with_deps() {
        let store = test_store();
        store
            .create_bead("dep-1", "Dep", "", 1, "task")
            .await
            .unwrap();
        store
            .create_bead_full(
                "main-1",
                "Main",
                "desc",
                1,
                "feature",
                "agent",
                &["src/main.rs".into()],
                &["src/main_test.rs".into()],
                &["dep-1".into()],
                Some("test-user"),
                "",
                &[],
            )
            .await
            .unwrap();

        let bead = store.get_bead("main-1", "repo").await.unwrap().unwrap();
        assert_eq!(bead.owner.as_deref(), Some("agent"));
        assert_eq!(bead.files, vec!["src/main.rs"]);
        assert_eq!(bead.test_files, vec!["src/main_test.rs"]);

        let deps = store.get_dependencies("main-1").await.unwrap();
        assert_eq!(deps, vec!["dep-1"]);
    }

    #[tokio::test]
    async fn search_beads_by_title() {
        let store = test_store();
        store
            .create_bead("a", "Fix dispatch bug", "", 1, "bug")
            .await
            .unwrap();
        store
            .create_bead("b", "Add feature X", "", 2, "feature")
            .await
            .unwrap();

        let results = store.search_beads("dispatch", "repo", 10).await.unwrap();
        assert_eq!(results.len(), 1);
        assert_eq!(results[0].id, "a");
    }

    /// rosary-a9bc77: search hits comment text, not just title/description.
    #[tokio::test]
    async fn search_beads_matches_comment_text() {
        let store = test_store();
        store
            .create_bead("a", "Generic title one", "", 1, "task")
            .await
            .unwrap();
        store
            .create_bead("b", "Generic title two", "", 1, "task")
            .await
            .unwrap();
        // Distinctive content lives in a comment on bead `b`.
        store
            .add_comment("b", "investigating zarathustra anomaly", "u")
            .await
            .unwrap();

        // Title search misses both (no overlap with comment text).
        assert!(
            store
                .search_beads("zarathustra", "repo", 10)
                .await
                .unwrap()
                .iter()
                .any(|x| x.id == "b")
        );
        // Sanity: searching for a word in `a`'s title still works.
        let one = store
            .search_beads("Generic title one", "repo", 10)
            .await
            .unwrap();
        assert!(one.iter().any(|x| x.id == "a"));
    }

    /// rosary-a9bc77: soft-deleted comments must NOT contribute to search hits.
    /// Otherwise scrubbed PII would still surface — which is the exact failure
    /// mode the comment-edit primitive was added to fix.
    #[tokio::test]
    async fn search_beads_excludes_soft_deleted_comments() {
        let store = test_store();
        store
            .create_bead("a", "Title with no match", "", 1, "task")
            .await
            .unwrap();
        store
            .add_comment("a", "leaked /Users/alice/secret/path", "u")
            .await
            .unwrap();

        // Pre-delete: comment-text search hits.
        let pre = store.search_beads("alice", "repo", 10).await.unwrap();
        assert!(pre.iter().any(|b| b.id == "a"), "comment text should match");

        // Soft-delete the comment.
        let cid = store.list_comments("a", false).await.unwrap()[0].id.clone();
        store.delete_comment(&cid, Some("scrub")).await.unwrap();

        // Post-delete: comment is hidden from search.
        let post = store.search_beads("alice", "repo", 10).await.unwrap();
        assert!(
            !post.iter().any(|b| b.id == "a"),
            "soft-deleted comment text must not surface in search",
        );
    }

    #[tokio::test]
    async fn search_beads_by_id() {
        let store = test_store();
        store
            .create_bead("rosary-abc123", "Fix dispatch bug", "", 1, "bug")
            .await
            .unwrap();
        store
            .create_bead("rosary-def456", "Add feature X", "", 2, "feature")
            .await
            .unwrap();

        // Exact ID match
        let results = store
            .search_beads("rosary-abc123", "repo", 10)
            .await
            .unwrap();
        assert_eq!(results.len(), 1);
        assert_eq!(results[0].id, "rosary-abc123");

        // Partial ID prefix match
        let results = store.search_beads("rosary-", "repo", 10).await.unwrap();
        assert_eq!(results.len(), 2);
    }

    #[tokio::test]
    async fn search_beads_fts_stemming() {
        let store = test_store();
        store
            .create_bead(
                "a",
                "Dispatch agent workers",
                "Fix dispatching logic",
                1,
                "bug",
            )
            .await
            .unwrap();
        store
            .create_bead("b", "Add feature X", "Unrelated", 2, "feature")
            .await
            .unwrap();

        // Porter stemmer: "dispatching" matches "dispatch"
        let results = store
            .search_beads_fts("dispatch", "repo", 10)
            .await
            .unwrap();
        assert_eq!(results.len(), 1, "FTS should find stemmed match");
        assert_eq!(results[0].id, "a");

        // Multi-word: both terms must appear
        let results = store
            .search_beads_fts("dispatch workers", "repo", 10)
            .await
            .unwrap();
        assert_eq!(results.len(), 1);

        // No match for unrelated term
        let results = store
            .search_beads_fts("nonexistent", "repo", 10)
            .await
            .unwrap();
        assert!(results.is_empty());
    }

    #[tokio::test]
    async fn search_excludes_closed_deps_from_dependency_count() {
        let store = test_store();
        // Create dep bead and main bead with a dependency on it.
        store
            .create_bead("dep", "Dep", "", 1, "task")
            .await
            .unwrap();
        store
            .create_bead("main", "Main", "", 1, "task")
            .await
            .unwrap();
        store.add_dependency("main", "dep").await.unwrap();

        // Before closing dep: search should show dependency_count = 1 (blocked).
        let results = store.search_beads("Main", "repo", 10).await.unwrap();
        assert_eq!(results.len(), 1);
        assert_eq!(results[0].dependency_count, 1);
        assert!(results[0].is_blocked());

        // After closing dep: search should show dependency_count = 0 (unblocked).
        store.close_bead("dep").await.unwrap();
        let results = store.search_beads("Main", "repo", 10).await.unwrap();
        assert_eq!(results.len(), 1);
        assert_eq!(results[0].dependency_count, 0);
        assert!(!results[0].is_blocked());
    }

    #[tokio::test]
    async fn update_status_and_close() {
        let store = test_store();
        store.create_bead("x", "Test", "", 1, "task").await.unwrap();

        store.update_status("x", "dispatched").await.unwrap();
        assert_eq!(
            store.get_status("x").await.unwrap().as_deref(),
            Some("dispatched")
        );

        store.close_bead("x").await.unwrap();
        assert_eq!(
            store.get_status("x").await.unwrap().as_deref(),
            Some("closed")
        );
    }

    #[tokio::test]
    async fn comments_and_events() {
        let store = test_store();
        store.create_bead("c", "Test", "", 1, "task").await.unwrap();

        store
            .add_comment("c", "progress note", "dev-agent")
            .await
            .unwrap();
        store.log_event("c", "dispatched", "agent started").await;

        let event = store.get_latest_event("c", "dispatched").await.unwrap();
        assert_eq!(event.as_deref(), Some("agent started"));
    }

    /// rosary-a96b06: list returns audit-trail-aware comments.
    #[tokio::test]
    async fn list_comments_oldest_first_excludes_deleted_by_default() {
        let store = test_store();
        store.create_bead("c", "T", "", 1, "task").await.unwrap();
        store.add_comment("c", "first", "alice").await.unwrap();
        store.add_comment("c", "second", "bob").await.unwrap();
        store.add_comment("c", "third", "carol").await.unwrap();

        let listed = store.list_comments("c", false).await.unwrap();
        assert_eq!(listed.len(), 3);
        assert_eq!(listed[0].text, "first");
        assert_eq!(listed[2].text, "third");

        // Soft-delete the middle one.
        let mid_id = listed[1].id.clone();
        store.delete_comment(&mid_id, None).await.unwrap();

        let visible = store.list_comments("c", false).await.unwrap();
        assert_eq!(visible.len(), 2);
        assert!(visible.iter().all(|c| !c.is_deleted()));

        let all = store.list_comments("c", true).await.unwrap();
        assert_eq!(all.len(), 3);
        let deleted: Vec<_> = all.iter().filter(|c| c.is_deleted()).collect();
        assert_eq!(deleted.len(), 1);
        assert_eq!(deleted[0].id, mid_id);
    }

    /// rosary-a96b06: first edit captures original_text; subsequent edits do not.
    #[tokio::test]
    async fn update_comment_first_edit_captures_original_text() {
        let store = test_store();
        store.create_bead("c", "T", "", 1, "task").await.unwrap();
        store
            .add_comment("c", "the original body", "u")
            .await
            .unwrap();
        let listed = store.list_comments("c", false).await.unwrap();
        let cid = listed[0].id.clone();

        let edited1 = store
            .update_comment(&cid, "first revision", Some("typo fix"))
            .await
            .unwrap();
        assert_eq!(edited1.text, "first revision");
        assert_eq!(
            edited1.original_text.as_deref(),
            Some("the original body"),
            "first edit must capture the prior body in original_text",
        );
        assert_eq!(edited1.edit_reason.as_deref(), Some("typo fix"));
        assert!(edited1.is_edited());

        // Second edit must NOT overwrite original_text.
        let edited2 = store
            .update_comment(&cid, "second revision", Some("clarify"))
            .await
            .unwrap();
        assert_eq!(edited2.text, "second revision");
        assert_eq!(
            edited2.original_text.as_deref(),
            Some("the original body"),
            "subsequent edits must NOT rewrite original_text — audit-trail invariant",
        );
        assert_eq!(edited2.edit_reason.as_deref(), Some("clarify"));
    }

    /// rosary-a96b06: update on non-existent id errors cleanly.
    #[tokio::test]
    async fn update_comment_nonexistent_errors() {
        let store = test_store();
        let r = store.update_comment("99999", "body", None).await;
        assert!(r.is_err());
        let msg = r.unwrap_err().to_string();
        assert!(msg.contains("99999") && msg.contains("not found"));
    }

    /// rosary-a96b06 regression (Copilot review on PR #188): the deletion
    /// reason must land in its own column so it survives even when the
    /// comment was previously edited. The first cut overloaded
    /// `edit_reason` via `COALESCE(edit_reason, ?)`, which silently dropped
    /// the deletion reason whenever an edit had already populated it.
    #[tokio::test]
    async fn delete_reason_survives_prior_edit() {
        let store = test_store();
        store.create_bead("c", "T", "", 1, "task").await.unwrap();
        store.add_comment("c", "v1", "u").await.unwrap();
        let cid = store.list_comments("c", false).await.unwrap()[0].id.clone();

        // Edit first, with an edit reason.
        let edited = store
            .update_comment(&cid, "v2", Some("typo fix"))
            .await
            .unwrap();
        assert_eq!(edited.edit_reason.as_deref(), Some("typo fix"));
        assert!(edited.delete_reason.is_none());

        // Now soft-delete with a deletion reason. This is the case the
        // first implementation got wrong.
        store
            .delete_comment(&cid, Some("no longer relevant"))
            .await
            .unwrap();

        let after = store.list_comments("c", true).await.unwrap();
        assert_eq!(after.len(), 1);
        let c = &after[0];
        assert!(c.is_deleted());
        assert_eq!(
            c.edit_reason.as_deref(),
            Some("typo fix"),
            "edit_reason must be preserved verbatim from the prior edit",
        );
        assert_eq!(
            c.delete_reason.as_deref(),
            Some("no longer relevant"),
            "delete_reason must be recorded in its dedicated column, not lost",
        );
    }

    /// rosary-a96b06: soft-delete preserves the row + audit trail; hard-delete removes it.
    #[tokio::test]
    async fn soft_delete_preserves_audit_trail_hard_delete_removes_row() {
        let store = test_store();
        store.create_bead("c", "T", "", 1, "task").await.unwrap();
        store
            .add_comment("c", "/Users/alice/leak", "u")
            .await
            .unwrap();
        let cid = store.list_comments("c", false).await.unwrap()[0].id.clone();

        // Soft-delete with a reason.
        store
            .delete_comment(&cid, Some("contains absolute path"))
            .await
            .unwrap();
        let after_soft = store.list_comments("c", true).await.unwrap();
        assert_eq!(after_soft.len(), 1);
        assert!(after_soft[0].is_deleted());
        // Reason lands in delete_reason (the dedicated column), NOT
        // edit_reason. This preserves it across previously-edited comments
        // — see delete_reason_survives_prior_edit for the regression case.
        assert_eq!(
            after_soft[0].delete_reason.as_deref(),
            Some("contains absolute path"),
        );
        assert!(
            after_soft[0].edit_reason.is_none(),
            "edit_reason must not be touched by delete",
        );

        // Soft-delete is idempotent.
        store.delete_comment(&cid, None).await.unwrap();

        // Hard-delete actually removes the row.
        store.hard_delete_comment(&cid).await.unwrap();
        let after_hard = store.list_comments("c", true).await.unwrap();
        assert!(after_hard.is_empty());

        // Hard-delete on a missing id errors cleanly.
        let r = store.hard_delete_comment(&cid).await;
        assert!(r.is_err());
    }

    #[tokio::test]
    async fn log_event_synthetic_id_skips_resolution() {
        // IDs starting with `_` are synthetic (e.g. `_schema` for migration
        // records). They must not be resolved against the issues table —
        // before the fix, every migration logged "bead not found: _schema"
        // and the audit row was silently dropped.
        let store = test_store();
        // No bead created — synthetic ID has no real bead behind it.
        store.log_event("_schema", "migration", "001_initial").await;

        // The event should still be visible via direct query (resolve_id
        // would fail on a synthetic ID, but the row was written).
        let conn = store.conn.lock().unwrap();
        let count: i64 = conn
            .query_row(
                "SELECT COUNT(*) FROM events WHERE issue_id = '_schema'",
                [],
                |row| row.get(0),
            )
            .unwrap();
        assert_eq!(count, 1, "synthetic _schema event must be persisted");
    }

    #[tokio::test]
    async fn external_ref_roundtrip() {
        let store = test_store();
        store.create_bead("e", "Test", "", 1, "task").await.unwrap();
        store.set_external_ref("e", "AGE-42").await.unwrap();

        assert_eq!(
            store.get_external_ref("e").await.unwrap().as_deref(),
            Some("AGE-42")
        );
        assert_eq!(
            store
                .find_by_external_ref("AGE-42")
                .await
                .unwrap()
                .as_deref(),
            Some("e")
        );
    }

    #[tokio::test]
    async fn dependency_lifecycle() {
        let store = test_store();
        store.create_bead("a", "A", "", 1, "task").await.unwrap();
        store.create_bead("b", "B", "", 1, "task").await.unwrap();

        store.add_dependency("b", "a").await.unwrap();
        assert_eq!(store.get_dependencies("b").await.unwrap(), vec!["a"]);
        assert_eq!(store.get_dependents("a").await.unwrap(), vec!["b"]);

        store.remove_dependency("b", "a").await.unwrap();
        assert!(store.get_dependencies("b").await.unwrap().is_empty());
    }

    #[tokio::test]
    async fn update_bead_fields() {
        let store = test_store();
        store
            .create_bead("u", "Original", "", 2, "task")
            .await
            .unwrap();

        let update = BeadUpdate {
            title: Some("Updated".into()),
            priority: Some(1),
            ..Default::default()
        };
        let fields = store.update_bead_fields("u", &update).await.unwrap();
        assert!(fields.contains(&"title".to_string()));
        assert!(fields.contains(&"priority".to_string()));

        let bead = store.get_bead("u", "repo").await.unwrap().unwrap();
        assert_eq!(bead.title, "Updated");
        assert_eq!(bead.priority, 1);
    }

    #[tokio::test]
    async fn created_by_and_scope_round_trip() {
        let store = test_store();
        store
            .create_bead_full(
                "s-1",
                "Scoped bead",
                "desc",
                2,
                "task",
                "agent",
                &[],
                &[],
                &[],
                Some("alice"),
                "payments",
                &[],
            )
            .await
            .unwrap();

        let bead = store.get_bead("s-1", "repo").await.unwrap().unwrap();
        assert_eq!(bead.created_by.as_deref(), Some("alice"));
        assert_eq!(bead.scope, "payments");
    }

    #[tokio::test]
    async fn scope_defaults_to_empty_for_simple_create() {
        let store = test_store();
        store
            .create_bead("plain", "Plain bead", "", 1, "task")
            .await
            .unwrap();

        let bead = store.get_bead("plain", "repo").await.unwrap().unwrap();
        assert_eq!(bead.scope, "");
        assert_eq!(bead.created_by, None);
    }

    #[tokio::test]
    async fn scope_appears_in_list_beads() {
        let store = test_store();
        store
            .create_bead_full(
                "ls-1",
                "Listed bead",
                "",
                2,
                "task",
                "",
                &[],
                &[],
                &[],
                Some("bob"),
                "auth",
                &[],
            )
            .await
            .unwrap();

        let beads = store.list_beads("repo").await.unwrap();
        assert_eq!(beads.len(), 1);
        assert_eq!(beads[0].scope, "auth");
        assert_eq!(beads[0].created_by.as_deref(), Some("bob"));
    }

    #[tokio::test]
    async fn derived_from_round_trip() {
        use bdr::provenance::ProvenanceRef;

        let store = test_store();
        let provenance = vec![
            ProvenanceRef::Adr {
                id: "ADR-007".into(),
            },
            ProvenanceRef::Doc {
                path: "docs/spec.md".into(),
            },
        ];
        store
            .create_bead_full(
                "prov-1",
                "Provenance bead",
                "desc",
                2,
                "task",
                "agent",
                &[],
                &[],
                &[],
                None,
                "",
                &provenance,
            )
            .await
            .unwrap();

        let bead = store.get_bead("prov-1", "repo").await.unwrap().unwrap();
        assert_eq!(bead.derived_from.len(), 2);
        assert_eq!(bead.derived_from[0].label(), "adr:ADR-007");
        assert_eq!(bead.derived_from[1].label(), "doc:docs/spec.md");
    }
}
