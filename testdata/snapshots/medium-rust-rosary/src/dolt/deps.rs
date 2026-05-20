use anyhow::{Context, Result};
use sqlx_core::query::query;
use sqlx_core::row::Row;

use super::DoltClient;

impl DoltClient {
    /// Add a dependency: `issue_id` depends on `depends_on_id`.
    pub async fn add_dependency(&self, issue_id: &str, depends_on_id: &str) -> Result<()> {
        let full_issue = self.resolve_id(issue_id).await?;
        let full_dep = self.resolve_id(depends_on_id).await?;
        query("INSERT IGNORE INTO dependencies (issue_id, depends_on_id) VALUES (?, ?)")
            .bind(&full_issue)
            .bind(&full_dep)
            .execute(&self.pool)
            .await
            .with_context(|| format!("adding dependency {issue_id} → {depends_on_id}"))?;
        self.auto_commit(&format!("dep {full_issue} → {full_dep}"))
            .await;
        Ok(())
    }

    /// Remove a dependency.
    pub async fn remove_dependency(&self, issue_id: &str, depends_on_id: &str) -> Result<()> {
        let full_issue = self.resolve_id(issue_id).await?;
        let full_dep = self.resolve_id(depends_on_id).await?;
        query("DELETE FROM dependencies WHERE issue_id = ? AND depends_on_id = ?")
            .bind(&full_issue)
            .bind(&full_dep)
            .execute(&self.pool)
            .await
            .with_context(|| format!("removing dependency {issue_id} → {depends_on_id}"))?;
        self.auto_commit(&format!("undep {full_issue} → {full_dep}"))
            .await;
        Ok(())
    }

    /// List dependencies of a bead (what it depends ON).
    #[allow(dead_code)] // API surface — used by future MCP tools and reconciler dep checks
    pub async fn get_dependencies(&self, issue_id: &str) -> Result<Vec<String>> {
        let full_id = match self.resolve_id(issue_id).await {
            Ok(id) => id,
            Err(_) => return Ok(vec![]),
        };
        let rows = query("SELECT depends_on_id FROM dependencies WHERE issue_id = ?")
            .bind(&full_id)
            .fetch_all(&self.pool)
            .await
            .with_context(|| format!("listing dependencies for {issue_id}"))?;
        Ok(rows
            .iter()
            .map(|r| r.get::<String, _>("depends_on_id"))
            .collect())
    }

    /// List dependents of a bead (what depends on IT).
    #[allow(dead_code)] // API surface — used by future MCP tools and reconciler dep checks
    pub async fn get_dependents(&self, issue_id: &str) -> Result<Vec<String>> {
        let full_id = match self.resolve_id(issue_id).await {
            Ok(id) => id,
            Err(_) => return Ok(vec![]),
        };
        let rows = query("SELECT issue_id FROM dependencies WHERE depends_on_id = ?")
            .bind(&full_id)
            .fetch_all(&self.pool)
            .await
            .with_context(|| format!("listing dependents for {issue_id}"))?;
        Ok(rows
            .iter()
            .map(|r| r.get::<String, _>("issue_id"))
            .collect())
    }

    /// Add a comment to an issue.
    ///
    /// Generates the comment `id` in Rust (UUID v4) and supplies it explicitly
    /// at INSERT time. Migration 005 (rosary-a96b06) makes `comments.id` a
    /// `CHAR(36) NOT NULL PRIMARY KEY` with no SQL-side default — the previous
    /// `DEFAULT (uuid())` was unreliable across Dolt versions and was removed in
    /// rosary-5ad4b0. The Rust-side generation keeps INSERT paths working after
    /// the migration ships.
    pub async fn add_comment(&self, issue_id: &str, body: &str, author: &str) -> Result<()> {
        let full_id = self.resolve_id(issue_id).await?;
        let body = crate::secrets::scrub_and_warn(body, &format!("comment on {issue_id}"));
        let comment_id = uuid::Uuid::new_v4().to_string();
        query("INSERT INTO comments (id, issue_id, text, author, created_at) VALUES (?, ?, ?, ?, NOW())")
            .bind(&comment_id)
            .bind(&full_id)
            .bind(&body)
            .bind(author)
            .execute(&self.pool)
            .await
            .with_context(|| format!("adding comment to {issue_id}"))?;
        self.auto_commit(&format!("comment on {full_id}")).await;
        Ok(())
    }

    /// List comments for an issue. Soft-deleted comments are omitted unless
    /// `include_deleted` is true. Returns oldest-first.
    pub async fn list_comments(
        &self,
        issue_id: &str,
        include_deleted: bool,
    ) -> Result<Vec<crate::bead::Comment>> {
        use sqlx_core::row::Row;
        let full_id = self.resolve_id(issue_id).await?;
        let sql = if include_deleted {
            "SELECT id, issue_id, text, author, created_at, edited_at, edit_reason,
                    original_text, deleted_at, delete_reason
             FROM comments WHERE issue_id = ? ORDER BY created_at ASC, id ASC"
        } else {
            "SELECT id, issue_id, text, author, created_at, edited_at, edit_reason,
                    original_text, deleted_at, delete_reason
             FROM comments WHERE issue_id = ? AND deleted_at IS NULL
             ORDER BY created_at ASC, id ASC"
        };
        let rows = query(sql)
            .bind(&full_id)
            .fetch_all(&self.pool)
            .await
            .with_context(|| format!("listing comments for {issue_id}"))?;
        Ok(rows
            .iter()
            .map(|r| crate::bead::Comment {
                id: r.try_get::<String, _>("id").unwrap_or_default(),
                issue_id: r.try_get("issue_id").unwrap_or_default(),
                text: r.try_get("text").unwrap_or_default(),
                author: r.try_get("author").unwrap_or_default(),
                created_at: r
                    .try_get("created_at")
                    .unwrap_or_else(|_| chrono::Utc::now()),
                edited_at: r.try_get("edited_at").ok(),
                edit_reason: r.try_get::<Option<String>, _>("edit_reason").ok().flatten(),
                original_text: r
                    .try_get::<Option<String>, _>("original_text")
                    .ok()
                    .flatten(),
                deleted_at: r.try_get("deleted_at").ok(),
                delete_reason: r
                    .try_get::<Option<String>, _>("delete_reason")
                    .ok()
                    .flatten(),
            })
            .collect())
    }

    /// Update a comment's body. Captures `original_text` on first edit
    /// (immutable thereafter); records `edited_at` and `edit_reason`.
    /// Returns the updated comment. Errors if `comment_id` does not exist.
    pub async fn update_comment(
        &self,
        comment_id: &str,
        body: &str,
        reason: Option<&str>,
    ) -> Result<crate::bead::Comment> {
        use sqlx_core::row::Row;
        // Fetch current state to determine whether to capture original_text.
        let existing = query(
            "SELECT id, issue_id, text, author, created_at, edited_at, edit_reason,
                    original_text, deleted_at, delete_reason
             FROM comments WHERE id = ?",
        )
        .bind(comment_id)
        .fetch_optional(&self.pool)
        .await
        .with_context(|| format!("looking up comment {comment_id}"))?
        .ok_or_else(|| anyhow::anyhow!("comment {comment_id} not found"))?;

        let issue_id: String = existing.try_get("issue_id").unwrap_or_default();
        let prior_text: String = existing.try_get("text").unwrap_or_default();
        let prior_original: Option<String> = existing
            .try_get::<Option<String>, _>("original_text")
            .ok()
            .flatten();

        let scrubbed =
            crate::secrets::scrub_and_warn(body, &format!("comment update {comment_id}"));

        // First-edit captures original_text. Subsequent edits leave it alone.
        if prior_original.is_none() {
            query(
                "UPDATE comments
                 SET text = ?, edited_at = NOW(), edit_reason = ?, original_text = ?
                 WHERE id = ?",
            )
            .bind(&scrubbed)
            .bind(reason)
            .bind(&prior_text)
            .bind(comment_id)
            .execute(&self.pool)
            .await
            .with_context(|| format!("updating comment {comment_id}"))?;
        } else {
            query(
                "UPDATE comments
                 SET text = ?, edited_at = NOW(), edit_reason = ?
                 WHERE id = ?",
            )
            .bind(&scrubbed)
            .bind(reason)
            .bind(comment_id)
            .execute(&self.pool)
            .await
            .with_context(|| format!("updating comment {comment_id}"))?;
        }

        self.auto_commit(&format!("comment update {comment_id} on {issue_id}"))
            .await;

        // Return the post-update row.
        let updated = query(
            "SELECT id, issue_id, text, author, created_at, edited_at, edit_reason,
                    original_text, deleted_at, delete_reason
             FROM comments WHERE id = ?",
        )
        .bind(comment_id)
        .fetch_one(&self.pool)
        .await
        .with_context(|| format!("re-fetching comment {comment_id}"))?;

        Ok(crate::bead::Comment {
            id: updated
                .try_get::<String, _>("id")
                .unwrap_or_else(|_| comment_id.to_string()),
            issue_id: updated.try_get("issue_id").unwrap_or_default(),
            text: updated.try_get("text").unwrap_or_default(),
            author: updated.try_get("author").unwrap_or_default(),
            created_at: updated
                .try_get("created_at")
                .unwrap_or_else(|_| chrono::Utc::now()),
            edited_at: updated.try_get("edited_at").ok(),
            edit_reason: updated
                .try_get::<Option<String>, _>("edit_reason")
                .ok()
                .flatten(),
            original_text: updated
                .try_get::<Option<String>, _>("original_text")
                .ok()
                .flatten(),
            deleted_at: updated.try_get("deleted_at").ok(),
            delete_reason: updated
                .try_get::<Option<String>, _>("delete_reason")
                .ok()
                .flatten(),
        })
    }

    /// Soft-delete a comment. Idempotent (re-deleting refreshes timestamp).
    /// Errors if the comment does not exist.
    pub async fn delete_comment(&self, comment_id: &str, reason: Option<&str>) -> Result<()> {
        // Confirm existence first so we can report a clean error.
        let exists: i64 = query("SELECT COUNT(*) AS cnt FROM comments WHERE id = ?")
            .bind(comment_id)
            .fetch_one(&self.pool)
            .await
            .with_context(|| format!("looking up comment {comment_id}"))?
            .try_get("cnt")
            .unwrap_or(0);
        if exists == 0 {
            anyhow::bail!("comment {comment_id} not found");
        }

        // The deletion reason lands in `delete_reason` — its own column —
        // so it's preserved even when the comment was previously edited
        // (which would have populated `edit_reason`). Re-deletion overwrites
        // an earlier deletion's reason; the timestamp is also refreshed.
        query(
            "UPDATE comments
             SET deleted_at = NOW(), delete_reason = ?
             WHERE id = ?",
        )
        .bind(reason)
        .bind(comment_id)
        .execute(&self.pool)
        .await
        .with_context(|| format!("soft-deleting comment {comment_id}"))?;
        self.auto_commit(&format!("comment soft-delete {comment_id}"))
            .await;
        Ok(())
    }

    /// Hard-delete a comment — physically removes the row. CLI-only;
    /// destroys audit trail. Errors if the comment does not exist.
    pub async fn hard_delete_comment(&self, comment_id: &str) -> Result<()> {
        let result = query("DELETE FROM comments WHERE id = ?")
            .bind(comment_id)
            .execute(&self.pool)
            .await
            .with_context(|| format!("hard-deleting comment {comment_id}"))?;
        if result.rows_affected() == 0 {
            anyhow::bail!("comment {comment_id} not found");
        }
        self.auto_commit(&format!("comment hard-delete {comment_id}"))
            .await;
        Ok(())
    }
}
