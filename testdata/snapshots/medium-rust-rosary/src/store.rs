//! Backend store traits for rosary.
//!
//! Two trait families:
//!
//! **Orchestrator state** (cross-repo, single global DB):
//! - [`HierarchyStore`]: decades, threads, bead-to-thread membership
//! - [`DispatchStore`]: pipeline state, dispatch history, backoff
//! - [`LinkageStore`]: cross-repo dependencies, Linear linkage
//! - [`BackendStore`]: unified supertrait
//!
//! **Bead CRUD** (per-repo `.beads/` directory):
//! - [`BeadStore`]: create, read, update, search, close, comment, dependencies

use anyhow::Result;
use async_trait::async_trait;
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};

// ── Data types ──────────────────────────────────────────

/// A reference to a bead across repos.
#[derive(Debug, Clone, PartialEq, Eq, Hash, PartialOrd, Ord, Serialize, Deserialize)]
pub struct WorkRef {
    pub repo: String,
    /// Team/folder scope within a monorepo (e.g. "auth", "payments/core").
    /// Empty string for cross-repo and single-team repos — backward compatible.
    #[serde(default)]
    pub scope: String,
    pub bead_id: String,
}

/// Persistent record of a decade (ADR-level organizing primitive).
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct DecadeRecord {
    /// e.g. "ADR-003"
    pub id: String,
    pub title: String,
    /// Path to the source ADR markdown file.
    pub source_path: String,
    /// proposed, active, completed, superseded
    pub status: String,
}

/// Persistent record of a thread within a decade.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ThreadRecord {
    /// e.g. "ADR-003/implementation"
    pub id: String,
    pub name: String,
    pub decade_id: String,
    pub feature_branch: Option<String>,
}

/// Pipeline state for a single bead — replaces in-memory BeadTracker.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct PipelineState {
    pub bead_ref: WorkRef,
    /// Index into the agent sequence (dev=0, staging=1, prod=2, feature=3).
    pub pipeline_phase: u8,
    /// Current agent name (e.g. "dev-agent").
    pub pipeline_agent: String,
    /// Sub-state within the current phase. Eliminates ambiguity during recovery:
    /// - pending: phase selected, not yet dispatched
    /// - executing: agent spawned and running
    /// - completed: agent exited, verification passed
    /// - failed: agent exited, verification failed or timeout
    pub phase_status: String,
    pub retries: u32,
    pub consecutive_reverts: u32,
    pub highest_verify_tier: Option<u8>,
    /// Content hash — changes signal rescan needed.
    pub last_generation: u64,
    /// When this bead becomes eligible for retry after backoff.
    pub backoff_until: Option<DateTime<Utc>>,
}

/// Record of a single dispatch (agent execution).
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct DispatchRecord {
    /// UUID v4
    pub id: String,
    pub bead_ref: WorkRef,
    pub agent: String,
    /// claude, gemini, acp
    pub provider: String,
    pub started_at: DateTime<Utc>,
    pub completed_at: Option<DateTime<Utc>>,
    /// success, failure, timeout
    pub outcome: Option<String>,
    pub work_dir: String,
    /// Claude Code session ID (from --output-format json). Enables --resume.
    pub session_id: Option<String>,
    /// jj workspace path (distinct from work_dir repo root).
    pub workspace_path: Option<String>,
    /// HEAD commit SHA of the target repo at dispatch time (APAS chain integrity).
    /// Ties agent output to a specific repo snapshot for auditability.
    pub chain_hash: Option<String>,
}

/// Modal evidence tier for cross-repo dependency edges (ADR-0009).
///
/// `Asserted` and `Derived` block dispatch. `Conjectured` annotates only.
/// A janitor pass demotes `Derived` → `Conjectured` after TTL if mache
/// doesn't re-confirm the edge, preventing phantom blockers.
#[derive(Debug, Clone, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize, Default)]
#[serde(rename_all = "lowercase")]
pub enum EvidenceTier {
    /// Conjectured: mache heuristic (markdown mention, symbol use). Annotates only.
    Conjectured,
    /// Derived: BDR `derived_from`, `depends_on:` frontmatter, ProvenanceRef. Blocks dispatch.
    Derived,
    /// Asserted: human via `rsry_bead_link --cross-repo`. Blocks dispatch.
    #[default]
    Asserted,
}

impl EvidenceTier {
    /// Whether this tier should block dispatch (Asserted or Derived).
    pub fn blocks_dispatch(&self) -> bool {
        matches!(self, EvidenceTier::Asserted | EvidenceTier::Derived)
    }

    pub fn as_str(&self) -> &'static str {
        match self {
            EvidenceTier::Asserted => "asserted",
            EvidenceTier::Derived => "derived",
            EvidenceTier::Conjectured => "conjectured",
        }
    }
}

impl std::fmt::Display for EvidenceTier {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(self.as_str())
    }
}

impl std::str::FromStr for EvidenceTier {
    type Err = anyhow::Error;
    fn from_str(s: &str) -> Result<Self> {
        match s {
            "asserted" => Ok(EvidenceTier::Asserted),
            "derived" => Ok(EvidenceTier::Derived),
            "conjectured" => Ok(EvidenceTier::Conjectured),
            other => anyhow::bail!("unknown evidence tier: {other}"),
        }
    }
}

/// Cross-repo dependency between beads.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct CrossRepoDep {
    pub from: WorkRef,
    pub to: WorkRef,
    /// blocks, relates_to
    pub dep_type: String,
    /// Modal evidence tier (ADR-0009). Defaults to Asserted for human-written edges.
    #[serde(default)]
    pub evidence_tier: EvidenceTier,
    /// Source of the edge: mache scan id, "human", or agent name.
    #[serde(default = "default_source")]
    pub source: String,
    /// When this edge was last observed/written.
    #[serde(default = "Utc::now")]
    pub observed_at: DateTime<Utc>,
}

fn default_source() -> String {
    "human".to_string()
}

/// Mapping between a bead and its Linear representation.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct LinearLink {
    pub bead_ref: WorkRef,
    /// e.g. "AGE-330"
    pub linear_id: String,
    /// issue, sub_issue, milestone
    pub linear_type: String,
}

// ── Traits ──────────────────────────────────────────────

/// Decades, threads, bead-to-thread membership.
/// Drives BDR accretion and Linear milestone/issue projection.
#[async_trait]
pub trait HierarchyStore: Send + Sync {
    async fn upsert_decade(&self, decade: &DecadeRecord) -> Result<()>;
    async fn get_decade(&self, id: &str) -> Result<Option<DecadeRecord>>;
    async fn list_decades(&self, status: Option<&str>) -> Result<Vec<DecadeRecord>>;

    async fn upsert_thread(&self, thread: &ThreadRecord) -> Result<()>;
    async fn list_threads(&self, decade_id: &str) -> Result<Vec<ThreadRecord>>;

    async fn add_bead_to_thread(&self, thread_id: &str, bead: &WorkRef) -> Result<()>;
    async fn list_beads_in_thread(&self, thread_id: &str) -> Result<Vec<WorkRef>>;
    async fn find_thread_for_bead(&self, bead: &WorkRef) -> Result<Option<String>>;
}

/// Pipeline state, dispatch history, backoff.
/// Replaces in-memory BeadTracker + SessionRegistry.
#[async_trait]
pub trait DispatchStore: Send + Sync {
    async fn upsert_pipeline(&self, state: &PipelineState) -> Result<()>;
    async fn get_pipeline(&self, bead: &WorkRef) -> Result<Option<PipelineState>>;
    async fn list_active_pipelines(&self) -> Result<Vec<PipelineState>>;
    async fn clear_pipeline(&self, bead: &WorkRef) -> Result<()>;

    async fn record_dispatch(&self, record: &DispatchRecord) -> Result<()>;
    /// Upsert a dispatch record (insert or update). Used by migration to handle
    /// both active and completed dispatches idempotently.
    async fn upsert_dispatch(&self, record: &DispatchRecord) -> Result<()>;
    async fn complete_dispatch(&self, id: &str, outcome: &str) -> Result<()>;
    /// Update the session_id on a dispatch record (captured after agent starts).
    async fn update_dispatch_session(&self, id: &str, session_id: &str) -> Result<()>;
    async fn active_dispatches(&self) -> Result<Vec<DispatchRecord>>;
}

/// Cross-repo dependencies and Linear linkage.
/// Replaces overloaded `external_ref` field and mirror-bead pattern.
#[async_trait]
pub trait LinkageStore: Send + Sync {
    /// Write a cross-repo dependency edge.
    /// For same-repo edges, rejects if the edge would create a per-repo cycle (ADR-0009).
    async fn add_dependency(&self, dep: &CrossRepoDep) -> Result<()>;
    async fn dependencies_of(&self, bead: &WorkRef) -> Result<Vec<CrossRepoDep>>;
    async fn dependents_of(&self, bead: &WorkRef) -> Result<Vec<CrossRepoDep>>;
    /// All dep_type='blocks' edges with evidence tier >= Derived. Used by triage.
    async fn all_blocking_deps(&self) -> Result<Vec<CrossRepoDep>>;
    /// Remove a specific cross-repo dep edge.
    async fn remove_cross_repo_dep(&self, from: &WorkRef, to: &WorkRef) -> Result<()>;
    /// Demote Derived → Conjectured for edges not re-confirmed within `ttl_days`.
    /// Returns the number of edges demoted.
    async fn demote_stale_derived(&self, ttl_days: u32) -> Result<u64>;

    async fn upsert_linear_link(&self, link: &LinearLink) -> Result<()>;
    async fn find_by_linear_id(&self, linear_id: &str) -> Result<Option<LinearLink>>;
}

/// Per-user repo registration for multi-tenant.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct UserRepo {
    pub user_id: String,
    pub repo_url: String,
    pub repo_name: String,
    /// Reference to encrypted GitHub token in KV (not the token itself).
    pub github_token_ref: Option<String>,
}

/// Per-user repo registration store.
#[async_trait]
pub trait UserRepoStore: Send + Sync {
    async fn register_repo(&self, repo: &UserRepo) -> Result<()>;
    async fn list_user_repos(&self, user_id: &str) -> Result<Vec<UserRepo>>;
    async fn unregister_repo(&self, user_id: &str, repo_name: &str) -> Result<()>;
}

// ── Bead CRUD trait ──────────────────────────────────────

/// Per-repo bead storage — CRUD, search, dependencies, comments, events.
///
/// Each repo has its own BeadStore (SQLite file or Dolt server).
/// Implementations: [`crate::bead_sqlite::SqliteBeadStore`],
/// [`crate::bead_dolt::DoltBeadStore`].
#[async_trait]
pub trait BeadStore: Send + Sync {
    // ── CRUD ──
    async fn list_beads(&self, repo_name: &str) -> Result<Vec<crate::bead::Bead>>;
    async fn list_beads_scoped(
        &self,
        repo_name: &str,
        user_id: Option<&str>,
    ) -> Result<Vec<crate::bead::Bead>>;
    async fn get_bead(&self, id: &str, repo_name: &str) -> Result<Option<crate::bead::Bead>>;
    async fn create_bead(
        &self,
        id: &str,
        title: &str,
        description: &str,
        priority: u8,
        issue_type: &str,
    ) -> Result<()>;
    #[allow(clippy::too_many_arguments)]
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
    ) -> Result<()>;

    // ── Field updates ──
    async fn update_bead_fields(
        &self,
        id: &str,
        update: &crate::bead::BeadUpdate,
    ) -> Result<Vec<String>>;
    async fn update_status(&self, id: &str, status: &str) -> Result<()>;
    async fn get_status(&self, id: &str) -> Result<Option<String>>;
    async fn close_bead(&self, id: &str) -> Result<()>;
    async fn set_assignee(&self, id: &str, assignee: &str) -> Result<()>;
    async fn set_user_id(&self, id: &str, user_id: &str) -> Result<()>;
    async fn set_files(&self, id: &str, files: &[String], test_files: &[String]) -> Result<()>;

    // ── Search ──
    async fn search_beads(
        &self,
        query: &str,
        repo_name: &str,
        limit: u32,
    ) -> Result<Vec<crate::bead::Bead>>;

    /// Full-text search using FTS5 (SQLite-only, porter stemmer).
    ///
    /// Falls back to LIKE-based `search_beads` on backends that don't support
    /// FTS5 (Dolt). SQLiteBeadStore overrides this with the real FTS5 path.
    async fn search_beads_fts(
        &self,
        query: &str,
        repo_name: &str,
        limit: u32,
    ) -> Result<Vec<crate::bead::Bead>> {
        self.search_beads(query, repo_name, limit).await
    }

    // ── External references (Linear linkage) ──
    async fn get_external_ref(&self, id: &str) -> Result<Option<String>>;
    async fn set_external_ref(&self, id: &str, external_ref: &str) -> Result<()>;
    async fn find_by_external_ref(&self, external_ref: &str) -> Result<Option<String>>;
    async fn list_closed_linked_beads(&self, repo_name: &str) -> Result<Vec<crate::bead::Bead>>;

    // ── Dependencies ──
    async fn add_dependency(&self, issue_id: &str, depends_on_id: &str) -> Result<()>;
    async fn remove_dependency(&self, issue_id: &str, depends_on_id: &str) -> Result<()>;
    async fn get_dependencies(&self, issue_id: &str) -> Result<Vec<String>>;
    async fn get_dependents(&self, issue_id: &str) -> Result<Vec<String>>;

    // ── Comments & events ──
    async fn add_comment(&self, issue_id: &str, body: &str, author: &str) -> Result<()>;
    /// List comments for a bead. `include_deleted = false` filters out
    /// soft-deleted entries (the default for end-user surfaces).
    /// Returns oldest-first.
    async fn list_comments(
        &self,
        issue_id: &str,
        include_deleted: bool,
    ) -> Result<Vec<crate::bead::Comment>>;
    /// Update an existing comment's body. Sets `edited_at` to now,
    /// `edit_reason` to the supplied reason (if any), and on the **first**
    /// edit captures the prior body in `original_text` (immutable
    /// thereafter — subsequent edits do not rewrite it). Returns the
    /// updated comment.
    ///
    /// Returns `Err` if the comment does not exist or has been hard-deleted.
    /// Soft-deleted comments are still editable (un-deleting is out of scope
    /// for v1; if you need it, file a separate bead).
    async fn update_comment(
        &self,
        comment_id: &str,
        body: &str,
        reason: Option<&str>,
    ) -> Result<crate::bead::Comment>;
    /// Soft-delete a comment by setting `deleted_at = NOW()`. Audit trail
    /// (`original_text`, `edit_reason`) is preserved. Returns `Err` if the
    /// comment does not exist. Idempotent: deleting an already-deleted
    /// comment refreshes `deleted_at` but does not error.
    async fn delete_comment(&self, comment_id: &str, reason: Option<&str>) -> Result<()>;
    /// Hard-delete a comment — physically removes the row. Audit trail is
    /// destroyed. CLI-only with explicit confirmation; **never** exposed
    /// through MCP. Returns `Err` if the comment does not exist.
    async fn hard_delete_comment(&self, comment_id: &str) -> Result<()>;
    /// Best-effort audit log. Implementations should warn on failure, not error.
    async fn log_event(&self, issue_id: &str, event_type: &str, detail: &str);
    /// Most recent event detail for a bead + event type.
    async fn get_latest_event(&self, issue_id: &str, event_type: &str) -> Result<Option<String>>;
}

// ── Composite traits ───────────────────────────────────

/// Unified supertrait — single trait object for all orchestrator state.
pub trait BackendStore: HierarchyStore + DispatchStore + LinkageStore + UserRepoStore {}

/// Blanket impl: anything implementing all four traits is a BackendStore.
impl<T: HierarchyStore + DispatchStore + LinkageStore + UserRepoStore> BackendStore for T {}

/// Re-parent an existing thread under a different decade.
///
/// Walks all decades to find the thread by id (caller doesn't need to know
/// the current parent). Auto-creates the target decade if missing. Preserves
/// the existing `feature_branch`. Optional `new_name` renames in the same call.
///
/// Shared by `rsry thread-reparent` (CLI) and `rsry_thread_reparent` (MCP).
pub async fn reparent_thread(
    backend: &dyn BackendStore,
    thread_id: &str,
    decade_id: &str,
    new_name: Option<&str>,
) -> anyhow::Result<()> {
    let mut found: Option<ThreadRecord> = None;
    for d in backend.list_decades(None).await? {
        for t in backend.list_threads(&d.id).await? {
            if t.id == thread_id {
                found = Some(t);
                break;
            }
        }
        if found.is_some() {
            break;
        }
    }
    let Some(existing) = found else {
        anyhow::bail!("thread not found: {thread_id}");
    };
    if backend.get_decade(decade_id).await?.is_none() {
        backend
            .upsert_decade(&DecadeRecord {
                id: decade_id.to_string(),
                title: decade_id.to_string(),
                source_path: String::new(),
                status: "active".to_string(),
            })
            .await?;
    }
    backend
        .upsert_thread(&ThreadRecord {
            id: existing.id.clone(),
            name: new_name.map(String::from).unwrap_or(existing.name),
            decade_id: decade_id.to_string(),
            feature_branch: existing.feature_branch,
        })
        .await?;
    Ok(())
}

/// Bulk export — used by migration and backup.
/// Separate from the main store traits to keep them focused.
#[async_trait]
pub trait BackendExport: BackendStore {
    async fn all_threads(&self) -> Result<Vec<ThreadRecord>>;
    async fn all_thread_members(&self) -> Result<Vec<(String, WorkRef)>>;
    async fn all_dispatches(&self) -> Result<Vec<DispatchRecord>>;
    async fn all_dependencies(&self) -> Result<Vec<CrossRepoDep>>;
    async fn all_linear_links(&self) -> Result<Vec<LinearLink>>;
    async fn all_user_repos(&self) -> Result<Vec<UserRepo>>;
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use std::sync::Mutex;

    /// DFS reachability: can we reach `target` from `start` following dep edges?
    fn can_reach(deps: &[CrossRepoDep], start: &WorkRef, target: &WorkRef) -> bool {
        let mut visited = std::collections::HashSet::new();
        let mut stack = vec![start.clone()];
        while let Some(node) = stack.pop() {
            if &node == target {
                return true;
            }
            if visited.contains(&node.bead_id) {
                continue;
            }
            visited.insert(node.bead_id.clone());
            for dep in deps.iter().filter(|d| &d.from == &node) {
                stack.push(dep.to.clone());
            }
        }
        false
    }

    /// In-memory implementation of all three store traits for testing.
    /// Exported `pub(crate)` so sibling test modules (handlers tests,
    /// reconcile tests) can use it without duplicating the trait impls.
    pub(crate) struct InMemoryStore {
        decades: Mutex<Vec<DecadeRecord>>,
        threads: Mutex<Vec<ThreadRecord>>,
        /// (thread_id, beads)
        thread_members: Mutex<Vec<(String, WorkRef)>>,
        pipelines: Mutex<Vec<PipelineState>>,
        dispatches: Mutex<Vec<DispatchRecord>>,
        deps: Mutex<Vec<CrossRepoDep>>,
        linear_links: Mutex<Vec<LinearLink>>,
    }

    impl InMemoryStore {
        pub(crate) fn new() -> Self {
            Self {
                decades: Mutex::new(Vec::new()),
                threads: Mutex::new(Vec::new()),
                thread_members: Mutex::new(Vec::new()),
                pipelines: Mutex::new(Vec::new()),
                dispatches: Mutex::new(Vec::new()),
                deps: Mutex::new(Vec::new()),
                linear_links: Mutex::new(Vec::new()),
            }
        }
    }

    #[async_trait]
    impl HierarchyStore for InMemoryStore {
        async fn upsert_decade(&self, decade: &DecadeRecord) -> Result<()> {
            let mut decades = self.decades.lock().unwrap();
            if let Some(existing) = decades.iter_mut().find(|d| d.id == decade.id) {
                *existing = decade.clone();
            } else {
                decades.push(decade.clone());
            }
            Ok(())
        }

        async fn get_decade(&self, id: &str) -> Result<Option<DecadeRecord>> {
            let decades = self.decades.lock().unwrap();
            Ok(decades.iter().find(|d| d.id == id).cloned())
        }

        async fn list_decades(&self, status: Option<&str>) -> Result<Vec<DecadeRecord>> {
            let decades = self.decades.lock().unwrap();
            Ok(match status {
                Some(s) => decades.iter().filter(|d| d.status == s).cloned().collect(),
                None => decades.clone(),
            })
        }

        async fn upsert_thread(&self, thread: &ThreadRecord) -> Result<()> {
            let mut threads = self.threads.lock().unwrap();
            if let Some(existing) = threads.iter_mut().find(|t| t.id == thread.id) {
                *existing = thread.clone();
            } else {
                threads.push(thread.clone());
            }
            Ok(())
        }

        async fn list_threads(&self, decade_id: &str) -> Result<Vec<ThreadRecord>> {
            let threads = self.threads.lock().unwrap();
            Ok(threads
                .iter()
                .filter(|t| t.decade_id == decade_id)
                .cloned()
                .collect())
        }

        async fn add_bead_to_thread(&self, thread_id: &str, bead: &WorkRef) -> Result<()> {
            let mut members = self.thread_members.lock().unwrap();
            if !members.iter().any(|(tid, b)| tid == thread_id && b == bead) {
                members.push((thread_id.to_string(), bead.clone()));
            }
            Ok(())
        }

        async fn list_beads_in_thread(&self, thread_id: &str) -> Result<Vec<WorkRef>> {
            let members = self.thread_members.lock().unwrap();
            Ok(members
                .iter()
                .filter(|(tid, _)| tid == thread_id)
                .map(|(_, b)| b.clone())
                .collect())
        }

        async fn find_thread_for_bead(&self, bead: &WorkRef) -> Result<Option<String>> {
            let members = self.thread_members.lock().unwrap();
            Ok(members
                .iter()
                .find(|(_, b)| b == bead)
                .map(|(tid, _)| tid.clone()))
        }
    }

    #[async_trait]
    impl DispatchStore for InMemoryStore {
        async fn upsert_pipeline(&self, state: &PipelineState) -> Result<()> {
            let mut pipelines = self.pipelines.lock().unwrap();
            if let Some(existing) = pipelines.iter_mut().find(|p| p.bead_ref == state.bead_ref) {
                *existing = state.clone();
            } else {
                pipelines.push(state.clone());
            }
            Ok(())
        }

        async fn get_pipeline(&self, bead: &WorkRef) -> Result<Option<PipelineState>> {
            let pipelines = self.pipelines.lock().unwrap();
            Ok(pipelines.iter().find(|p| &p.bead_ref == bead).cloned())
        }

        async fn list_active_pipelines(&self) -> Result<Vec<PipelineState>> {
            let pipelines = self.pipelines.lock().unwrap();
            Ok(pipelines.clone())
        }

        async fn clear_pipeline(&self, bead: &WorkRef) -> Result<()> {
            let mut pipelines = self.pipelines.lock().unwrap();
            pipelines.retain(|p| &p.bead_ref != bead);
            Ok(())
        }

        async fn record_dispatch(&self, record: &DispatchRecord) -> Result<()> {
            let mut dispatches = self.dispatches.lock().unwrap();
            dispatches.push(record.clone());
            Ok(())
        }

        async fn upsert_dispatch(&self, record: &DispatchRecord) -> Result<()> {
            let mut dispatches = self.dispatches.lock().unwrap();
            if let Some(existing) = dispatches.iter_mut().find(|d| d.id == record.id) {
                *existing = record.clone();
            } else {
                dispatches.push(record.clone());
            }
            Ok(())
        }

        async fn complete_dispatch(&self, id: &str, outcome: &str) -> Result<()> {
            let mut dispatches = self.dispatches.lock().unwrap();
            if let Some(d) = dispatches.iter_mut().find(|d| d.id == id) {
                d.completed_at = Some(Utc::now());
                d.outcome = Some(outcome.to_string());
            }
            Ok(())
        }

        async fn update_dispatch_session(&self, id: &str, session_id: &str) -> Result<()> {
            let mut dispatches = self.dispatches.lock().unwrap();
            if let Some(d) = dispatches.iter_mut().find(|d| d.id == id) {
                d.session_id = Some(session_id.to_string());
            }
            Ok(())
        }

        async fn active_dispatches(&self) -> Result<Vec<DispatchRecord>> {
            let dispatches = self.dispatches.lock().unwrap();
            Ok(dispatches
                .iter()
                .filter(|d| d.completed_at.is_none())
                .cloned()
                .collect())
        }
    }

    #[async_trait]
    impl LinkageStore for InMemoryStore {
        async fn add_dependency(&self, dep: &CrossRepoDep) -> Result<()> {
            let mut deps = self.deps.lock().unwrap();
            // Same-repo cycle check: reject if to→from path exists
            if dep.from.repo == dep.to.repo {
                if can_reach(&deps, &dep.to, &dep.from) {
                    anyhow::bail!(
                        "cycle: adding {}/{} → {}/{} would create a per-repo cycle",
                        dep.from.repo,
                        dep.from.bead_id,
                        dep.to.repo,
                        dep.to.bead_id
                    );
                }
            }
            if !deps.iter().any(|d| d.from == dep.from && d.to == dep.to) {
                deps.push(dep.clone());
            }
            Ok(())
        }

        async fn dependencies_of(&self, bead: &WorkRef) -> Result<Vec<CrossRepoDep>> {
            let deps = self.deps.lock().unwrap();
            Ok(deps.iter().filter(|d| &d.from == bead).cloned().collect())
        }

        async fn dependents_of(&self, bead: &WorkRef) -> Result<Vec<CrossRepoDep>> {
            let deps = self.deps.lock().unwrap();
            Ok(deps.iter().filter(|d| &d.to == bead).cloned().collect())
        }

        async fn all_blocking_deps(&self) -> Result<Vec<CrossRepoDep>> {
            let deps = self.deps.lock().unwrap();
            Ok(deps
                .iter()
                .filter(|d| d.dep_type == "blocks" && d.evidence_tier.blocks_dispatch())
                .cloned()
                .collect())
        }

        async fn remove_cross_repo_dep(&self, from: &WorkRef, to: &WorkRef) -> Result<()> {
            let mut deps = self.deps.lock().unwrap();
            deps.retain(|d| !(&d.from == from && &d.to == to));
            Ok(())
        }

        async fn demote_stale_derived(&self, ttl_days: u32) -> Result<u64> {
            let cutoff = Utc::now() - chrono::Duration::days(i64::from(ttl_days));
            let mut deps = self.deps.lock().unwrap();
            let mut demoted = 0u64;
            for dep in deps.iter_mut() {
                if dep.evidence_tier == EvidenceTier::Derived && dep.observed_at < cutoff {
                    dep.evidence_tier = EvidenceTier::Conjectured;
                    demoted += 1;
                }
            }
            Ok(demoted)
        }

        async fn upsert_linear_link(&self, link: &LinearLink) -> Result<()> {
            let mut links = self.linear_links.lock().unwrap();
            if let Some(existing) = links.iter_mut().find(|l| l.bead_ref == link.bead_ref) {
                *existing = link.clone();
            } else {
                links.push(link.clone());
            }
            Ok(())
        }

        async fn find_by_linear_id(&self, linear_id: &str) -> Result<Option<LinearLink>> {
            let links = self.linear_links.lock().unwrap();
            Ok(links.iter().find(|l| l.linear_id == linear_id).cloned())
        }
    }

    /// Minimal UserRepoStore impl so InMemoryStore satisfies the
    /// `BackendStore` blanket impl. The tests that exercise registered
    /// repos use other fixtures; these stubs keep the type system happy
    /// for sibling test modules (e.g. serve/handlers tests) that need
    /// to pass `&InMemoryStore` as `&dyn BackendStore`.
    #[async_trait]
    impl UserRepoStore for InMemoryStore {
        async fn register_repo(&self, _repo: &UserRepo) -> Result<()> {
            Ok(())
        }
        async fn list_user_repos(&self, _user_id: &str) -> Result<Vec<UserRepo>> {
            Ok(Vec::new())
        }
        async fn unregister_repo(&self, _user_id: &str, _repo_name: &str) -> Result<()> {
            Ok(())
        }
    }

    // ── HierarchyStore tests ────────────────────────────

    #[tokio::test]
    async fn decade_upsert_and_get() {
        let store = InMemoryStore::new();
        let decade = DecadeRecord {
            id: "ADR-003".into(),
            title: "Linear hierarchy mapping".into(),
            source_path: "docs/adr/0003-linear-hierarchy-mapping.md".into(),
            status: "proposed".into(),
        };
        store.upsert_decade(&decade).await.unwrap();

        let got = store.get_decade("ADR-003").await.unwrap();
        assert_eq!(got, Some(decade.clone()));

        // Upsert updates existing
        let mut updated = decade;
        updated.status = "active".into();
        store.upsert_decade(&updated).await.unwrap();
        let got = store.get_decade("ADR-003").await.unwrap().unwrap();
        assert_eq!(got.status, "active");
    }

    #[tokio::test]
    async fn decade_list_with_filter() {
        let store = InMemoryStore::new();
        store
            .upsert_decade(&DecadeRecord {
                id: "ADR-001".into(),
                title: "A".into(),
                source_path: "a.md".into(),
                status: "active".into(),
            })
            .await
            .unwrap();
        store
            .upsert_decade(&DecadeRecord {
                id: "ADR-002".into(),
                title: "B".into(),
                source_path: "b.md".into(),
                status: "proposed".into(),
            })
            .await
            .unwrap();

        let all = store.list_decades(None).await.unwrap();
        assert_eq!(all.len(), 2);

        let active = store.list_decades(Some("active")).await.unwrap();
        assert_eq!(active.len(), 1);
        assert_eq!(active[0].id, "ADR-001");
    }

    #[tokio::test]
    async fn get_nonexistent_decade() {
        let store = InMemoryStore::new();
        let got = store.get_decade("nope").await.unwrap();
        assert!(got.is_none());
    }

    #[tokio::test]
    async fn thread_upsert_and_list() {
        let store = InMemoryStore::new();
        let thread = ThreadRecord {
            id: "ADR-003/implementation".into(),
            name: "Linear hierarchy: Implementation".into(),
            decade_id: "ADR-003".into(),
            feature_branch: Some("feat/linear-hierarchy".into()),
        };
        store.upsert_thread(&thread).await.unwrap();

        let threads = store.list_threads("ADR-003").await.unwrap();
        assert_eq!(threads.len(), 1);
        assert_eq!(threads[0], thread);

        // Other decade has no threads
        let empty = store.list_threads("ADR-999").await.unwrap();
        assert!(empty.is_empty());
    }

    #[tokio::test]
    async fn bead_thread_membership() {
        let store = InMemoryStore::new();
        let bead1 = WorkRef {
            repo: "rosary".into(),
            bead_id: "rsry-abc".into(),
            scope: String::new(),
        };
        let bead2 = WorkRef {
            repo: "mache".into(),
            bead_id: "mch-def".into(),
            scope: String::new(),
        };

        store
            .add_bead_to_thread("ADR-003/impl", &bead1)
            .await
            .unwrap();
        store
            .add_bead_to_thread("ADR-003/impl", &bead2)
            .await
            .unwrap();
        // Idempotent
        store
            .add_bead_to_thread("ADR-003/impl", &bead1)
            .await
            .unwrap();

        let members = store.list_beads_in_thread("ADR-003/impl").await.unwrap();
        assert_eq!(members.len(), 2);

        let found = store.find_thread_for_bead(&bead1).await.unwrap();
        assert_eq!(found, Some("ADR-003/impl".into()));

        let not_found = store
            .find_thread_for_bead(&WorkRef {
                repo: "x".into(),
                bead_id: "y".into(),
                scope: String::new(),
            })
            .await
            .unwrap();
        assert!(not_found.is_none());
    }

    // ── DispatchStore tests ─────────────────────────────

    #[tokio::test]
    async fn pipeline_lifecycle() {
        let store = InMemoryStore::new();
        let bead = WorkRef {
            repo: "rosary".into(),
            bead_id: "rsry-001".into(),
            scope: String::new(),
        };
        let state = PipelineState {
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

        // Upsert + get
        store.upsert_pipeline(&state).await.unwrap();
        let got = store.get_pipeline(&bead).await.unwrap().unwrap();
        assert_eq!(got.pipeline_phase, 0);
        assert_eq!(got.last_generation, 42);

        // Update phase
        let mut advanced = state.clone();
        advanced.pipeline_phase = 1;
        advanced.pipeline_agent = "staging-agent".into();
        store.upsert_pipeline(&advanced).await.unwrap();
        let got = store.get_pipeline(&bead).await.unwrap().unwrap();
        assert_eq!(got.pipeline_phase, 1);

        // List active
        let active = store.list_active_pipelines().await.unwrap();
        assert_eq!(active.len(), 1);

        // Clear
        store.clear_pipeline(&bead).await.unwrap();
        assert!(store.get_pipeline(&bead).await.unwrap().is_none());
        assert!(store.list_active_pipelines().await.unwrap().is_empty());
    }

    #[tokio::test]
    async fn dispatch_record_and_complete() {
        let store = InMemoryStore::new();
        let record = DispatchRecord {
            id: "d-001".into(),
            bead_ref: WorkRef {
                repo: "rosary".into(),
                bead_id: "rsry-001".into(),
                scope: String::new(),
            },
            agent: "dev-agent".into(),
            provider: "claude".into(),
            started_at: Utc::now(),
            completed_at: None,
            outcome: None,
            work_dir: "/tmp/work".into(),
            session_id: None,
            workspace_path: None,
            chain_hash: None,
        };

        store.record_dispatch(&record).await.unwrap();

        let active = store.active_dispatches().await.unwrap();
        assert_eq!(active.len(), 1);
        assert_eq!(active[0].id, "d-001");

        store.complete_dispatch("d-001", "success").await.unwrap();

        let active = store.active_dispatches().await.unwrap();
        assert!(active.is_empty());
    }

    #[tokio::test]
    async fn dispatch_update_session_id() {
        let store = InMemoryStore::new();
        let record = DispatchRecord {
            id: "d-002".into(),
            bead_ref: WorkRef {
                repo: "rosary".into(),
                bead_id: "rsry-002".into(),
                scope: String::new(),
            },
            agent: "dev-agent".into(),
            provider: "claude".into(),
            started_at: Utc::now(),
            completed_at: None,
            outcome: None,
            work_dir: "/tmp/work".into(),
            session_id: None,
            workspace_path: Some("/tmp/.rsry-workspaces/rsry-002".into()),
            chain_hash: None,
        };

        store.record_dispatch(&record).await.unwrap();

        // Session ID not set yet
        let active = store.active_dispatches().await.unwrap();
        assert!(active[0].session_id.is_none());
        assert_eq!(
            active[0].workspace_path.as_deref(),
            Some("/tmp/.rsry-workspaces/rsry-002")
        );

        // Update session_id after agent starts
        store
            .update_dispatch_session("d-002", "sess-abc-123")
            .await
            .unwrap();

        let active = store.active_dispatches().await.unwrap();
        assert_eq!(active[0].session_id.as_deref(), Some("sess-abc-123"));
    }

    #[tokio::test]
    async fn upsert_dispatch_idempotent() {
        let store = InMemoryStore::new();
        let record = DispatchRecord {
            id: "d-upsert".into(),
            bead_ref: WorkRef {
                repo: "rosary".into(),
                bead_id: "rsry-001".into(),
                scope: String::new(),
            },
            agent: "dev-agent".into(),
            provider: "claude".into(),
            started_at: Utc::now(),
            completed_at: None,
            outcome: None,
            work_dir: "/tmp/work".into(),
            session_id: None,
            workspace_path: None,
            chain_hash: None,
        };

        // Insert via upsert
        store.upsert_dispatch(&record).await.unwrap();
        assert_eq!(store.active_dispatches().await.unwrap().len(), 1);

        // Upsert again with completion — updates, doesn't duplicate
        let mut completed = record.clone();
        completed.completed_at = Some(Utc::now());
        completed.outcome = Some("success".into());
        store.upsert_dispatch(&completed).await.unwrap();

        // Still one dispatch, now completed
        assert!(store.active_dispatches().await.unwrap().is_empty());
    }

    // ── LinkageStore tests ──────────────────────────────

    #[tokio::test]
    async fn cross_repo_dependency() {
        let store = InMemoryStore::new();
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
            evidence_tier: EvidenceTier::Asserted,
            source: "human".into(),
            observed_at: Utc::now(),
        };
        store.add_dependency(&dep).await.unwrap();
        // Idempotent
        store.add_dependency(&dep).await.unwrap();

        let deps = store.dependencies_of(&from).await.unwrap();
        assert_eq!(deps.len(), 1);
        assert_eq!(deps[0].to, to);

        let dependents = store.dependents_of(&to).await.unwrap();
        assert_eq!(dependents.len(), 1);
        assert_eq!(dependents[0].from, from);
    }

    #[tokio::test]
    async fn linear_link_upsert_and_find() {
        let store = InMemoryStore::new();
        let link = LinearLink {
            bead_ref: WorkRef {
                repo: "rosary".into(),
                bead_id: "rsry-001".into(),
                scope: String::new(),
            },
            linear_id: "AGE-330".into(),
            linear_type: "issue".into(),
        };

        store.upsert_linear_link(&link).await.unwrap();

        let found = store.find_by_linear_id("AGE-330").await.unwrap();
        assert_eq!(found, Some(link.clone()));

        let not_found = store.find_by_linear_id("AGE-999").await.unwrap();
        assert!(not_found.is_none());

        // Upsert changes type
        let mut updated = link;
        updated.linear_type = "sub_issue".into();
        store.upsert_linear_link(&updated).await.unwrap();
        let found = store.find_by_linear_id("AGE-330").await.unwrap().unwrap();
        assert_eq!(found.linear_type, "sub_issue");
    }

    fn make_dep(from_repo: &str, from_id: &str, to_repo: &str, to_id: &str) -> CrossRepoDep {
        CrossRepoDep {
            from: WorkRef {
                repo: from_repo.into(),
                scope: String::new(),
                bead_id: from_id.into(),
            },
            to: WorkRef {
                repo: to_repo.into(),
                scope: String::new(),
                bead_id: to_id.into(),
            },
            dep_type: "blocks".into(),
            evidence_tier: EvidenceTier::Asserted,
            source: "human".into(),
            observed_at: Utc::now(),
        }
    }

    #[tokio::test]
    async fn evidence_tier_blocks_dispatch() {
        assert!(EvidenceTier::Asserted.blocks_dispatch());
        assert!(EvidenceTier::Derived.blocks_dispatch());
        assert!(!EvidenceTier::Conjectured.blocks_dispatch());
    }

    #[tokio::test]
    async fn same_repo_cycle_rejected() {
        let store = InMemoryStore::new();
        // A → B → C; trying to add C → A should fail
        store
            .add_dependency(&make_dep("repo", "a", "repo", "b"))
            .await
            .unwrap();
        store
            .add_dependency(&make_dep("repo", "b", "repo", "c"))
            .await
            .unwrap();
        let cycle = store
            .add_dependency(&make_dep("repo", "c", "repo", "a"))
            .await;
        assert!(cycle.is_err(), "cycle should be rejected");
        assert!(cycle.unwrap_err().to_string().contains("cycle"));
    }

    #[tokio::test]
    async fn cross_repo_cycle_allowed() {
        let store = InMemoryStore::new();
        // A:x → B:y and B:y → A:x is fine (cross-repo SCC = co-dispatch signal)
        store
            .add_dependency(&make_dep("repo-a", "x", "repo-b", "y"))
            .await
            .unwrap();
        store
            .add_dependency(&make_dep("repo-b", "y", "repo-a", "x"))
            .await
            .unwrap();
        let deps = store
            .dependencies_of(&WorkRef {
                repo: "repo-b".into(),
                scope: String::new(),
                bead_id: "y".into(),
            })
            .await
            .unwrap();
        assert_eq!(deps.len(), 1);
    }

    #[tokio::test]
    async fn all_blocking_deps_filters_conjectured() {
        let store = InMemoryStore::new();
        let asserted = make_dep("r", "a", "r2", "b");
        let mut conjectured = make_dep("r", "c", "r2", "d");
        conjectured.evidence_tier = EvidenceTier::Conjectured;
        store.add_dependency(&asserted).await.unwrap();
        store.add_dependency(&conjectured).await.unwrap();
        let blocking = store.all_blocking_deps().await.unwrap();
        assert_eq!(blocking.len(), 1);
        assert_eq!(blocking[0].from.bead_id, "a");
    }

    #[tokio::test]
    async fn demote_stale_derived_works() {
        let store = InMemoryStore::new();
        let mut stale = make_dep("r", "old", "r2", "x");
        stale.evidence_tier = EvidenceTier::Derived;
        stale.observed_at = Utc::now() - chrono::Duration::days(10);
        let mut fresh = make_dep("r", "new", "r2", "y");
        fresh.evidence_tier = EvidenceTier::Derived;
        // fresh.observed_at = now (default)
        store.add_dependency(&stale).await.unwrap();
        store.add_dependency(&fresh).await.unwrap();

        let demoted = store.demote_stale_derived(7).await.unwrap();
        assert_eq!(demoted, 1);

        let blocking = store.all_blocking_deps().await.unwrap();
        assert_eq!(blocking.len(), 1, "only fresh derived should still block");
        assert_eq!(blocking[0].from.bead_id, "new");
    }

    #[tokio::test]
    async fn remove_cross_repo_dep_works() {
        let store = InMemoryStore::new();
        let dep = make_dep("repo-a", "bead-1", "repo-b", "bead-2");
        store.add_dependency(&dep).await.unwrap();
        assert_eq!(store.all_blocking_deps().await.unwrap().len(), 1);
        store
            .remove_cross_repo_dep(&dep.from, &dep.to)
            .await
            .unwrap();
        assert_eq!(store.all_blocking_deps().await.unwrap().len(), 0);
    }
}
