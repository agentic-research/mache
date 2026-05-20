use anyhow::{Context, Result};
use sqlx_core::query::query;
use sqlx_core::row::Row;

use super::DoltClient;
use crate::bead::Bead;

impl DoltClient {
    /// Get the current status of a bead by ID.
    #[allow(dead_code)] // Used by is_bead_agent_closed (agent-first path)
    pub async fn get_status(&self, id: &str) -> Result<Option<String>> {
        let row = query("SELECT status FROM issues WHERE id = ?")
            .bind(id)
            .fetch_optional(&self.pool)
            .await
            .with_context(|| format!("querying status for {id}"))?;
        Ok(row.map(|r| r.get("status")))
    }

    #[allow(dead_code)] // API surface for rsry bead search
    /// Search beads by title or description substring match (case-insensitive).
    pub async fn search_beads(
        &self,
        query_str: &str,
        repo_name: &str,
        limit: u32,
    ) -> Result<Vec<Bead>> {
        let words: Vec<String> = query_str
            .split_whitespace()
            .map(|w| format!("%{}%", w.to_lowercase()))
            .collect();

        // Build WHERE clause: each word must appear in title, description, id,
        // OR a live (non-soft-deleted) comment. ID is included so that passing
        // a bead ID directly (e.g. "rosary-5ad4b0") surfaces an exact match
        // without requiring a separate lookup path. Comment-text matching is
        // what rosary-a9bc77 added — most bead context lives in comments and
        // wasn't previously searchable.
        let where_clause = if words.is_empty() {
            "1=1".to_string()
        } else {
            words
                .iter()
                .map(|_| {
                    "(LOWER(i.title) LIKE ? OR LOWER(i.description) LIKE ? OR i.id LIKE ? \
                      OR EXISTS (SELECT 1 FROM comments c \
                                 WHERE c.issue_id = i.id \
                                   AND c.deleted_at IS NULL \
                                   AND LOWER(c.text) LIKE ?))"
                        .to_string()
                })
                .collect::<Vec<_>>()
                .join(" AND ")
        };

        let sql = format!(
            r#"SELECT i.id, i.title, i.description, i.status, i.priority, i.issue_type,
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
               LIMIT {limit}"#,
        );

        let mut q = query(&sql);
        // Each word binds 4 times — title, description, id, comment text.
        for word in &words {
            q = q.bind(word).bind(word).bind(word).bind(word);
        }

        let rows = q
            .fetch_all(&self.pool)
            .await
            .with_context(|| format!("searching beads for '{query_str}'"))?;

        let beads = rows
            .iter()
            .map(|row| {
                let (files, test_files) = Self::parse_files_from_notes(row);
                Bead {
                    id: row.get("id"),
                    title: row.get("title"),
                    description: row.try_get("description").unwrap_or_default(),
                    status: row.get("status"),
                    priority: row.try_get::<i32, _>("priority").unwrap_or(2) as u8,
                    issue_type: row
                        .try_get("issue_type")
                        .unwrap_or_else(|_| "task".to_string()),
                    owner: row.try_get("assignee").ok(),
                    repo: repo_name.to_string(),
                    created_at: row.try_get("created_at").unwrap_or_default(),
                    updated_at: row.try_get("updated_at").unwrap_or_default(),
                    dependency_count: row.try_get::<i64, _>("dependency_count").unwrap_or(0) as u32,
                    dependent_count: row.try_get::<i64, _>("dep_count").unwrap_or(0) as u32,
                    comment_count: row.try_get::<i64, _>("comment_count").unwrap_or(0) as u32,
                    branch: None,
                    pr_url: None,
                    jj_change_id: None,
                    external_ref: row.try_get("external_ref").ok(),
                    files,
                    test_files,
                    created_by: row.try_get("created_by").ok(),
                    scope: row.try_get("scope").unwrap_or_default(),
                    derived_from: Self::parse_derived_from_notes(row),
                }
            })
            .collect();

        Ok(beads)
    }

    /// Get the external_ref for a bead (e.g., "AGE-5").
    /// Used by persist_status to mirror state transitions to Linear.
    pub async fn get_external_ref(&self, id: &str) -> Result<Option<String>> {
        let full_id = match self.resolve_id(id).await {
            Ok(id) => id,
            Err(_) => return Ok(None),
        };
        let row = query(
            "SELECT external_ref FROM issues WHERE id = ? AND external_ref IS NOT NULL AND external_ref != ''",
        )
        .bind(&full_id)
        .fetch_optional(&self.pool)
        .await
        .with_context(|| format!("getting external_ref for {id}"))?;
        Ok(row.and_then(|r| r.try_get::<String, _>("external_ref").ok()))
    }

    #[allow(dead_code)] // API surface — used by sync module
    /// Set the external_ref for a bead (e.g., Linear issue identifier like "AGE-5").
    pub async fn set_external_ref(&self, id: &str, external_ref: &str) -> Result<()> {
        let full_id = self.resolve_id(id).await?;
        query("UPDATE issues SET external_ref = ?, updated_at = NOW() WHERE id = ?")
            .bind(external_ref)
            .bind(&full_id)
            .execute(&self.pool)
            .await
            .with_context(|| format!("setting external_ref for {id}"))?;
        Ok(())
    }

    /// Find a bead by its external_ref (e.g., "AGE-5").
    /// Returns the bead ID if found. Used by the webhook handler to map
    /// Linear issue identifiers back to local beads.
    #[allow(dead_code)] // Called from serve.rs webhook handler
    pub async fn find_by_external_ref(&self, external_ref: &str) -> Result<Option<String>> {
        let row = query("SELECT id FROM issues WHERE external_ref = ?")
            .bind(external_ref)
            .fetch_optional(&self.pool)
            .await
            .with_context(|| format!("finding bead by external_ref {external_ref}"))?;
        Ok(row.map(|r| r.get("id")))
    }

    #[allow(dead_code)] // Called from linear.rs sync() — clippy can't trace async
    /// List closed beads that have an external_ref set.
    /// Used by sync to propagate close status to external trackers.
    pub async fn list_closed_linked_beads(&self, repo_name: &str) -> Result<Vec<Bead>> {
        let rows = query(
            r#"SELECT id, title, description, status, priority, issue_type,
                      assignee, external_ref, created_by, scope, created_at, updated_at,
                      0 as dep_count, 0 as dependency_count, 0 as comment_count
               FROM issues
               WHERE status = 'closed' AND external_ref IS NOT NULL AND external_ref != ''
               ORDER BY updated_at DESC
               LIMIT 500"#,
        )
        .fetch_all(&self.pool)
        .await
        .context("querying closed linked beads")?;

        let beads = rows
            .iter()
            .map(|row| Bead {
                id: row.get("id"),
                title: row.get("title"),
                description: row.try_get("description").unwrap_or_default(),
                status: row.get("status"),
                priority: row.try_get::<i32, _>("priority").unwrap_or(2) as u8,
                issue_type: row
                    .try_get("issue_type")
                    .unwrap_or_else(|_| "task".to_string()),
                owner: row.try_get("assignee").ok(),
                repo: repo_name.to_string(),
                created_at: row.try_get("created_at").unwrap_or_default(),
                updated_at: row.try_get("updated_at").unwrap_or_default(),
                dependency_count: row.try_get::<i64, _>("dependency_count").unwrap_or(0) as u32,
                dependent_count: row.try_get::<i64, _>("dep_count").unwrap_or(0) as u32,
                comment_count: row.try_get::<i64, _>("comment_count").unwrap_or(0) as u32,
                branch: None,
                pr_url: None,
                jj_change_id: None,
                external_ref: row.try_get("external_ref").ok(),
                files: Vec::new(),
                test_files: Vec::new(),
                created_by: row.try_get("created_by").ok(),
                scope: row.try_get("scope").unwrap_or_default(),
                derived_from: vec![],
            })
            .collect();

        Ok(beads)
    }
}
