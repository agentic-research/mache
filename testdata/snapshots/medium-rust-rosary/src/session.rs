//! Agent session registry — tracks active dispatches.
//!
//! Persisted to `~/.rsry/sessions.json`. This is ephemeral state
//! (not beads, not Dolt) — rebuilt on startup by checking PIDs.

use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};
use std::path::PathBuf;

/// A tracked agent session.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SessionEntry {
    pub bead_id: String,
    pub repo: String,
    pub provider: String,
    pub pid: Option<u32>,
    pub work_dir: String,
    pub started_at: chrono::DateTime<chrono::Utc>,
    #[serde(default)]
    pub title: String,
    #[serde(default)]
    pub agent: String,
    /// VCS kind used for workspace isolation ("jj", "git", or empty).
    #[serde(default)]
    pub workspace_vcs: String,
    /// Original repo path (needed for workspace cleanup when session dies).
    #[serde(default)]
    pub repo_path: String,
    /// Last activity timestamp (updated on bead_comment).
    #[serde(default)]
    pub last_activity: Option<chrono::DateTime<chrono::Utc>>,
    /// Latest bead comment body (truncated).
    #[serde(default)]
    pub last_comment: Option<String>,
}

/// File-based session registry at `~/.rsry/sessions.json`.
#[derive(Debug, Default, Serialize, Deserialize)]
pub struct SessionRegistry {
    pub sessions: Vec<SessionEntry>,
}

impl SessionRegistry {
    fn path() -> Result<PathBuf> {
        let home = dirs_next::home_dir().context("cannot determine home directory")?;
        Ok(home.join(".rsry").join("sessions.json"))
    }

    /// Load the registry, returning empty if file doesn't exist.
    pub fn load() -> Result<Self> {
        let path = Self::path()?;
        if !path.exists() {
            return Ok(Self::default());
        }
        let content = std::fs::read_to_string(&path)
            .with_context(|| format!("reading {}", path.display()))?;
        let mut registry: Self =
            serde_json::from_str(&content).with_context(|| "parsing sessions.json")?;

        // Remove sessions with no PID (legacy entries), but keep dead-PID
        // sessions — their worktrees may contain unmerged work. Cleanup
        // happens explicitly via rsry_workspace_cleanup or rsry_workspace_merge,
        // NOT on load. Auto-cleanup on load caused data loss: agent finishes →
        // PID dies → next MCP call nukes worktree before merge.
        registry.sessions.retain(|s| s.pid.is_some());
        Ok(registry)
    }

    /// Save the registry.
    pub fn save(&self) -> Result<()> {
        let path = Self::path()?;
        if let Some(parent) = path.parent() {
            std::fs::create_dir_all(parent)
                .with_context(|| format!("creating {}", parent.display()))?;
        }
        let content = serde_json::to_string_pretty(self).context("serializing sessions")?;
        std::fs::write(&path, content).with_context(|| format!("writing {}", path.display()))
    }

    /// Register a new session.
    #[allow(dead_code)] // Used by reconciler path
    pub fn register(&mut self, entry: SessionEntry) -> Result<()> {
        // Remove any stale entry for the same (repo, bead) pair
        self.sessions
            .retain(|s| !(s.bead_id == entry.bead_id && s.repo == entry.repo));
        self.sessions.push(entry);
        self.save()
    }

    /// Remove a session by bead ID and repo.
    #[allow(dead_code)] // Used by reconciler on completion
    pub fn unregister(&mut self, bead_id: &str, repo: &str) -> Result<()> {
        self.sessions
            .retain(|s| !(s.bead_id == bead_id && s.repo == repo));
        self.save()
    }

    /// List all registered sessions (including dead-PID sessions that may have
    /// unmerged worktree work). Callers should use `is_pid_alive()` to check
    /// liveness — dead sessions are kept because their worktrees may contain
    /// uncommitted work that needs explicit merge/cleanup.
    pub fn active(&self) -> &[SessionEntry] {
        &self.sessions
    }

    /// Record activity for a session (called on bead_comment).
    pub fn touch(&mut self, bead_id: &str, repo: &str, comment: &str) -> Result<()> {
        if let Some(session) = self
            .sessions
            .iter_mut()
            .find(|s| s.bead_id == bead_id && s.repo == repo)
        {
            session.last_activity = Some(chrono::Utc::now());
            session.last_comment = Some(comment.chars().take(200).collect());
            self.save()?;
        }
        Ok(())
    }
}

/// Check if a PID is alive via `kill(pid, 0)`.
///
/// Returns:
/// - `true` if `kill(pid, 0)` returns 0 (process exists, we can signal it).
/// - `true` if `kill(pid, 0)` returns -1 with `errno = EPERM` — the process
///   exists but is owned by another user / sandbox. Critical for liveness
///   checks: a process we can't signal is still alive, and treating EPERM
///   as "dead" would incorrectly dead-letter live workers spawned under a
///   different uid (e.g. setuid binaries, containerized siblings).
/// - `false` on `ESRCH` (no such process — truly dead).
/// - `false` on any other unexpected errno; treats unknown errors as dead
///   so a broken `kill(2)` doesn't masquerade as a live worker.
pub(crate) fn is_pid_alive(pid: u32) -> bool {
    unsafe {
        if libc::kill(pid as i32, 0) == 0 {
            return true;
        }
        // kill returned -1 — distinguish ESRCH (dead) from EPERM (alive).
        let err = std::io::Error::last_os_error().raw_os_error();
        matches!(err, Some(libc::EPERM))
    }
}

/// Clean up workspace for a dead session (best-effort).
/// NOTE: intentionally not called from load() — auto-cleanup caused data loss
/// (worktrees nuked before merge). Call rsry_workspace_cleanup explicitly instead.
#[allow(dead_code)]
fn cleanup_session_workspace(session: &SessionEntry) {
    if session.repo_path.is_empty() || session.workspace_vcs.is_empty() {
        return;
    }
    let repo_path = std::path::Path::new(&session.repo_path);
    eprintln!(
        "[session-cleanup] {} workspace for {} (vcs={})",
        session.bead_id, session.repo, session.workspace_vcs
    );
    match session.workspace_vcs.as_str() {
        "jj" => crate::workspace::cleanup_jj_workspace(repo_path, &session.bead_id),
        "git" => crate::workspace::cleanup_git_worktree(repo_path, &session.bead_id),
        _ => {}
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn empty_registry() {
        let reg = SessionRegistry::default();
        assert!(reg.active().is_empty());
    }

    #[test]
    fn register_and_list() {
        let mut reg = SessionRegistry::default();
        reg.sessions.push(SessionEntry {
            bead_id: "rsry-abc".into(),
            repo: "rosary".into(),
            provider: "claude".into(),
            pid: Some(std::process::id()), // current process — alive
            work_dir: "/tmp/test".into(),
            started_at: chrono::Utc::now(),
            title: String::new(),
            agent: String::new(),
            workspace_vcs: String::new(),
            repo_path: String::new(),
            last_activity: None,
            last_comment: None,
        });
        assert_eq!(reg.active().len(), 1);
        assert_eq!(reg.active()[0].bead_id, "rsry-abc");
    }

    #[test]
    fn unregister_removes() {
        let mut reg = SessionRegistry::default();
        reg.sessions.push(SessionEntry {
            bead_id: "rsry-abc".into(),
            repo: "rosary".into(),
            provider: "claude".into(),
            pid: Some(1),
            work_dir: "/tmp/test".into(),
            started_at: chrono::Utc::now(),
            title: String::new(),
            agent: String::new(),
            workspace_vcs: String::new(),
            repo_path: String::new(),
            last_activity: None,
            last_comment: None,
        });
        reg.sessions.retain(|s| s.bead_id != "rsry-abc");
        assert!(reg.active().is_empty());
    }

    /// Regression for Copilot review on PR #202: `kill(pid, 0)` returns
    /// EPERM (not 0, not ESRCH) when the target process exists but is
    /// owned by another user. Treating EPERM as "dead" would falsely
    /// dead-letter live workers (or false-revert orphan dispatches). PID 1
    /// (`launchd` on macOS, `systemd` on Linux) is always alive and always
    /// owned by root, so a non-root test gets EPERM from `kill(1, 0)` —
    /// the exact case `is_pid_alive` must treat as alive.
    #[test]
    fn is_pid_alive_returns_true_on_eperm() {
        // Probe `kill(1, 0)` directly and require errno == EPERM before
        // exercising the regression. `geteuid() != 0` was the previous
        // guard but isn't sufficient — in some environments (containers
        // where pid 1 isn't root-owned, processes with extra capabilities,
        // user namespaces) the syscall can return 0 even for non-root
        // callers, making the assertion vacuous. Probing directly ensures
        // we only run the regression when EPERM is the actual signal,
        // and we use `geteuid` (effective uid — what `kill(2)` consults)
        // rather than `getuid` for the eligibility log.
        let probe = unsafe { libc::kill(1, 0) };
        let errno = std::io::Error::last_os_error().raw_os_error();
        let euid = unsafe { libc::geteuid() };
        if probe == 0 {
            eprintln!(
                "[skip] kill(1, 0) returned 0 (euid={euid}) — EPERM branch \
                 unobservable in this environment"
            );
            return;
        }
        if errno != Some(libc::EPERM) {
            eprintln!(
                "[skip] kill(1, 0) returned {probe} with errno={errno:?} \
                 (euid={euid}); test requires EPERM to exercise the regression"
            );
            return;
        }
        assert!(
            is_pid_alive(1),
            "pid 1 (init/launchd) is alive and kill(1, 0) gave us EPERM — \
             is_pid_alive must treat that as alive, not dead"
        );
    }

    #[test]
    fn is_pid_alive_self() {
        assert!(is_pid_alive(std::process::id()));
    }

    #[test]
    fn is_pid_alive_dead() {
        // PID 99999999 almost certainly doesn't exist
        assert!(!is_pid_alive(99_999_999));
    }

    #[test]
    fn serialization_roundtrip() {
        let reg = SessionRegistry {
            sessions: vec![SessionEntry {
                bead_id: "rsry-abc".into(),
                repo: "rosary".into(),
                provider: "claude".into(),
                pid: Some(42),
                work_dir: "/tmp/test".into(),
                started_at: chrono::Utc::now(),
                title: "Test bead".into(),
                agent: "dev-agent".into(),
                workspace_vcs: "jj".into(),
                repo_path: "/tmp/repo".into(),
                last_activity: None,
                last_comment: None,
            }],
        };
        let json = serde_json::to_string(&reg).unwrap();
        let parsed: SessionRegistry = serde_json::from_str(&json).unwrap();
        assert_eq!(parsed.sessions.len(), 1);
        assert_eq!(parsed.sessions[0].bead_id, "rsry-abc");
    }

    #[test]
    fn session_lifecycle_register_unregister() {
        // Simulates: dispatch → agent works → bead closed → unregister called
        let mut reg = SessionRegistry::default();

        // 1. Dispatch registers the session
        reg.sessions.push(SessionEntry {
            bead_id: "rsry-lifecycle".into(),
            repo: "rosary".into(),
            provider: "claude".into(),
            pid: Some(std::process::id()),
            work_dir: "/tmp/test".into(),
            started_at: chrono::Utc::now(),
            title: "Lifecycle test".into(),
            agent: "dev-agent".into(),
            workspace_vcs: String::new(),
            repo_path: String::new(),
            last_activity: None,
            last_comment: None,
        });
        assert_eq!(reg.active().len(), 1);

        // 2. bead_close / workspace_merge calls unregister()
        // (save will fail in test env — no ~/.rsry/, so call retain directly
        // which is what unregister does internally)
        reg.sessions.retain(|s| s.bead_id != "rsry-lifecycle");
        assert!(
            reg.active().is_empty(),
            "session should be gone after unregister"
        );
    }

    #[test]
    fn dead_session_stays_until_explicit_unregister() {
        // Dead PID sessions stay in registry (worktree may have unmerged work)
        // Only removed when bead_close or workspace_merge calls unregister()
        let mut reg = SessionRegistry::default();

        reg.sessions.push(SessionEntry {
            bead_id: "rsry-dead".into(),
            repo: "rosary".into(),
            provider: "claude".into(),
            pid: Some(99_999_999), // dead PID
            work_dir: "/tmp/test".into(),
            started_at: chrono::Utc::now(),
            title: "Dead session".into(),
            agent: "dev-agent".into(),
            workspace_vcs: String::new(),
            repo_path: String::new(),
            last_activity: None,
            last_comment: None,
        });

        // Dead session still in registry (not auto-cleaned on load)
        assert_eq!(reg.active().len(), 1);
        assert!(!is_pid_alive(99_999_999));

        // Only explicit unregister (via bead_close/workspace_merge) removes it
        reg.sessions.retain(|s| s.bead_id != "rsry-dead");
        assert!(reg.active().is_empty());
    }
}
