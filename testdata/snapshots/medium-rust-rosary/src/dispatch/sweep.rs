//! Orphan-dispatch detection and recovery.
//!
//! A bead is an "orphan dispatch" when:
//! - its state is `BeadState::Dispatched` (i.e. status is `"dispatched"` or
//!   `"in_progress"` — Linear's vocabulary maps to the same state per
//!   `bead.rs:147`)
//! - **and** there is no live agent process working on it: no entry in the
//!   session registry with a live PID, **and** no worktree on disk at
//!   `~/.rsry/worktrees/<repo>/<bead>/`
//!
//! This happens when `rsry_dispatch` stages a worktree + flips status,
//! the caller never spawns the agent (or removes the worktree before
//! spawning), and nothing else cleans up. Without this sweep the bead
//! sits in `Dispatched` forever — invisible-but-harmful state: triage
//! skips it, the dispatcher won't redispatch, the statusline counts it
//! as active. See bead `rosary-67c43d`.
//!
//! The sweep is conservative: it never touches a bead with a live
//! session or an existing worktree. Worst case is a slightly slower
//! `rsry_dispatch` / `rsry_active` (a few `kill(pid, 0)` syscalls + a
//! `Path::exists`).

use crate::bead::BeadState;
use crate::session::{SessionEntry, SessionRegistry, is_pid_alive};
use crate::store::BeadStore;
use std::collections::HashMap;

/// Pre-built lookup of session entries by `(bead_id, repo)`. Callers
/// (the reconciler today) build this ONCE per iteration and pass it
/// into each per-repo `sweep_dead_workers` call. Without the shared
/// index the sweep would rebuild a per-call HashMap making the total
/// work O(repos × sessions) on every iteration — round-9 finding on
/// PR #202.
pub type SessionIndex<'a> = HashMap<(&'a str, &'a str), &'a SessionEntry>;

/// Build the `SessionIndex` from a slice of session entries. One-time
/// O(sessions) work; callers reuse the same index across all repos in
/// an iteration.
pub fn build_session_index(sessions: &[SessionEntry]) -> SessionIndex<'_> {
    sessions
        .iter()
        .map(|s| ((s.bead_id.as_str(), s.repo.as_str()), s))
        .collect()
}

/// Result of a sweep — the bead IDs that were reverted to `open`.
#[derive(Debug, Default, Clone)]
pub struct SweepReport {
    pub reverted: Vec<String>,
}

impl SweepReport {
    /// Used by tests + future MCP `rsry_dispatch_sweep` exposure. Marked
    /// dead-code-allowed because the production callers currently ignore
    /// the report (best-effort cleanup).
    #[allow(dead_code)]
    pub fn is_empty(&self) -> bool {
        self.reverted.is_empty()
    }
}

/// Candidate identified by the liveness sweep — a Dispatched bead whose
/// worker pid is gone. The sweep does NOT perform the state transition
/// itself; callers transition to `dead_letter` via the reconciler's
/// `persist_status` so Linear mirroring + state_change events fire.
#[derive(Debug, Clone)]
pub struct DeadWorkerCandidate {
    pub bead_id: String,
    /// Forensic context — pid, started_at, last_activity, work_dir.
    /// Already written to the event log by the sweep itself; included here
    /// so callers can surface it in user-facing logs without re-querying.
    pub detail: String,
}

/// Result of a liveness sweep — bead IDs identified as needing transition
/// to `dead_letter`. The sweep writes a forensic event to the event log
/// for each but does NOT mutate bead status; the caller is responsible
/// for the actual state transition (typically through the reconciler's
/// `persist_status` path so Linear/issue-tracker mirroring fires).
#[derive(Debug, Default, Clone)]
pub struct LivenessSweepReport {
    pub candidates: Vec<DeadWorkerCandidate>,
}

impl LivenessSweepReport {
    #[allow(dead_code)]
    pub fn is_empty(&self) -> bool {
        self.candidates.is_empty()
    }
}

/// Sweep orphan dispatches in the given repo. Reverts any
/// `BeadState::Dispatched` bead with no live session and no worktree
/// back to `"open"`.
///
/// `repo_path` is the on-disk root of the repo (e.g.
/// `/Users/foo/code/myrepo`); `repo_name` is the basename used to look
/// up worktree locations and session registry entries (e.g. `myrepo`).
pub async fn sweep_orphan_dispatches(
    client: &dyn BeadStore,
    repo_path: &std::path::Path,
    repo_name: &str,
) -> SweepReport {
    let mut report = SweepReport::default();

    let beads = match client.list_beads(repo_name).await {
        Ok(b) => b,
        Err(e) => {
            eprintln!("[sweep] list_beads({repo_name}) failed: {e}");
            return report;
        }
    };

    let registry = SessionRegistry::load().unwrap_or_default();

    for bead in beads {
        if bead.state() != BeadState::Dispatched {
            continue;
        }

        // Live session with a live PID? Leave it alone — there's a real
        // agent at work.
        let has_live_session = registry.active().iter().any(|s| {
            s.bead_id == bead.id && s.repo == repo_name && s.pid.map(is_pid_alive).unwrap_or(false)
        });
        if has_live_session {
            continue;
        }

        // No live session — does a worktree still exist on disk?
        // If so, leave it alone: the agent process may have died but
        // there's unmerged work to recover via rsry_workspace_merge.
        let workspace = crate::workspace::workspace_dir(repo_path, &bead.id);
        if workspace.exists() {
            continue;
        }

        // Orphan confirmed: status is Dispatched, no live PID, no worktree.
        // Revert to open so triage can pick it back up.
        match client.update_status(&bead.id, "open").await {
            Ok(()) => {
                eprintln!(
                    "[sweep] reverted orphan dispatch {} → open (no live session, no worktree)",
                    bead.id
                );
                let _ = client
                    .log_event(
                        &bead.id,
                        "orphan_revert",
                        "no live session + no worktree at sweep time",
                    )
                    .await;
                report.reverted.push(bead.id);
            }
            Err(e) => {
                eprintln!("[sweep] failed to revert {}: {e}", bead.id);
            }
        }
    }

    report
}

/// Liveness sweep: catch dispatched workers whose process is gone.
///
/// Where `sweep_orphan_dispatches` handles "dispatched but never spawned"
/// (no session registered, no worktree on disk → safe to revert to Open),
/// THIS sweep handles "dispatched, spawn happened, worker has since died":
///
/// - bead state is `Dispatched`
/// - SessionRegistry has a matching entry with `pid` set
/// - `is_pid_alive(pid)` returns `false`
///
/// **This sweep does NOT mutate bead status.** It surfaces matching beads
/// as `DeadWorkerCandidate` entries (bead_id + forensic detail) and
/// writes a `deadletter_dead_worker` event to the log; the caller is
/// responsible for the actual `Dispatched → DeadLetter` state
/// transition. In rosary today the caller is `Reconciler::liveness_sweep`,
/// which routes through `persist_status` so the standard `state_change`
/// audit event + Linear/issue-tracker mirroring fire alongside the
/// status change. Bypassing that path (calling `update_status` directly)
/// was the round-7 Copilot finding on PR #202; this contract pins the
/// fix.
///
/// The semantic distinction between this sweep and the
/// `sweep_orphan_dispatches → Open` revert still matters even though
/// neither writes status here anymore: worktrees from dead workers
/// often contain unmerged work the operator wants to recover, so the
/// caller transitions them to `DeadLetter` (operator review required)
/// rather than `Open` (auto-dispatch would wipe the worktree).
///
/// `repo_name` filter scopes the sweep to one repo at a time. Iterate over
/// all configured repos at the call site for global coverage.
pub async fn sweep_dead_workers(
    client: &dyn BeadStore,
    repo_name: &str,
    session_index: &SessionIndex<'_>,
) -> LivenessSweepReport {
    let mut report = LivenessSweepReport::default();

    let beads = match client.list_beads(repo_name).await {
        Ok(b) => b,
        Err(e) => {
            eprintln!("[liveness-sweep] list_beads({repo_name}) failed: {e}");
            return report;
        }
    };

    for bead in beads {
        if bead.state() != BeadState::Dispatched {
            continue;
        }

        // Find the session entry for this bead+repo. If none, this isn't a
        // liveness case — sweep_orphan_dispatches handles "never spawned".
        let session = match session_index.get(&(bead.id.as_str(), repo_name)) {
            Some(s) => *s,
            None => continue,
        };

        let pid = match session.pid {
            Some(p) => p,
            // Entry exists but pid is None — registration was incomplete.
            // Don't touch; sweep_orphan_dispatches will revert if there's
            // also no worktree.
            None => continue,
        };

        if is_pid_alive(pid) {
            continue; // Healthy session, leave alone.
        }

        // Worker is gone. Surface as a candidate. The actual `dead_letter`
        // status transition is performed by the caller (the reconciler)
        // via `persist_status` so Linear mirroring + state_change events
        // fire — bypassing it would skip the audit/sync round.
        let detail = format!(
            "pid={} started_at={} last_activity={:?} work_dir={}",
            pid,
            session.started_at.to_rfc3339(),
            session.last_activity.map(|t| t.to_rfc3339()),
            session.work_dir,
        );
        // No eprintln here — the reconciler logs once per bead with the
        // forensic detail. Logging in both layers (round-4 / round-9
        // findings) just adds daemon-mode noise.
        let _ = client
            .log_event(&bead.id, "deadletter_dead_worker", &detail)
            .await;
        report.candidates.push(DeadWorkerCandidate {
            bead_id: bead.id,
            detail,
        });
    }

    report
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::bead_sqlite::SqliteBeadStore;
    use tempfile::TempDir;

    fn test_store_in(dir: &std::path::Path) -> SqliteBeadStore {
        SqliteBeadStore::connect(&dir.join("beads.db")).unwrap()
    }

    #[tokio::test]
    async fn sweep_reverts_dispatched_bead_with_no_session_no_worktree() {
        let tmp = TempDir::new().unwrap();
        let store = test_store_in(tmp.path());
        store
            .create_bead("orphan-1", "T", "", 1, "task")
            .await
            .unwrap();
        store.update_status("orphan-1", "dispatched").await.unwrap();

        let report = sweep_orphan_dispatches(&store, tmp.path(), "no-such-repo-xyz").await;
        // No worktree exists at ~/.rsry/worktrees/no-such-repo-xyz/orphan-1,
        // no session registered → orphan reverted.
        assert_eq!(report.reverted, vec!["orphan-1".to_string()]);

        let bead = store
            .get_bead("orphan-1", "no-such-repo-xyz")
            .await
            .unwrap()
            .unwrap();
        assert_eq!(bead.status, "open");
    }

    #[tokio::test]
    async fn sweep_also_reverts_in_progress_alias() {
        // Linear sync writes "in_progress" — bead.rs:147 maps it to
        // BeadState::Dispatched. The sweep must catch this alias too.
        let tmp = TempDir::new().unwrap();
        let store = test_store_in(tmp.path());
        store
            .create_bead("orphan-2", "T", "", 1, "task")
            .await
            .unwrap();
        store
            .update_status("orphan-2", "in_progress")
            .await
            .unwrap();

        let report = sweep_orphan_dispatches(&store, tmp.path(), "no-such-repo-xyz").await;
        assert_eq!(report.reverted, vec!["orphan-2".to_string()]);
    }

    #[tokio::test]
    async fn sweep_leaves_open_beads_alone() {
        let tmp = TempDir::new().unwrap();
        let store = test_store_in(tmp.path());
        store.create_bead("a", "T", "", 1, "task").await.unwrap();
        // Status is "open" by default; sweep must not touch it.

        let report = sweep_orphan_dispatches(&store, tmp.path(), "no-such-repo-xyz").await;
        assert!(report.is_empty());
    }

    #[tokio::test]
    async fn sweep_leaves_dispatched_with_existing_worktree() {
        let tmp = TempDir::new().unwrap();
        let store = test_store_in(tmp.path());
        store
            .create_bead("guarded", "T", "", 1, "task")
            .await
            .unwrap();
        store.update_status("guarded", "dispatched").await.unwrap();

        // The sweep computes the worktree path from `repo_path` basename
        // (`workspace::workspace_dir`), so pre-create at exactly that path
        // — otherwise the lookup misses and the bead gets falsely reverted.
        let workspace = crate::workspace::workspace_dir(tmp.path(), "guarded");
        std::fs::create_dir_all(&workspace).unwrap();

        let report = sweep_orphan_dispatches(&store, tmp.path(), "sweep-test-guarded-repo").await;
        assert!(
            report.is_empty(),
            "must not revert when worktree still exists — unmerged work might be there",
        );
        let bead = store
            .get_bead("guarded", "sweep-test-guarded-repo")
            .await
            .unwrap()
            .unwrap();
        assert_eq!(bead.status, "dispatched");

        // Cleanup — remove the test worktree dir so re-runs don't accumulate.
        let _ = std::fs::remove_dir_all(&workspace);
    }

    // ---- sweep_dead_workers ---------------------------------------------

    /// Synthesize a `SessionEntry` for tests. The `pid` field is what the
    /// sweep checks; everything else is forensic context.
    fn fake_session(bead_id: &str, repo: &str, pid: Option<u32>) -> crate::session::SessionEntry {
        crate::session::SessionEntry {
            bead_id: bead_id.to_string(),
            repo: repo.to_string(),
            provider: "claude".to_string(),
            pid,
            work_dir: "/tmp/test-work-dir".to_string(),
            started_at: chrono::Utc::now(),
            title: format!("test bead {bead_id}"),
            agent: "scoping-agent".to_string(),
            workspace_vcs: "git".to_string(),
            repo_path: "/tmp/test-repo".to_string(),
            last_activity: None,
            last_comment: None,
        }
    }

    /// Spawn a real subprocess we can kill to get a guaranteed-dead pid.
    /// Returns the pid AFTER killing + reaping the child.
    fn spawn_and_reap() -> u32 {
        let mut child = std::process::Command::new("sh")
            .args(["-c", "exit 0"])
            .spawn()
            .expect("spawn");
        let pid = child.id();
        let _ = child.wait();
        // pid is now reaped; kill(pid, 0) → ESRCH on subsequent checks.
        pid
    }

    #[tokio::test]
    async fn liveness_sweep_deadletters_dead_worker() {
        let tmp = TempDir::new().unwrap();
        let store = test_store_in(tmp.path());
        store
            .create_bead("dead-w-1", "T", "", 1, "task")
            .await
            .unwrap();
        store.update_status("dead-w-1", "dispatched").await.unwrap();

        let dead_pid = spawn_and_reap();
        let sessions = vec![fake_session("dead-w-1", "test-repo", Some(dead_pid))];

        let report = sweep_dead_workers(&store, "test-repo", &build_session_index(&sessions)).await;
        // After round-7 refactor: sweep returns CANDIDATES; the caller is
        // responsible for the actual `dead_letter` transition via
        // `persist_status` (Linear mirroring + state_change event).
        assert_eq!(report.candidates.len(), 1);
        assert_eq!(report.candidates[0].bead_id, "dead-w-1");
        assert!(
            report.candidates[0]
                .detail
                .contains(&format!("pid={dead_pid}")),
            "forensic detail must include pid; got: {}",
            report.candidates[0].detail
        );

        // Sweep itself MUST NOT mutate status — that's the caller's job
        // (the reconciler routes through persist_status). Test the
        // separation of concerns explicitly so a regression that re-adds
        // direct update_status calls would fail here.
        let bead = store
            .get_bead("dead-w-1", "test-repo")
            .await
            .unwrap()
            .unwrap();
        assert_eq!(
            bead.status, "dispatched",
            "sweep is candidate-only; status transition happens at the reconciler layer"
        );
    }

    #[tokio::test]
    async fn liveness_sweep_leaves_live_workers_alone() {
        let tmp = TempDir::new().unwrap();
        let store = test_store_in(tmp.path());
        store
            .create_bead("live-w", "T", "", 1, "task")
            .await
            .unwrap();
        store.update_status("live-w", "dispatched").await.unwrap();

        // Use our own pid — guaranteed alive.
        let our_pid = std::process::id();
        let sessions = vec![fake_session("live-w", "test-repo", Some(our_pid))];

        let report = sweep_dead_workers(&store, "test-repo", &build_session_index(&sessions)).await;
        assert!(report.is_empty(), "live pid must not be deadlettered");

        let bead = store
            .get_bead("live-w", "test-repo")
            .await
            .unwrap()
            .unwrap();
        assert_eq!(bead.status, "dispatched");
    }

    #[tokio::test]
    async fn liveness_sweep_skips_beads_without_session_entry() {
        // sweep_dead_workers is specifically for the "session existed, pid
        // is now dead" path. Beads with no session entry are sweep_orphan_
        // dispatches's territory (never spawned). They must NOT get
        // deadlettered — that would lose the dispatch lifecycle distinction.
        let tmp = TempDir::new().unwrap();
        let store = test_store_in(tmp.path());
        store
            .create_bead("no-session", "T", "", 1, "task")
            .await
            .unwrap();
        store
            .update_status("no-session", "dispatched")
            .await
            .unwrap();

        let sessions: Vec<crate::session::SessionEntry> = vec![]; // empty registry

        let report = sweep_dead_workers(&store, "test-repo", &build_session_index(&sessions)).await;
        assert!(
            report.is_empty(),
            "bead with no session entry must NOT be deadlettered (that's sweep_orphan_dispatches's job)"
        );
    }

    #[tokio::test]
    async fn liveness_sweep_skips_non_dispatched_beads() {
        let tmp = TempDir::new().unwrap();
        let store = test_store_in(tmp.path());
        store
            .create_bead("not-dispatched", "T", "", 1, "task")
            .await
            .unwrap();
        // status stays "open"

        let dead_pid = spawn_and_reap();
        let sessions = vec![fake_session("not-dispatched", "test-repo", Some(dead_pid))];

        let report = sweep_dead_workers(&store, "test-repo", &build_session_index(&sessions)).await;
        assert!(
            report.is_empty(),
            "open bead must not be deadlettered even with a dead session entry"
        );
    }

    #[tokio::test]
    async fn liveness_sweep_skips_session_with_none_pid() {
        // Session entry exists but pid is None — registration was incomplete.
        // sweep_orphan_dispatches handles that case (worktree check); this
        // sweep should leave it alone.
        let tmp = TempDir::new().unwrap();
        let store = test_store_in(tmp.path());
        store
            .create_bead("no-pid", "T", "", 1, "task")
            .await
            .unwrap();
        store.update_status("no-pid", "dispatched").await.unwrap();

        let sessions = vec![fake_session("no-pid", "test-repo", None)];

        let report = sweep_dead_workers(&store, "test-repo", &build_session_index(&sessions)).await;
        assert!(report.is_empty());
    }

    #[tokio::test]
    async fn liveness_sweep_only_touches_matching_repo() {
        let tmp = TempDir::new().unwrap();
        let store = test_store_in(tmp.path());
        store
            .create_bead("xrepo", "T", "", 1, "task")
            .await
            .unwrap();
        store.update_status("xrepo", "dispatched").await.unwrap();

        let dead_pid = spawn_and_reap();
        // Session entry is for a DIFFERENT repo than the sweep is scoped to.
        let sessions = vec![fake_session("xrepo", "OTHER-repo", Some(dead_pid))];

        let report = sweep_dead_workers(&store, "test-repo", &build_session_index(&sessions)).await;
        assert!(
            report.is_empty(),
            "sweep must not deadletter when session entry is for a different repo"
        );
    }
}
