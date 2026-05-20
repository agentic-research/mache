//! Dolt-backed per-repo bead store.
//!
//! Thin wrapper around [`DoltClient`] implementing [`BeadStore`].
//! Preserves backward compatibility for repos still using Dolt servers.

use anyhow::Result;
use async_trait::async_trait;

use crate::bead::{Bead, BeadUpdate};
use crate::dolt::DoltClient;
use crate::store::BeadStore;

pub struct DoltBeadStore {
    pub(crate) client: DoltClient,
}

impl DoltBeadStore {
    pub fn new(client: DoltClient) -> Self {
        Self { client }
    }
}

#[async_trait]
impl BeadStore for DoltBeadStore {
    async fn list_beads(&self, repo_name: &str) -> Result<Vec<Bead>> {
        self.client.list_beads(repo_name).await
    }

    async fn list_beads_scoped(&self, repo_name: &str, user_id: Option<&str>) -> Result<Vec<Bead>> {
        self.client.list_beads_scoped(repo_name, user_id).await
    }

    async fn get_bead(&self, id: &str, repo_name: &str) -> Result<Option<Bead>> {
        self.client.get_bead(id, repo_name).await
    }

    async fn create_bead(
        &self,
        id: &str,
        title: &str,
        description: &str,
        priority: u8,
        issue_type: &str,
    ) -> Result<()> {
        self.client
            .create_bead(id, title, description, priority, issue_type)
            .await
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
        self.client
            .create_bead_full(
                id,
                title,
                description,
                priority,
                issue_type,
                owner,
                files,
                test_files,
                depends_on,
                created_by,
                scope,
                derived_from,
            )
            .await
    }

    async fn update_bead_fields(&self, id: &str, update: &BeadUpdate) -> Result<Vec<String>> {
        self.client.update_bead_fields(id, update).await
    }

    async fn update_status(&self, id: &str, status: &str) -> Result<()> {
        self.client.update_status(id, status).await
    }

    async fn get_status(&self, id: &str) -> Result<Option<String>> {
        self.client.get_status(id).await
    }

    async fn close_bead(&self, id: &str) -> Result<()> {
        self.client.close_bead(id).await
    }

    async fn set_assignee(&self, id: &str, assignee: &str) -> Result<()> {
        self.client.set_assignee(id, assignee).await
    }

    async fn set_user_id(&self, id: &str, user_id: &str) -> Result<()> {
        self.client.set_user_id(id, user_id).await
    }

    async fn set_files(&self, id: &str, files: &[String], test_files: &[String]) -> Result<()> {
        self.client.set_files(id, files, test_files).await
    }

    async fn search_beads(&self, query: &str, repo_name: &str, limit: u32) -> Result<Vec<Bead>> {
        self.client.search_beads(query, repo_name, limit).await
    }

    async fn get_external_ref(&self, id: &str) -> Result<Option<String>> {
        self.client.get_external_ref(id).await
    }

    async fn set_external_ref(&self, id: &str, external_ref: &str) -> Result<()> {
        self.client.set_external_ref(id, external_ref).await
    }

    async fn find_by_external_ref(&self, external_ref: &str) -> Result<Option<String>> {
        self.client.find_by_external_ref(external_ref).await
    }

    async fn list_closed_linked_beads(&self, repo_name: &str) -> Result<Vec<Bead>> {
        self.client.list_closed_linked_beads(repo_name).await
    }

    async fn add_dependency(&self, issue_id: &str, depends_on_id: &str) -> Result<()> {
        self.client.add_dependency(issue_id, depends_on_id).await
    }

    async fn remove_dependency(&self, issue_id: &str, depends_on_id: &str) -> Result<()> {
        self.client.remove_dependency(issue_id, depends_on_id).await
    }

    async fn get_dependencies(&self, issue_id: &str) -> Result<Vec<String>> {
        self.client.get_dependencies(issue_id).await
    }

    async fn get_dependents(&self, issue_id: &str) -> Result<Vec<String>> {
        self.client.get_dependents(issue_id).await
    }

    async fn add_comment(&self, issue_id: &str, body: &str, author: &str) -> Result<()> {
        self.client.add_comment(issue_id, body, author).await
    }

    async fn list_comments(
        &self,
        issue_id: &str,
        include_deleted: bool,
    ) -> Result<Vec<crate::bead::Comment>> {
        self.client.list_comments(issue_id, include_deleted).await
    }

    async fn update_comment(
        &self,
        comment_id: &str,
        body: &str,
        reason: Option<&str>,
    ) -> Result<crate::bead::Comment> {
        self.client.update_comment(comment_id, body, reason).await
    }

    async fn delete_comment(&self, comment_id: &str, reason: Option<&str>) -> Result<()> {
        self.client.delete_comment(comment_id, reason).await
    }

    async fn hard_delete_comment(&self, comment_id: &str) -> Result<()> {
        self.client.hard_delete_comment(comment_id).await
    }

    async fn log_event(&self, issue_id: &str, event_type: &str, detail: &str) {
        self.client.log_event(issue_id, event_type, detail).await
    }

    async fn get_latest_event(&self, issue_id: &str, event_type: &str) -> Result<Option<String>> {
        self.client.get_latest_event(issue_id, event_type).await
    }
}
