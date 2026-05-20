use anyhow::{Context, Result};
use sqlx_core::query::query;
use sqlx_core::row::Row;

use super::DoltClient;

impl DoltClient {
    /// Parse files and test_files from the notes JSON column.
    pub(crate) fn parse_files_from_notes(row: &sqlx_mysql::MySqlRow) -> (Vec<String>, Vec<String>) {
        let notes: Option<String> = row.try_get("notes").ok();
        let parsed: Option<serde_json::Value> = notes.and_then(|s| serde_json::from_str(&s).ok());
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
    pub(crate) fn parse_derived_from_notes(
        row: &sqlx_mysql::MySqlRow,
    ) -> Vec<bdr::provenance::ProvenanceRef> {
        let notes: Option<String> = row.try_get("notes").ok();
        let parsed: Option<serde_json::Value> = notes.and_then(|s| serde_json::from_str(&s).ok());
        parsed
            .as_ref()
            .and_then(|v| v.get("derived_from"))
            .and_then(|v| serde_json::from_value(v.clone()).ok())
            .unwrap_or_default()
    }

    /// Enable dolt_transaction_commit at the server level (GLOBAL) so ALL
    /// connections auto-create Dolt commits. This handles multiple rsry
    /// processes sharing the same Dolt server (MCP stdio + HTTP + agent MCP).
    ///
    /// With this set, writes are immediately visible to other connections
    /// without a separate commit step — no data loss on timeout.
    pub(crate) async fn enable_auto_dolt_commit(&self) {
        let result = query("SET GLOBAL dolt_transaction_commit = 1")
            .execute(&self.pool)
            .await;
        if let Err(e) = result {
            eprintln!("[dolt] warning: failed to enable dolt_transaction_commit: {e}");
        }
    }

    /// Explicit Dolt commit — only used as fallback when dolt_transaction_commit
    /// is not available. Prefer enable_auto_dolt_commit() at connection time.
    pub(crate) async fn auto_commit(&self, _message: &str) {
        // No-op: dolt_transaction_commit handles this automatically.
        // If enable_auto_dolt_commit() failed at connect time, writes
        // are still visible within the same session but may not persist
        // across connections until the session closes.
    }

    /// Execute a raw SQL statement. Best-effort, for operations not covered by typed methods.
    pub async fn execute_raw(&self, sql: &str) -> anyhow::Result<()> {
        query(sql)
            .execute(&self.pool)
            .await
            .with_context(|| format!("executing raw SQL: {}", &sql[..sql.len().min(80)]))?;
        Ok(())
    }

    /// Log an event to the events table for audit trail.
    /// Best-effort: logs warning on failure rather than propagating error.
    ///
    /// IDs starting with `_` are treated as synthetic (e.g. `_schema` for
    /// migration records) and skip the issues-table resolution step.
    /// Without this, every migration logged a noisy "bead not found:
    /// _schema" warning and the audit row was silently dropped.
    pub async fn log_event(&self, issue_id: &str, event_type: &str, detail: &str) {
        let full_id = if issue_id.starts_with('_') {
            issue_id.to_string()
        } else {
            match self.resolve_id(issue_id).await {
                Ok(id) => id,
                Err(e) => {
                    eprintln!("warning: failed to resolve bead ID {issue_id} for event log: {e}");
                    return;
                }
            }
        };
        let result = query(
            "INSERT INTO events (issue_id, event_type, actor, comment, created_at) VALUES (?, ?, 'rosary', ?, NOW())",
        )
        .bind(&full_id)
        .bind(event_type)
        .bind(detail)
        .execute(&self.pool)
        .await;

        if let Err(e) = result {
            eprintln!("warning: failed to log event for {issue_id}: {e}");
        }
    }

    /// Get the most recent event detail for a bead + event type.
    /// Used by poll_pr_merges to find the PR URL recorded at dispatch time.
    pub async fn get_latest_event(
        &self,
        issue_id: &str,
        event_type: &str,
    ) -> Result<Option<String>> {
        let full_id = match self.resolve_id(issue_id).await {
            Ok(id) => id,
            Err(_) => return Ok(None),
        };
        let row = query(
            "SELECT comment FROM events WHERE issue_id = ? AND event_type = ? ORDER BY created_at DESC LIMIT 1",
        )
        .bind(&full_id)
        .bind(event_type)
        .fetch_optional(&self.pool)
        .await
        .with_context(|| format!("querying latest {event_type} event for {issue_id}"))?;

        Ok(row.map(|r| r.get("comment")))
    }
}
