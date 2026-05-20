//! Backend-agnostic issue tracker sync engine.
//!
//! The `IssueTracker` trait abstracts over external systems (Linear, GitHub
//! Issues, Jira). The `SyncEngine` runs bidirectional reconciliation:
//!   Pull: external → bead (create beads for new external issues)
//!   Push: bead → external (create external issues for unlinked beads)
//!   Reconcile: status sync via external_ref linkage

use anyhow::Result;

use crate::bead::{Bead, BeadUpdate};
use crate::store::BeadStore;

/// A normalized issue from an external tracker.
#[derive(Debug, Clone)]
pub struct ExternalIssue {
    /// External system's ID (e.g., "AGE-5", "agentic-research/mache#42")
    pub external_id: String,
    /// Title
    pub title: String,
    /// Description
    pub description: String,
    /// Status mapped to rosary states: "open", "in_progress", "closed"
    pub status: String,
    /// Priority 0-4
    pub priority: u8,
    /// Labels/tags
    pub labels: Vec<String>,
}

/// Trait for external issue tracker backends.
#[async_trait::async_trait]
pub trait IssueTracker: Send + Sync {
    /// List open issues from the external system.
    async fn list_open(&self) -> Result<Vec<ExternalIssue>>;

    /// Create a new issue in the external system. Returns the external ID.
    async fn create(&self, issue: &ExternalIssue) -> Result<String>;

    /// Update an issue's status in the external system.
    async fn update_status(&self, external_id: &str, status: &str) -> Result<()>;

    /// Update an issue's fields in the external system (PATCH semantics).
    /// Default implementation is a no-op — backends opt in to field sync.
    async fn update_fields(&self, _external_id: &str, _update: &BeadUpdate) -> Result<()> {
        Ok(())
    }

    /// Human-readable backend name (e.g., "linear", "github").
    fn name(&self) -> &str;
}

/// Result of a sync operation.
#[derive(Debug, Default)]
pub struct SyncResult {
    /// Beads created from external issues (pull)
    pub pulled: usize,
    /// External issues created from beads (push)
    pub pushed: usize,
    /// Status updates applied (reconcile)
    pub reconciled: usize,
    /// Errors encountered (non-fatal)
    pub errors: usize,
}

impl std::fmt::Display for SyncResult {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "pulled={} pushed={} reconciled={} errors={}",
            self.pulled, self.pushed, self.reconciled, self.errors
        )
    }
}

/// Run bidirectional sync between beads and an external tracker.
pub async fn bidi_sync(
    tracker: &dyn IssueTracker,
    client: &dyn BeadStore,
    beads: &[Bead],
    _repo_name: &str,
) -> Result<SyncResult> {
    let mut result = SyncResult::default();

    let external = tracker.list_open().await?;

    // Build lookup maps
    let bead_by_ext_ref: std::collections::HashMap<&str, &Bead> = beads
        .iter()
        .filter_map(|b| b.external_ref.as_deref().map(|r| (r, b)))
        .collect();

    let ext_by_id: std::collections::HashMap<&str, &ExternalIssue> = external
        .iter()
        .map(|e| (e.external_id.as_str(), e))
        .collect();

    // --- PULL: external → bead ---
    // Create beads for external issues that have no linked bead.
    // If a bead already exists (title match), link it via external_ref.
    for ext in &external {
        if bead_by_ext_ref.contains_key(ext.external_id.as_str()) {
            continue; // already linked
        }
        // Check title match as fallback dedup — and link if found
        let title_match = beads
            .iter()
            .find(|b| b.title == ext.title || ext.title == format!("[{}] {}", b.repo, b.title));
        if let Some(matched) = title_match {
            if matched.external_ref.is_none() {
                if let Err(e) = client.set_external_ref(&matched.id, &ext.external_id).await {
                    eprintln!(
                        "[sync] link error {} → {}: {e}",
                        matched.id, ext.external_id
                    );
                    result.errors += 1;
                } else {
                    eprintln!("[sync] linked {} → {}", matched.id, ext.external_id);
                    result.reconciled += 1;
                }
            }
            continue;
        }

        let millis = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap_or_default()
            .as_millis();
        let id = format!("rsry-{:06x}", millis & 0xffffff);
        match client
            .create_bead(&id, &ext.title, &ext.description, ext.priority, "task")
            .await
        {
            Ok(()) => {
                eprintln!("[sync] pulled {} → {id}", ext.external_id);
                result.pulled += 1;
            }
            Err(e) => {
                eprintln!("[sync] pull error for {}: {e}", ext.external_id);
                result.errors += 1;
            }
        }
    }

    // --- PUSH: bead → external ---
    // Create external issues for beads with no external_ref
    for bead in beads {
        if bead.external_ref.is_some() {
            continue; // already linked
        }
        if bead.status == "closed" {
            continue;
        }
        if bead.priority > 2 {
            continue; // skip low-priority
        }
        // Check if external issue with same title exists
        if external.iter().any(|e| e.title.contains(&bead.title)) {
            continue;
        }

        let label = format!("[{}] ", bead.repo);
        let mut issue_labels = vec![bead.repo.clone()];
        // Derive perspective label from bead owner (e.g., "dev-agent" → "perspective:dev")
        if let Some(ref owner) = bead.owner
            && let Some(perspective) = owner.strip_suffix("-agent")
        {
            issue_labels.push(format!("perspective:{perspective}"));
        }
        let ext_issue = ExternalIssue {
            external_id: String::new(),
            title: format!("{label}{}", bead.title),
            description: bead.description.clone(),
            status: bead.status.clone(),
            priority: bead.priority,
            labels: issue_labels,
        };

        match tracker.create(&ext_issue).await {
            Ok(ext_id) => {
                eprintln!("[sync] pushed {} → {ext_id}", bead.id);
                // Store external_ref back on the bead so future syncs can reconcile
                if let Err(e) = client.set_external_ref(&bead.id, &ext_id).await {
                    eprintln!("[sync] failed to store external_ref for {}: {e}", bead.id);
                }
                result.pushed += 1;
            }
            Err(e) => {
                eprintln!("[sync] push error for {}: {e}", bead.id);
                result.errors += 1;
            }
        }
    }

    // --- RECONCILE: bidirectional status sync for linked beads ---
    for bead in beads {
        let Some(ext_ref) = bead.external_ref.as_deref() else {
            continue;
        };
        let Some(ext) = ext_by_id.get(ext_ref) else {
            // External issue not in open list — may already be closed externally.
            // If bead is still open, mark it closed to match.
            if bead.status != "closed" {
                if let Err(e) = client.update_status(&bead.id, "closed").await {
                    eprintln!("[sync] reconcile error for {}: {e}", bead.id);
                    result.errors += 1;
                } else {
                    eprintln!(
                        "[sync] reconciled {} ({} → closed, external gone)",
                        bead.id, bead.status
                    );
                    result.reconciled += 1;
                }
            }
            continue;
        };

        let ext_status = &ext.status;
        if *ext_status == bead.status {
            continue;
        }

        if bead.status == "closed" {
            // Bead closed locally — push closure to external tracker
            if let Err(e) = tracker.update_status(ext_ref, &bead.status).await {
                eprintln!("[sync] reconcile error pushing close for {}: {e}", bead.id);
                result.errors += 1;
            } else {
                eprintln!("[sync] reconciled {} (closed → external)", bead.id);
                result.reconciled += 1;
            }
        } else {
            // External changed — update bead
            if let Err(e) = client.update_status(&bead.id, ext_status).await {
                eprintln!("[sync] reconcile error for {}: {e}", bead.id);
                result.errors += 1;
            } else {
                eprintln!(
                    "[sync] reconciled {} ({} → {ext_status})",
                    bead.id, bead.status
                );
                result.reconciled += 1;
            }
        }
    }

    Ok(result)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Mutex;

    /// Mock tracker for testing
    struct MockTracker {
        issues: Mutex<Vec<ExternalIssue>>,
    }

    impl MockTracker {
        fn new(issues: Vec<ExternalIssue>) -> Self {
            Self {
                issues: Mutex::new(issues),
            }
        }
    }

    #[async_trait::async_trait]
    impl IssueTracker for MockTracker {
        async fn list_open(&self) -> Result<Vec<ExternalIssue>> {
            Ok(self.issues.lock().unwrap().clone())
        }

        async fn create(&self, issue: &ExternalIssue) -> Result<String> {
            let id = format!("MOCK-{}", self.issues.lock().unwrap().len() + 1);
            let mut issues = self.issues.lock().unwrap();
            let mut new_issue = issue.clone();
            new_issue.external_id = id.clone();
            issues.push(new_issue);
            Ok(id)
        }

        async fn update_status(&self, external_id: &str, status: &str) -> Result<()> {
            let mut issues = self.issues.lock().unwrap();
            if let Some(issue) = issues.iter_mut().find(|i| i.external_id == external_id) {
                issue.status = status.to_string();
            }
            Ok(())
        }

        fn name(&self) -> &str {
            "mock"
        }
    }

    #[test]
    fn sync_result_display() {
        let r = SyncResult {
            pulled: 3,
            pushed: 5,
            reconciled: 1,
            errors: 0,
        };
        assert!(format!("{r}").contains("pulled=3"));
        assert!(format!("{r}").contains("pushed=5"));
    }

    #[test]
    fn external_issue_fields() {
        let issue = ExternalIssue {
            external_id: "AGE-5".into(),
            title: "Test".into(),
            description: "Desc".into(),
            status: "open".into(),
            priority: 1,
            labels: vec!["rosary".into()],
        };
        assert_eq!(issue.external_id, "AGE-5");
        assert_eq!(issue.priority, 1);
    }

    #[tokio::test]
    async fn mock_tracker_create() {
        let tracker = MockTracker::new(vec![]);
        let issue = ExternalIssue {
            external_id: String::new(),
            title: "New issue".into(),
            description: "Desc".into(),
            status: "open".into(),
            priority: 1,
            labels: vec![],
        };
        let id = tracker.create(&issue).await.unwrap();
        assert_eq!(id, "MOCK-1");

        let open = tracker.list_open().await.unwrap();
        assert_eq!(open.len(), 1);
        assert_eq!(open[0].title, "New issue");
    }

    #[tokio::test]
    async fn mock_tracker_update_status() {
        let tracker = MockTracker::new(vec![ExternalIssue {
            external_id: "MOCK-1".into(),
            title: "Test".into(),
            description: String::new(),
            status: "open".into(),
            priority: 1,
            labels: vec![],
        }]);

        tracker.update_status("MOCK-1", "closed").await.unwrap();
        let issues = tracker.list_open().await.unwrap();
        assert_eq!(issues[0].status, "closed");
    }
}
