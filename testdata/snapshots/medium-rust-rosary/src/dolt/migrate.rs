#![allow(dead_code)] // Dolt migrations — kept for repos still using Dolt backend
//! Schema migrations for beads databases.
//!
//! Migrations are versioned SQL statements applied in order. Each migration
//! runs exactly once per database (tracked via an event with type "migration").
//! Designed for Dolt's `CREATE TABLE IF NOT EXISTS` pattern — migrations
//! handle ALTER TABLE and new columns that can't use IF NOT EXISTS.

use anyhow::{Context, Result};
use sqlx_core::query::query;
use sqlx_core::row::Row;

use super::DoltClient;

/// A schema migration with a unique version tag and SQL to execute.
struct Migration {
    /// Unique version string (e.g., "001_add_user_id"). Must be stable.
    version: &'static str,
    /// SQL statement(s) to execute. Use `;` to separate multiple statements.
    sql: &'static str,
    /// Human-readable description for logging.
    description: &'static str,
    /// Optional SELECT that must succeed after apply.
    /// Catches partial applies (migration marked done but schema still wrong).
    verify: Option<&'static str>,
}

/// All migrations in order. Append new ones at the end — never reorder or remove.
const MIGRATIONS: &[Migration] = &[
    Migration {
        version: "001_add_user_id",
        sql: "ALTER TABLE issues ADD COLUMN user_id VARCHAR(128) DEFAULT NULL",
        description: "Add user_id column for multi-tenant scoping",
        verify: None,
    },
    Migration {
        version: "002_observations",
        sql: "CREATE TABLE IF NOT EXISTS observations (
            bead_id VARCHAR(128) NOT NULL,
            agent VARCHAR(128) NOT NULL,
            phase INT NOT NULL DEFAULT 0,
            verdict VARCHAR(32) NOT NULL,
            detail TEXT DEFAULT '',
            content_hash VARCHAR(64) DEFAULT '',
            created_at DATETIME NOT NULL,
            INDEX idx_bead_id (bead_id),
            INDEX idx_verdict (verdict)
        )",
        description: "Append-only agent observations for CRDT-lattice bead state (rosary-45518d)",
        verify: None,
    },
    Migration {
        version: "003_add_created_by",
        sql: "ALTER TABLE issues ADD COLUMN created_by VARCHAR(255) DEFAULT NULL",
        description: "Git username of bead creator captured at creation time",
        verify: None,
    },
    Migration {
        version: "004_add_scope",
        sql: "ALTER TABLE issues ADD COLUMN scope VARCHAR(255) NOT NULL DEFAULT ''",
        description: "Team/folder scope for monorepo multi-team contexts",
        verify: None,
    },
    Migration {
        version: "005_comments_audit_columns",
        // DEFAULT (uuid()) expression is unreliable across Dolt versions. Use an
        // explicit UPDATE to backfill existing rows instead of a column default.
        // Step order matters: nullable ADD → backfill → MODIFY NOT NULL → PK.
        sql: "ALTER TABLE comments ADD COLUMN id CHAR(36) FIRST;
              UPDATE comments SET id = UUID() WHERE id IS NULL OR id = '';
              ALTER TABLE comments MODIFY COLUMN id CHAR(36) NOT NULL;
              ALTER TABLE comments ADD PRIMARY KEY (id);
              ALTER TABLE comments ADD COLUMN edited_at DATETIME NULL;
              ALTER TABLE comments ADD COLUMN edit_reason TEXT NULL;
              ALTER TABLE comments ADD COLUMN original_text TEXT NULL;
              ALTER TABLE comments ADD COLUMN deleted_at DATETIME NULL;
              ALTER TABLE comments ADD COLUMN delete_reason TEXT NULL",
        description: "Audit-trail columns + UUID id for comment update/delete (rosary-a96b06)",
        verify: Some(
            "SELECT id, edited_at, edit_reason, original_text, deleted_at, delete_reason \
             FROM comments LIMIT 0",
        ),
    },
    Migration {
        version: "006_drop_events_issue_fk",
        // The events table is best-effort audit logging. Synthetic IDs like
        // `_schema` (used to record migration application) violate the FK on
        // every rsry invocation, producing a noisy warning. Dropping the FK
        // is the right call: events is append-only audit, orphaned rows
        // after a hypothetical bead delete are harmless, and the SQLite
        // backend never had this constraint either. The IF EXISTS guard is
        // for older Dolt versions where the constraint may not be present.
        sql: "ALTER TABLE events DROP FOREIGN KEY fk_events_issue",
        description: "Drop fk_events_issue so synthetic _schema event log stops warning",
        verify: None,
    },
];

/// True when a DDL error means "schema already in the post-apply state" —
/// the statement is a duplicate of work already done. Anchored on the
/// structural error strings Dolt emits (both MySQL-classic and Dolt 2.x
/// variants); broad keywords like bare "already exists" / "does not exist"
/// are deliberately excluded because they masked real failures (e.g.
/// `DEFAULT (uuid())` unsupported) before being tightened.
fn is_already_applied_error(err: &str) -> bool {
    let s = err.to_lowercase();
    s.contains("duplicate column name")
        || s.contains("duplicate key name")
        || s.contains("multiple primary key")
        // Dolt 2.x form: `Column "user_id" already exists`.
        || (s.contains("column") && s.contains("already exists"))
        // DROP FOREIGN KEY on one that was already dropped / never existed.
        || (s.contains("can't drop")
            && (s.contains("doesn't exist") || s.contains("does not exist")))
        // Dolt-specific FK missing: "foreign key `name` was not found".
        || (s.contains("foreign key") && s.contains("was not found"))
}

impl DoltClient {
    /// Run all pending migrations on this database.
    /// Idempotent — skips already-applied migrations. When an already-applied
    /// migration's verify SQL fails (partial-apply state from rosary-b61362),
    /// re-runs the migration's SQL idempotently and re-verifies; loud-fails
    /// if the schema is still broken after the repair.
    pub async fn migrate(&self) -> Result<Vec<String>> {
        let mut applied = Vec::new();

        for migration in MIGRATIONS {
            if self.migration_applied(migration.version).await? {
                self.verify_or_repair(migration).await?;
                continue;
            }

            eprintln!(
                "[migrate] applying {} — {}",
                migration.version, migration.description
            );

            self.apply_migration_sql(migration).await?;

            // Verify schema is actually in place before recording as applied.
            // This prevents partial applies from silently being marked done.
            if let Some(verify_sql) = migration.verify {
                query(verify_sql)
                    .execute(&self.pool)
                    .await
                    .with_context(|| {
                        format!(
                            "migration {} verify failed — schema not fully applied",
                            migration.version
                        )
                    })?;
            }

            // Record migration in events table
            self.log_event("_schema", "migration", migration.version)
                .await;

            applied.push(migration.version.to_string());
            eprintln!("[migrate] applied {}", migration.version);
        }

        Ok(applied)
    }

    /// Check if a migration has already been applied.
    async fn migration_applied(&self, version: &str) -> Result<bool> {
        let row = query(
            "SELECT COUNT(*) as cnt FROM events WHERE event_type = 'migration' AND comment = ?",
        )
        .bind(version)
        .fetch_one(&self.pool)
        .await
        .context("checking migration status")?;

        let count: i64 = row.try_get("cnt").unwrap_or(0);
        Ok(count > 0)
    }

    /// Apply a migration's SQL statements, tolerating duplicate-shape errors
    /// as idempotent no-ops. Shared between first-apply and verify-failure
    /// repair so both paths use the same already-applied heuristic.
    async fn apply_migration_sql(&self, migration: &Migration) -> Result<()> {
        for stmt in migration
            .sql
            .split(';')
            .map(str::trim)
            .filter(|s| !s.is_empty())
        {
            if let Err(e) = query(stmt).execute(&self.pool).await {
                if is_already_applied_error(&e.to_string()) {
                    eprintln!(
                        "[migrate] {}: already applied (idempotent)",
                        migration.version
                    );
                } else {
                    return Err(e).with_context(|| {
                        format!("migration {} failed: {stmt}", migration.version)
                    });
                }
            }
        }
        // Force a Dolt commit so DDL applied after intermediate
        // `already-applied` errors becomes visible to subsequent verify
        // queries on this pool. Without this, statement N's successful
        // ADD COLUMN can land in a per-session state that doesn't reach
        // the next pooled query (observed on Dolt 1.x with mixed-result
        // multi-statement migrations — rosary-b61362).
        //
        // Log on failure rather than swallow silently: a failed commit
        // here surfaces downstream as a verify error with a confusing
        // schema message, but the *cause* is the commit. Logging
        // preserves the real cause for operators while letting the
        // verify path proceed (it'll fail loudly with its own context
        // if the commit genuinely didn't land).
        if let Err(e) = query("CALL DOLT_COMMIT('-Am', 'migrate apply', '--allow-empty')")
            .execute(&self.pool)
            .await
        {
            eprintln!(
                "[migrate] {} DOLT_COMMIT after apply failed: {e} — verify may report a misleading schema error",
                migration.version
            );
        }
        Ok(())
    }

    /// When a migration is marked applied but its verify SQL fails, the
    /// live schema diverged from the recorded state (e.g. a column never
    /// landed — rosary-b61362). Re-apply the migration idempotently and
    /// re-verify. If verify still fails after re-apply, the divergence is
    /// not auto-repairable and we propagate the error rather than silently
    /// continuing with a known-broken schema.
    async fn verify_or_repair(&self, migration: &Migration) -> Result<()> {
        let Some(verify_sql) = migration.verify else {
            return Ok(());
        };
        if query(verify_sql).execute(&self.pool).await.is_ok() {
            return Ok(());
        }
        eprintln!(
            "[migrate] {} marked applied but verify failed — re-applying to repair schema",
            migration.version
        );
        self.apply_migration_sql(migration).await?;
        query(verify_sql)
            .execute(&self.pool)
            .await
            .with_context(|| {
                format!(
                    "migration {} repair failed — verify still rejects after re-apply",
                    migration.version
                )
            })?;
        eprintln!("[migrate] {} repaired", migration.version);
        Ok(())
    }
}

#[cfg(test)]
mod heuristic_tests {
    use super::is_already_applied_error;

    #[test]
    fn classic_mysql_duplicates_are_tolerated() {
        assert!(is_already_applied_error("Duplicate column name 'user_id'"));
        assert!(is_already_applied_error("Duplicate key name 'idx_x'"));
        assert!(is_already_applied_error("Multiple primary key defined"));
    }

    #[test]
    fn dolt_2x_duplicate_column_form_is_tolerated() {
        // Dolt 2.x emits `Column "x" already exists` — different shape from
        // the MySQL-classic `Duplicate column name 'x'` and both must match.
        assert!(is_already_applied_error(
            "1105 (HY000): Column \"edited_at\" already exists"
        ));
    }

    #[test]
    fn missing_foreign_key_drops_are_tolerated() {
        // MySQL-style: "Can't DROP 'fk_x'; check that column/key exists".
        assert!(is_already_applied_error(
            "Can't DROP 'fk_events_issue'; it doesn't exist"
        ));
        assert!(is_already_applied_error(
            "Can't DROP CONSTRAINT 'fk'; it does not exist"
        ));
        // Dolt-specific: foreign key by name was not found.
        assert!(is_already_applied_error(
            "foreign key `fk_events_issue` was not found"
        ));
    }

    #[test]
    fn unrelated_errors_propagate() {
        // Real failures must NOT be silently swallowed — the heuristic
        // exists to tolerate idempotent re-apply, not mask real bugs.
        assert!(!is_already_applied_error("syntax error near 'FRIST'"));
        assert!(!is_already_applied_error("table comments does not exist"));
        assert!(!is_already_applied_error(
            "DEFAULT (uuid()) is not supported"
        ));
        // "Already exists" without "column" anchor must NOT match — that
        // was the over-broad form the heuristic was tightened against.
        assert!(!is_already_applied_error("Trigger 'foo' already exists"));
    }
}
