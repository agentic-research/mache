use anyhow::{Context, Result};
use sqlx_core::query::query;
use sqlx_core::row::Row;

use super::DoltClient;
use crate::bead::Bead;

impl DoltClient {
    /// Resolve a short or full bead ID to the canonical full ID in the DB.
    /// Accepts exact match or suffix match ("2a3970" → "ley-line-open-2a3970").
    /// Errors on zero matches (not found) or multiple matches (ambiguous).
    pub async fn resolve_id(&self, id: &str) -> Result<String> {
        let exact = query("SELECT id FROM issues WHERE id = ?")
            .bind(id)
            .fetch_optional(&self.pool)
            .await?;
        if let Some(row) = exact {
            return Ok(row.get("id"));
        }
        let suffix_pattern = format!("%-{id}");
        let matches = query("SELECT id FROM issues WHERE id LIKE ?")
            .bind(&suffix_pattern)
            .fetch_all(&self.pool)
            .await?;
        match matches.len() {
            0 => anyhow::bail!("bead not found: {id}"),
            1 => Ok(matches[0].get("id")),
            _ => {
                let ids: Vec<String> = matches.iter().map(|r| r.get("id")).collect();
                anyhow::bail!("ambiguous bead ID {id:?} — matches: {}", ids.join(", "))
            }
        }
    }

    /// List all open issues as Beads.
    pub async fn list_beads(&self, repo_name: &str) -> Result<Vec<Bead>> {
        let rows = query(
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
                         JOIN issues dep_i ON dep_i.id = d.depends_on_id
                         WHERE dep_i.status NOT IN ('closed', 'done')
                         GROUP BY d.issue_id) deps
                    ON deps.issue_id = i.id
               LEFT JOIN (SELECT issue_id, COUNT(*) as cnt FROM comments GROUP BY issue_id) cmt
                    ON cmt.issue_id = i.id
               WHERE i.status NOT IN ('closed', 'done')
               ORDER BY i.priority ASC, i.created_at DESC"#,
        )
        .fetch_all(&self.pool)
        .await
        .context("querying issues")?;

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

    /// List beads filtered by user_id (multi-tenant).
    #[allow(dead_code)] // Used by MCP handlers when user_scope is set
    /// When user_id is Some, only returns beads owned by that user.
    /// When None, returns all (single-tenant / machine identity).
    pub async fn list_beads_scoped(
        &self,
        repo_name: &str,
        user_id: Option<&str>,
    ) -> Result<Vec<Bead>> {
        match user_id {
            Some(uid) => {
                let rows = query(
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
                                 JOIN issues dep_i ON dep_i.id = d.depends_on_id
                                 WHERE dep_i.status NOT IN ('closed', 'done')
                                 GROUP BY d.issue_id) deps
                            ON deps.issue_id = i.id
                       LEFT JOIN (SELECT issue_id, COUNT(*) as cnt FROM comments GROUP BY issue_id) cmt
                            ON cmt.issue_id = i.id
                       WHERE i.status NOT IN ('closed', 'done') AND i.user_id = ?
                       ORDER BY i.priority ASC, i.created_at DESC"#,
                )
                .bind(uid)
                .fetch_all(&self.pool)
                .await
                .context("querying issues (scoped)")?;

                Ok(rows
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
                            dependency_count: row.try_get::<i64, _>("dependency_count").unwrap_or(0)
                                as u32,
                            dependent_count: row.try_get::<i64, _>("dep_count").unwrap_or(0) as u32,
                            comment_count: row.try_get::<i64, _>("comment_count").unwrap_or(0)
                                as u32,
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
                    .collect())
            }
            None => self.list_beads(repo_name).await,
        }
    }

    /// Get a single bead by ID.
    pub async fn get_bead(&self, id: &str, repo_name: &str) -> Result<Option<Bead>> {
        let full_id = match self.resolve_id(id).await {
            Ok(id) => id,
            Err(_) => return Ok(None),
        };
        let row = query(
            r#"SELECT id, title, description, status, priority, issue_type,
                      assignee, external_ref, notes, created_at, updated_at,
                      created_by, scope,
                      (SELECT COUNT(*) FROM dependencies d WHERE d.depends_on_id = i.id) as dep_count,
                      (SELECT COUNT(*) FROM dependencies d
                              JOIN issues dep_i ON dep_i.id = d.depends_on_id
                              WHERE d.issue_id = i.id
                              AND dep_i.status NOT IN ('closed', 'done')) as dependency_count,
                      (SELECT COUNT(*) FROM comments c WHERE c.issue_id = i.id) as comment_count
               FROM issues i
               WHERE id = ?"#,
        )
        .bind(&full_id)
        .fetch_optional(&self.pool)
        .await
        .with_context(|| format!("querying issue {id}"))?;

        Ok(row.map(|row| {
            let (files, test_files) = Self::parse_files_from_notes(&row);
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
                external_ref: row.try_get("external_ref").ok(),
                branch: None,
                pr_url: None,
                jj_change_id: None,
                files,
                test_files,
                created_by: row.try_get("created_by").ok(),
                scope: row.try_get("scope").unwrap_or_default(),
                derived_from: Self::parse_derived_from_notes(&row),
            }
        }))
    }

    /// Update a bead's status.
    pub async fn update_status(&self, id: &str, status: &str) -> Result<()> {
        let full_id = self.resolve_id(id).await?;
        query("UPDATE issues SET status = ?, updated_at = NOW() WHERE id = ?")
            .bind(status)
            .bind(&full_id)
            .execute(&self.pool)
            .await
            .with_context(|| format!("updating status for {id}"))?;
        self.auto_commit(&format!("{full_id}: status → {status}"))
            .await;
        Ok(())
    }

    /// Set the user_id (owner identity) for multi-tenant scoping.
    pub async fn set_user_id(&self, id: &str, user_id: &str) -> Result<()> {
        let full_id = self.resolve_id(id).await?;
        query("UPDATE issues SET user_id = ? WHERE id = ?")
            .bind(user_id)
            .bind(&full_id)
            .execute(&self.pool)
            .await
            .with_context(|| format!("setting user_id for {id}"))?;
        Ok(())
    }

    /// Update a bead's assignee (owner).
    pub async fn set_assignee(&self, id: &str, assignee: &str) -> Result<()> {
        let full_id = self.resolve_id(id).await?;
        query("UPDATE issues SET assignee = ?, updated_at = NOW() WHERE id = ?")
            .bind(assignee)
            .bind(&full_id)
            .execute(&self.pool)
            .await
            .with_context(|| format!("setting assignee for {id}"))?;
        Ok(())
    }

    /// Create a new bead (issue) in the database.
    pub async fn create_bead(
        &self,
        id: &str,
        title: &str,
        description: &str,
        priority: u8,
        issue_type: &str,
    ) -> Result<()> {
        query(
            r#"INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type, created_at, updated_at)
               VALUES (?, ?, ?, '', '', '', 'open', ?, ?, NOW(), NOW())"#,
        )
        .bind(id)
        .bind(title)
        .bind(description)
        .bind(priority as i32)
        .bind(issue_type)
        .execute(&self.pool)
        .await
        .with_context(|| format!("creating bead {id}"))?;
        self.auto_commit(&format!("create {id}")).await;
        Ok(())
    }

    /// Create a bead with all metadata in a single transaction (one dolt commit).
    ///
    /// Without this, create_bead + set_assignee + set_files + add_dependency
    /// each trigger a separate dolt commit (4+ commits for one bead). This
    /// wraps everything in START TRANSACTION → COMMIT for a single commit.
    #[allow(clippy::too_many_arguments)]
    pub async fn create_bead_full(
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
        // Scrub secrets before writing — Dolt history is append-only and hard to purge.
        let title = crate::secrets::scrub_and_warn(title, &format!("bead {id} title"));
        let description =
            crate::secrets::scrub_and_warn(description, &format!("bead {id} description"));

        let mut tx = self
            .pool
            .begin()
            .await
            .context("starting transaction for create_bead_full")?;

        // 1. Insert the bead
        query(
            r#"INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type, created_by, scope, created_at, updated_at)
               VALUES (?, ?, ?, '', '', '', 'open', ?, ?, ?, ?, NOW(), NOW())"#,
        )
        .bind(id)
        .bind(&title)
        .bind(&description)
        .bind(priority as i32)
        .bind(issue_type)
        .bind(created_by)
        .bind(scope)
        .execute(&mut *tx)
        .await
        .with_context(|| format!("creating bead {id}"))?;

        // 2. Set owner
        query("UPDATE issues SET assignee = ?, updated_at = NOW() WHERE id = ?")
            .bind(owner)
            .bind(id)
            .execute(&mut *tx)
            .await
            .with_context(|| format!("setting assignee for {id}"))?;

        // 3. Set files and provenance if provided
        if !files.is_empty() || !test_files.is_empty() || !derived_from.is_empty() {
            let notes_json = serde_json::json!({
                "files": files,
                "test_files": test_files,
                "derived_from": derived_from,
            });
            query("UPDATE issues SET notes = ?, updated_at = NOW() WHERE id = ?")
                .bind(notes_json.to_string())
                .bind(id)
                .execute(&mut *tx)
                .await
                .with_context(|| format!("setting files for {id}"))?;
        }

        // 4. Add dependencies
        for dep_id in depends_on {
            query("INSERT IGNORE INTO dependencies (issue_id, depends_on_id) VALUES (?, ?)")
                .bind(id)
                .bind(dep_id)
                .execute(&mut *tx)
                .await
                .with_context(|| format!("adding dependency {id} → {dep_id}"))?;
        }

        tx.commit()
            .await
            .with_context(|| format!("committing bead {id}"))?;

        Ok(())
    }

    /// Set files and test_files on a bead. Stored as JSON in the notes column.
    #[allow(dead_code)] // API surface — used by bead_update and future backfill tools
    pub async fn set_files(&self, id: &str, files: &[String], test_files: &[String]) -> Result<()> {
        let full_id = self.resolve_id(id).await?;
        let file_json = serde_json::json!({
            "files": files,
            "test_files": test_files,
        });
        query("UPDATE issues SET notes = ?, updated_at = NOW() WHERE id = ?")
            .bind(file_json.to_string())
            .bind(&full_id)
            .execute(&self.pool)
            .await
            .with_context(|| format!("setting files for {id}"))?;
        Ok(())
    }

    /// PATCH-style update: only writes fields that are `Some` in the update.
    /// Returns the list of field names that were actually updated.
    pub async fn update_bead_fields(
        &self,
        id: &str,
        update: &crate::bead::BeadUpdate,
    ) -> Result<Vec<String>> {
        let full_id = self.resolve_id(id).await?;
        let mut set_clauses = Vec::new();
        let mut bind_values: Vec<String> = Vec::new();
        let mut updated_fields = Vec::new();

        if let Some(ref title) = update.title {
            set_clauses.push("title = ?");
            bind_values.push(title.clone());
            updated_fields.push("title".to_string());
        }
        if let Some(ref description) = update.description {
            set_clauses.push("description = ?");
            bind_values.push(description.clone());
            updated_fields.push("description".to_string());
        }
        if let Some(priority) = update.priority {
            set_clauses.push("priority = ?");
            bind_values.push(priority.to_string());
            updated_fields.push("priority".to_string());
        }
        if let Some(ref issue_type) = update.issue_type {
            set_clauses.push("issue_type = ?");
            bind_values.push(issue_type.clone());
            updated_fields.push("issue_type".to_string());
        }
        if let Some(ref owner) = update.owner {
            set_clauses.push("assignee = ?");
            bind_values.push(owner.clone());
            updated_fields.push("owner".to_string());
        }
        if update.files.is_some() || update.test_files.is_some() {
            // Files/test_files are stored as JSON in the notes column.
            // Read existing notes first to preserve fields not being updated.
            let existing_notes: serde_json::Value = {
                let row = query("SELECT notes FROM issues WHERE id = ?")
                    .bind(&full_id)
                    .fetch_optional(&self.pool)
                    .await?;
                row.and_then(|r| {
                    r.try_get::<String, _>("notes")
                        .ok()
                        .and_then(|s| serde_json::from_str(&s).ok())
                })
                .unwrap_or_else(|| serde_json::json!({}))
            };

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
            let test_files = update
                .test_files
                .as_ref()
                .map(|f| serde_json::json!(f))
                .unwrap_or_else(|| {
                    existing_notes
                        .get("test_files")
                        .cloned()
                        .unwrap_or(serde_json::json!([]))
                });

            let notes_json = serde_json::json!({
                "files": files,
                "test_files": test_files,
            });
            set_clauses.push("notes = ?");
            bind_values.push(notes_json.to_string());
            if update.files.is_some() {
                updated_fields.push("files".to_string());
            }
            if update.test_files.is_some() {
                updated_fields.push("test_files".to_string());
            }
        }

        if set_clauses.is_empty() {
            return Ok(updated_fields);
        }

        set_clauses.push("updated_at = NOW()");
        let sql = format!("UPDATE issues SET {} WHERE id = ?", set_clauses.join(", "));

        let mut q = query(&sql);
        for val in &bind_values {
            q = q.bind(val);
        }
        q = q.bind(&full_id);
        q.execute(&self.pool)
            .await
            .with_context(|| format!("updating fields for {id}"))?;

        self.auto_commit(&format!("update {full_id}: {}", updated_fields.join(", ")))
            .await;
        Ok(updated_fields)
    }

    /// Close a bead by setting its status to 'closed'.
    pub async fn close_bead(&self, id: &str) -> Result<()> {
        let full_id = self.resolve_id(id).await?;
        query("UPDATE issues SET status = 'closed', updated_at = NOW() WHERE id = ?")
            .bind(&full_id)
            .execute(&self.pool)
            .await
            .with_context(|| format!("closing bead {id}"))?;
        self.auto_commit(&format!("close {full_id}")).await;
        Ok(())
    }
}
