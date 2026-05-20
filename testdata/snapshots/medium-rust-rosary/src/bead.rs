use std::fmt;

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};

/// Bead lifecycle states — modeled as a Labeled Transition System.
///
/// Transitions:
///   backlog → open (human/agent promotes after refinement)
///   open → queued (triage selects)
///   queued → dispatched (semaphore acquired)
///   dispatched → verifying (agent exits)
///   verifying → done (all tiers pass)
///   verifying → rejected (tier fails)
///   verifying → blocked (needs human / partial)
///   rejected → open (retry after backoff)
///   blocked → open (dependency resolved / manual unblock)
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum BeadState {
    /// Refinement zone — human + agent shape the work. Never auto-dispatched.
    Backlog,
    Open,
    Queued,
    Dispatched,
    Verifying,
    /// Pipeline complete, PR created, awaiting merge. NOT eligible for dispatch.
    PrOpen,
    Done,
    Rejected,
    Blocked,
    Stale,
    /// Worker died mid-flight (process exited / SIGKILL'd / crashed) and the
    /// reconciler's liveness check detected it. The bead is NOT eligible for
    /// auto-dispatch — an operator must explicitly transition it (retry → Open,
    /// or close as won't-fix → Done). Worktree + session-registry entry are
    /// preserved for forensics. Different from `Stale` (no-activity timer) and
    /// `Blocked` (dependency wait) — DeadLetter specifically means "we tried
    /// and the worker process is gone."
    DeadLetter,
}

impl BeadState {
    /// Valid successor states from this state.
    ///
    /// Used by `can_transition_to()` to enforce the LTS at write time.
    /// Includes both the "happy path" transitions and operational recovery edges
    /// (e.g., `Dispatched → Open` for stuck-bead recovery).
    pub fn valid_transitions(self) -> &'static [BeadState] {
        match self {
            BeadState::Backlog => &[BeadState::Open],
            // Open → Dispatched: reconciler's triage→dispatch path (skips Queued in practice)
            BeadState::Open => &[BeadState::Queued, BeadState::Dispatched],
            BeadState::Queued => &[BeadState::Dispatched],
            // Dispatched → Open: recovery for stuck agents (cold-start path)
            // Dispatched → DeadLetter: liveness sweep detected dead worker mid-flight
            BeadState::Dispatched => {
                &[BeadState::Verifying, BeadState::Open, BeadState::DeadLetter]
            }
            // Verifying → PrOpen: pipeline complete, PR created
            // Verifying → Open: phase failed, retry (reconciler uses "open" not "rejected")
            // Verifying → DeadLetter: verify worker died
            BeadState::Verifying => &[
                BeadState::Done,
                BeadState::Rejected,
                BeadState::Blocked,
                BeadState::PrOpen,
                BeadState::Open,
                BeadState::DeadLetter,
            ],
            BeadState::PrOpen => &[BeadState::Done],
            BeadState::Rejected => &[BeadState::Open],
            BeadState::Blocked => &[BeadState::Open],
            BeadState::Done => &[],
            BeadState::Stale => &[BeadState::Open],
            // DeadLetter is a manual-resolution state. Operator either retries
            // (→ Open) or closes as won't-fix (→ Done). No auto-transitions.
            BeadState::DeadLetter => &[BeadState::Open, BeadState::Done],
        }
    }

    /// Check if transitioning to `next` is valid.
    pub fn can_transition_to(self, next: BeadState) -> bool {
        self.valid_transitions().contains(&next)
    }

    /// Whether this state is terminal (no further transitions).
    #[allow(dead_code)]
    pub fn is_terminal(self) -> bool {
        self.valid_transitions().is_empty()
    }

    /// Map bead state to a Linear state type + preferred name.
    /// Returns (type, preferred_name) — the tracker resolves to an actual state ID.
    /// Type is stable across all Linear teams; name is a hint for teams that have it.
    pub fn to_linear_type(self) -> (&'static str, &'static str) {
        match self {
            BeadState::Backlog => ("backlog", "Backlog"),
            BeadState::Open | BeadState::Rejected | BeadState::Stale => ("unstarted", "Todo"),
            BeadState::Queued => ("unstarted", "Todo"),
            BeadState::Dispatched => ("started", "In Progress"),
            BeadState::Verifying => ("started", "In Review"),
            BeadState::PrOpen => ("started", "In Review"),
            BeadState::Done => ("completed", "Done"),
            BeadState::Blocked => ("backlog", "Backlog"),
            // DeadLetter maps to "canceled" so Linear stops nagging — the bead
            // needs explicit operator review before re-entering the pipeline.
            BeadState::DeadLetter => ("canceled", "Dead Letter"),
        }
    }

    /// Map a Linear state type to a BeadState.
    /// Type-based mapping is stable across all Linear configurations.
    /// Optional name hint refines within a type (e.g., "In Review" → Verifying
    /// vs "In Progress" → Dispatched, both type=started).
    pub fn from_linear_type(state_type: &str, state_name: &str) -> Self {
        match state_type {
            "completed" => BeadState::Done,
            // Refine "canceled" by name: a state literally named "Dead Letter"
            // (the upstream label `to_linear_type` emits for DeadLetter) round-
            // trips back to DeadLetter. Anything else canceled — workflow-level
            // cancellations, manual closes — stays Done. Without this branch the
            // Linear→rosary webhook would silently demote a DeadLetter bead to
            // Done on every sync, losing the operator-triage semantics.
            "canceled" => {
                let lowered = state_name.to_lowercase();
                if lowered.contains("dead letter") || lowered.contains("dead_letter") {
                    BeadState::DeadLetter
                } else {
                    BeadState::Done
                }
            }
            "started" => {
                // Refine by name within the "started" type
                if state_name.to_lowercase().contains("review") {
                    BeadState::Verifying
                } else {
                    BeadState::Dispatched
                }
            }
            "backlog" => BeadState::Backlog,
            "unstarted" => BeadState::Open,
            _ => BeadState::Open,
        }
    }
}

impl fmt::Display for BeadState {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let s = match self {
            BeadState::Backlog => "backlog",
            BeadState::Open => "open",
            BeadState::Queued => "queued",
            BeadState::Dispatched => "dispatched",
            BeadState::Verifying => "verifying",
            BeadState::PrOpen => "pr_open",
            BeadState::Done => "done",
            BeadState::Rejected => "rejected",
            BeadState::Blocked => "blocked",
            BeadState::Stale => "stale",
            BeadState::DeadLetter => "dead_letter",
        };
        write!(f, "{s}")
    }
}

impl From<&str> for BeadState {
    fn from(s: &str) -> Self {
        match s {
            "backlog" => BeadState::Backlog,
            "open" => BeadState::Open,
            "queued" => BeadState::Queued,
            "dispatched" => BeadState::Dispatched,
            "verifying" => BeadState::Verifying,
            "done" | "closed" => BeadState::Done,
            "rejected" => BeadState::Rejected,
            "blocked" => BeadState::Blocked,
            "stale" => BeadState::Stale,
            "pr_open" => BeadState::PrOpen,
            "in_progress" => BeadState::Dispatched, // legacy mapping
            "dead_letter" | "deadletter" => BeadState::DeadLetter,
            _ => BeadState::Open,
        }
    }
}

/// All bead types require scopes (files or directories) for overlap detection.
///
/// Files: `src/reconcile.rs` (exact path, no trailing slash)
/// Directories: `crates/bdr/` or `src/` (trailing slash = prefix match)
/// Repo-wide: `./` (blocks all dispatch in that repo — use sparingly)
///
/// This enables parallel dispatch: beads with non-overlapping scopes can
/// run concurrently, while overlapping scopes serialize execution.
/// All recognized `issue_type` values accepted by the MCP API.
/// Used for validation in tool handlers and as the canonical source for MCP schema docs.
pub const VALID_ISSUE_TYPES: &[&str] = &[
    "bug", "feature", "task", "chore", "review", "epic", "design", "research",
];

pub fn requires_files(issue_type: &str) -> bool {
    // Epics/design/research are planning beads — they don't touch code directly.
    // They decompose into child beads that DO have file scopes.
    !matches!(issue_type, "epic" | "design" | "research")
}

/// PATCH-style update for bead fields. Only `Some` fields are written;
/// `None` fields are left unchanged. Used by `rsry_bead_update` MCP tool
/// and the `IssueTracker::update_fields` trait method.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct BeadUpdate {
    pub title: Option<String>,
    pub description: Option<String>,
    pub priority: Option<u8>,
    pub issue_type: Option<String>,
    pub owner: Option<String>,
    pub files: Option<Vec<String>>,
    pub test_files: Option<Vec<String>>,
}

impl BeadUpdate {
    /// Returns true if no fields are set (nothing to update).
    pub fn is_empty(&self) -> bool {
        self.title.is_none()
            && self.description.is_none()
            && self.priority.is_none()
            && self.issue_type.is_none()
            && self.owner.is_none()
            && self.files.is_none()
            && self.test_files.is_none()
    }
}

/// A bead is a file-scoped work item tracked in a repo's .beads/ directory.
/// This is the common representation used across scanner, sync, and dispatch.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Bead {
    pub id: String,
    pub title: String,
    pub description: String,
    pub status: String,
    pub priority: u8,
    pub issue_type: String,
    pub owner: Option<String>,
    pub repo: String,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
    pub dependency_count: u32,
    pub dependent_count: u32,
    pub comment_count: u32,
    /// Git branch or jj bookmark name associated with this bead.
    pub branch: Option<String>,
    /// GitHub/GitLab PR URL associated with this bead.
    pub pr_url: Option<String>,
    /// jj change ID (immutable, preferred over branch for jj workflows).
    pub jj_change_id: Option<String>,
    /// External reference for cross-repo tracking (e.g., "kiln:ll-packaging").
    /// Format: "repo_name:label" — repo_name maps to a repo in rosary.toml.
    pub external_ref: Option<String>,
    /// Source files this bead touches (scopes agent dispatch).
    #[serde(default)]
    pub files: Vec<String>,
    /// Test files to validate the change.
    #[serde(default)]
    pub test_files: Vec<String>,
    /// Git username of the creator (from `git config user.name` at creation time).
    /// None if created in a non-git context or if git config is absent.
    /// Not included in generation() — metadata, not semantic content.
    #[serde(default)]
    pub created_by: Option<String>,
    /// Team/folder scope within a monorepo (e.g. "auth", "payments/core").
    /// Empty string for cross-repo and single-team repos — backward compatible.
    #[serde(default)]
    pub scope: String,
    /// Ordered provenance chain — primary source first, then secondary.
    /// Populated from the notes JSON at read time; not included in generation().
    #[serde(default)]
    pub derived_from: Vec<bdr::provenance::ProvenanceRef>,
}

/// One comment on a bead, with audit-trail fields. Returned by
/// [`crate::store::BeadStore::list_comments`] and the MCP/CLI surfaces.
///
/// Audit-trail invariants (rosary-a96b06):
/// - `id` is a stable surrogate key. Update/delete address this id.
/// - `original_text` is set on the **first** edit and immutable thereafter
///   — preserves a clean "what did this say originally" record without
///   storing a full revision history.
/// - `edited_at` is `None` on a never-edited comment and the latest edit
///   timestamp once edited.
/// - `deleted_at` is `None` for live comments and a timestamp for
///   soft-deleted ones. Hard-delete removes the row entirely.
/// - `text` always reflects the current displayable body (the post-edit
///   content for edited comments; the original for never-edited).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct Comment {
    /// Stable unique id. Dolt produces a UUID (char(36)); SQLite produces a
    /// stringified integer. Both are opaque from the public API's
    /// perspective — callers pass strings and pattern-match on `id` exactly.
    pub id: String,
    pub issue_id: String,
    pub text: String,
    pub author: String,
    pub created_at: DateTime<Utc>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub edited_at: Option<DateTime<Utc>>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub edit_reason: Option<String>,
    /// Original body, captured on first edit. `None` if the comment has
    /// never been edited (in which case `text` IS the original).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub original_text: Option<String>,
    /// Soft-delete marker. Live comments have `None`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub deleted_at: Option<DateTime<Utc>>,
    /// Optional rationale recorded when the comment was deleted. Independent
    /// of `edit_reason` so the deletion reason is preserved even when the
    /// comment was previously edited.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub delete_reason: Option<String>,
}

impl Comment {
    /// True iff this comment has ever been edited (i.e. `edited_at` is set).
    pub fn is_edited(&self) -> bool {
        self.edited_at.is_some()
    }

    /// True iff this comment is soft-deleted.
    pub fn is_deleted(&self) -> bool {
        self.deleted_at.is_some()
    }

    /// The body that was originally written. For never-edited comments,
    /// this is just `text`. For edited comments, this is `original_text`
    /// (which is required to be `Some` post-edit by the audit invariant).
    #[allow(dead_code)] // Public surface; consumed by future audit/scrub tooling.
    pub fn body_as_originally_written(&self) -> &str {
        self.original_text.as_deref().unwrap_or(&self.text)
    }
}

impl Bead {
    /// Content-based generation hash. Changes when semantic content changes,
    /// but not when status/timestamps change. Used for idempotency —
    /// if generation matches last processed, skip re-dispatch.
    ///
    /// Uses SHA-256 (first 8 bytes as u64) for cross-restart determinism.
    /// `std::collections::hash_map::DefaultHasher` is explicitly NOT used here
    /// because its output is not stable across process restarts (Rust docs warn).
    pub fn generation(&self) -> u64 {
        use sha2::{Digest, Sha256};
        let mut h = Sha256::new();
        h.update(self.id.as_bytes());
        h.update(b"\0");
        h.update(self.title.as_bytes());
        h.update(b"\0");
        h.update(self.description.as_bytes());
        h.update(b"\0");
        h.update([self.priority]);
        let digest = h.finalize();
        u64::from_le_bytes(digest[..8].try_into().unwrap())
    }

    /// Parse the status string into a typed BeadState.
    pub fn state(&self) -> BeadState {
        BeadState::from(self.status.as_str())
    }

    /// Parse a `Bead` from a JSON value matching the rosary bead JSON shape.
    ///
    /// Returns `None` if any required field is missing or invalid.
    /// `id`, `title`, `status`, and `priority` are required — a bare
    /// `{"id":"x","title":"y"}` must NOT silently create a dispatchable bead.
    #[allow(dead_code)] // used in tests and future CLI integration
    pub fn from_json(value: &serde_json::Value, repo: &str) -> Option<Self> {
        Some(Bead {
            id: value.get("id")?.as_str()?.to_string(),
            title: value.get("title")?.as_str()?.to_string(),
            description: value
                .get("description")
                .and_then(|v| v.as_str())
                .unwrap_or("")
                .to_string(),
            // status and priority are required — default values here would silently
            // create dispatchable beads from incomplete JSON (e.g. {"id","title"} → priority-2 open bead)
            status: value.get("status")?.as_str()?.to_string(),
            priority: value.get("priority")?.as_u64()? as u8,
            issue_type: value
                .get("issue_type")
                .and_then(|v| v.as_str())
                .unwrap_or("task")
                .to_string(),
            owner: value
                .get("owner")
                .and_then(|v| v.as_str())
                .map(|s| s.to_string()),
            repo: repo.to_string(),
            created_at: parse_datetime(value.get("created_at")),
            updated_at: parse_datetime(value.get("updated_at")),
            dependency_count: value
                .get("dependency_count")
                .and_then(|v| v.as_u64())
                .unwrap_or(0) as u32,
            dependent_count: value
                .get("dependent_count")
                .and_then(|v| v.as_u64())
                .unwrap_or(0) as u32,
            comment_count: value
                .get("comment_count")
                .and_then(|v| v.as_u64())
                .unwrap_or(0) as u32,
            branch: value
                .get("branch")
                .and_then(|v| v.as_str())
                .map(|s| s.to_string()),
            pr_url: value
                .get("pr_url")
                .and_then(|v| v.as_str())
                .map(|s| s.to_string()),
            jj_change_id: value
                .get("jj_change_id")
                .and_then(|v| v.as_str())
                .map(|s| s.to_string()),
            external_ref: value
                .get("external_ref")
                .and_then(|v| v.as_str())
                .map(|s| s.to_string()),
            files: value
                .get("files")
                .and_then(|v| v.as_array())
                .map(|arr| {
                    arr.iter()
                        .filter_map(|v| v.as_str().map(|s| s.to_string()))
                        .collect()
                })
                .unwrap_or_default(),
            test_files: value
                .get("test_files")
                .and_then(|v| v.as_array())
                .map(|arr| {
                    arr.iter()
                        .filter_map(|v| v.as_str().map(|s| s.to_string()))
                        .collect()
                })
                .unwrap_or_default(),
            created_by: None,
            scope: String::new(),
            derived_from: vec![],
        })
    }

    pub fn is_ready(&self) -> bool {
        self.status == "open" && self.dependency_count == 0
    }

    /// A bead is blocked if it has unresolved dependencies OR its status is explicitly "blocked".
    /// This is the single definition — used by both status counts and list filtering.
    pub fn is_blocked(&self) -> bool {
        self.status == "blocked" || (self.status == "open" && self.dependency_count > 0)
    }

    /// Golden Rule 12: implementation beads need refinement before dispatch.
    /// Returns true if this is an implementation bead (bug/feature/task/chore) with
    /// an empty or trivially short description — meaning the 5-whys haven't been answered.
    /// Research, design, epic, and review beads are exempt (they ARE the research step).
    pub fn needs_refinement(&self) -> bool {
        const MIN_DESCRIPTION_LEN: usize = 50;
        matches!(
            self.issue_type.as_str(),
            "bug" | "feature" | "task" | "chore"
        ) && self.description.trim().len() < MIN_DESCRIPTION_LEN
    }

    /// Returns true when the description contains a verifiable test/build command.
    /// Used by `rsry bead close` to enforce that implementation beads ship with
    /// runnable verification before being marked done. Research/epic/design beads
    /// are exempt — they describe work, they don't claim a behavior was shipped.
    pub fn has_verifiable_test_command(&self) -> bool {
        if !matches!(
            self.issue_type.as_str(),
            "bug" | "feature" | "task" | "chore"
        ) {
            return true; // exempt
        }
        verify::looks_like_test_command(&self.description)
    }
}

mod verify {
    /// Heuristic: does this text contain a runnable test/build command?
    /// Recognised: cargo test/check/build, pytest, npm/pnpm/yarn test,
    /// go test, make test, task test, just test.
    pub fn looks_like_test_command(text: &str) -> bool {
        const PATTERNS: &[&str] = &[
            "cargo test",
            "cargo check",
            "cargo build",
            "pytest",
            "npm test",
            "npm run test",
            "pnpm test",
            "yarn test",
            "go test",
            "make test",
            "task test",
            "just test",
        ];
        let lower = text.to_lowercase();
        PATTERNS.iter().any(|p| lower.contains(p))
    }

    #[cfg(test)]
    mod tests {
        use super::looks_like_test_command;

        #[test]
        fn detects_cargo_test() {
            assert!(looks_like_test_command(
                "Run with `cargo test -p rosary verify`"
            ));
        }

        #[test]
        fn detects_pytest_in_inline_block() {
            assert!(looks_like_test_command(
                "Success when: pytest tests/test_x.py passes"
            ));
        }

        #[test]
        fn rejects_plain_prose() {
            assert!(!looks_like_test_command(
                "This bead refactors the docs section and adds an example."
            ));
        }

        #[test]
        fn rejects_empty() {
            assert!(!looks_like_test_command(""));
        }

        #[test]
        fn case_insensitive() {
            assert!(looks_like_test_command("CARGO TEST"));
        }
    }
}

impl fmt::Display for Bead {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "[{}] {} ({})", self.id, self.title, self.status,)?;
        if let Some(ref branch) = self.branch {
            write!(f, " branch={branch}")?;
        }
        if let Some(ref pr_url) = self.pr_url {
            write!(f, " pr={pr_url}")?;
        }
        if let Some(ref jj_id) = self.jj_change_id {
            write!(f, " jj={jj_id}")?;
        }
        Ok(())
    }
}

#[allow(dead_code)]
fn parse_datetime(v: Option<&serde_json::Value>) -> DateTime<Utc> {
    v.and_then(|v| v.as_str())
        .and_then(|s| s.parse().ok())
        .unwrap_or_default()
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn parse_bd_json_output() {
        let val = json!({
            "id": "mache-tgl",
            "title": "[graphfs.go] Replace interface{} with any",
            "description": "Trivial modernization",
            "status": "open",
            "priority": 2,
            "issue_type": "chore",
            "owner": "jamestexas",
            "created_at": "2026-03-12T22:13:27Z",
            "updated_at": "2026-03-12T22:13:27Z",
            "dependency_count": 0,
            "dependent_count": 0,
            "comment_count": 0
        });

        let bead = Bead::from_json(&val, "mache").unwrap();
        assert_eq!(bead.id, "mache-tgl");
        assert_eq!(bead.repo, "mache");
        assert!(bead.is_ready());
    }

    #[test]
    fn state_from_string() {
        assert_eq!(BeadState::from("backlog"), BeadState::Backlog);
        assert_eq!(BeadState::from("open"), BeadState::Open);
        assert_eq!(BeadState::from("queued"), BeadState::Queued);
        // Regression for Copilot review on PR #202: Linear round-trip used
        // to demote DeadLetter to Done because `canceled` type mapped back
        // unconditionally to Done. Now the "Dead Letter" name within the
        // `canceled` type round-trips to DeadLetter; everything else
        // canceled (workflow cancellations) stays Done.
        let (ty, name) = BeadState::DeadLetter.to_linear_type();
        assert_eq!(
            BeadState::from_linear_type(ty, name),
            BeadState::DeadLetter,
            "DeadLetter must survive Linear round-trip via to/from_linear_type"
        );
        // Sanity: an actual cancellation (not our managed DeadLetter) still maps to Done.
        assert_eq!(
            BeadState::from_linear_type("canceled", "Cancelled"),
            BeadState::Done
        );
        assert_eq!(BeadState::from("dispatched"), BeadState::Dispatched);
        assert_eq!(BeadState::from("verifying"), BeadState::Verifying);
        assert_eq!(BeadState::from("done"), BeadState::Done);
        assert_eq!(BeadState::from("closed"), BeadState::Done);
        assert_eq!(BeadState::from("rejected"), BeadState::Rejected);
        assert_eq!(BeadState::from("blocked"), BeadState::Blocked);
        assert_eq!(BeadState::from("stale"), BeadState::Stale);
        assert_eq!(BeadState::from("in_progress"), BeadState::Dispatched);
        assert_eq!(BeadState::from("garbage"), BeadState::Open);
    }

    #[test]
    fn state_display_roundtrip() {
        let states = [
            BeadState::Backlog,
            BeadState::Open,
            BeadState::Queued,
            BeadState::Dispatched,
            BeadState::Verifying,
            BeadState::PrOpen,
            BeadState::Done,
            BeadState::Rejected,
            BeadState::Blocked,
            BeadState::Stale,
        ];
        for state in states {
            let s = state.to_string();
            assert_eq!(BeadState::from(s.as_str()), state);
        }
    }

    #[test]
    fn valid_transitions() {
        assert!(BeadState::Backlog.can_transition_to(BeadState::Open));
        assert!(!BeadState::Backlog.can_transition_to(BeadState::Done));

        assert!(BeadState::Open.can_transition_to(BeadState::Queued));
        assert!(!BeadState::Open.can_transition_to(BeadState::Done));

        assert!(BeadState::Queued.can_transition_to(BeadState::Dispatched));
        assert!(!BeadState::Queued.can_transition_to(BeadState::Open));

        assert!(BeadState::Dispatched.can_transition_to(BeadState::Verifying));
        assert!(!BeadState::Dispatched.can_transition_to(BeadState::Done));

        assert!(BeadState::Verifying.can_transition_to(BeadState::Done));
        assert!(BeadState::Verifying.can_transition_to(BeadState::Rejected));
        assert!(BeadState::Verifying.can_transition_to(BeadState::Blocked));

        assert!(BeadState::Rejected.can_transition_to(BeadState::Open));
        assert!(!BeadState::Rejected.can_transition_to(BeadState::Done));

        assert!(BeadState::Done.is_terminal());
    }

    #[test]
    fn to_linear_type_mapping() {
        assert_eq!(BeadState::Backlog.to_linear_type(), ("backlog", "Backlog"));
        assert_eq!(BeadState::Open.to_linear_type(), ("unstarted", "Todo"));
        assert_eq!(BeadState::Queued.to_linear_type(), ("unstarted", "Todo"));
        assert_eq!(
            BeadState::Dispatched.to_linear_type(),
            ("started", "In Progress")
        );
        assert_eq!(
            BeadState::Verifying.to_linear_type(),
            ("started", "In Review")
        );
        assert_eq!(BeadState::Done.to_linear_type(), ("completed", "Done"));
        assert_eq!(BeadState::Blocked.to_linear_type(), ("backlog", "Backlog"));
        assert_eq!(BeadState::Rejected.to_linear_type(), ("unstarted", "Todo"));
        assert_eq!(BeadState::Stale.to_linear_type(), ("unstarted", "Todo"));
    }

    #[test]
    fn from_linear_type_mapping() {
        assert_eq!(
            BeadState::from_linear_type("completed", "Done"),
            BeadState::Done
        );
        assert_eq!(
            BeadState::from_linear_type("canceled", "Canceled"),
            BeadState::Done
        );
        assert_eq!(
            BeadState::from_linear_type("started", "In Progress"),
            BeadState::Dispatched
        );
        assert_eq!(
            BeadState::from_linear_type("started", "In Review"),
            BeadState::Verifying
        );
        // Custom name with "review" in it still maps to Verifying
        assert_eq!(
            BeadState::from_linear_type("started", "Code Review"),
            BeadState::Verifying
        );
        assert_eq!(
            BeadState::from_linear_type("unstarted", "Todo"),
            BeadState::Open
        );
        assert_eq!(
            BeadState::from_linear_type("backlog", "Icebox"),
            BeadState::Backlog
        );
        assert_eq!(
            BeadState::from_linear_type("backlog", "Backlog"),
            BeadState::Backlog
        );
    }

    #[test]
    fn generation_changes_with_content() {
        let bead1 = Bead::from_json(
            &json!({
                "id": "x-1", "title": "fix bug", "description": "desc",
                "status": "open", "priority": 1,
                "created_at": "2026-03-12T00:00:00Z",
                "updated_at": "2026-03-12T00:00:00Z"
            }),
            "repo",
        )
        .unwrap();

        let bead2 = Bead::from_json(
            &json!({
                "id": "x-1", "title": "fix bug UPDATED", "description": "desc",
                "status": "open", "priority": 1,
                "created_at": "2026-03-12T00:00:00Z",
                "updated_at": "2026-03-12T00:00:00Z"
            }),
            "repo",
        )
        .unwrap();

        // Same content → same generation
        assert_eq!(bead1.generation(), bead1.generation());
        // Different title → different generation
        assert_ne!(bead1.generation(), bead2.generation());
    }

    #[test]
    fn generation_ignores_status_and_timestamps() {
        let bead1 = Bead::from_json(
            &json!({
                "id": "x-1", "title": "t", "description": "d",
                "status": "open", "priority": 1,
                "created_at": "2026-03-12T00:00:00Z",
                "updated_at": "2026-03-12T00:00:00Z"
            }),
            "repo",
        )
        .unwrap();

        let bead2 = Bead::from_json(
            &json!({
                "id": "x-1", "title": "t", "description": "d",
                "status": "in_progress", "priority": 1,
                "created_at": "2026-03-11T00:00:00Z",
                "updated_at": "2026-03-13T00:00:00Z"
            }),
            "repo",
        )
        .unwrap();

        assert_eq!(bead1.generation(), bead2.generation());
    }

    #[test]
    fn bead_state_accessor() {
        let bead = Bead::from_json(
            &json!({
                "id": "x-1", "title": "t",
                "status": "in_progress", "priority": 1,
                "created_at": "2026-03-12T00:00:00Z",
                "updated_at": "2026-03-12T00:00:00Z"
            }),
            "repo",
        )
        .unwrap();
        assert_eq!(bead.state(), BeadState::Dispatched);
    }

    #[test]
    fn blocked_bead_not_ready() {
        let val = json!({
            "id": "mache-abc",
            "title": "blocked task",
            "status": "open",
            "priority": 1,
            "dependency_count": 2,
            "dependent_count": 0,
            "comment_count": 0,
            "created_at": "2026-03-12T00:00:00Z",
            "updated_at": "2026-03-12T00:00:00Z"
        });

        let bead = Bead::from_json(&val, "mache").unwrap();
        assert!(!bead.is_ready());
    }

    #[test]
    fn bead_pr_fields_default_none() {
        let val = json!({
            "id": "x-1",
            "title": "some task",
            "status": "open",
            "priority": 1,
            "created_at": "2026-03-12T00:00:00Z",
            "updated_at": "2026-03-12T00:00:00Z"
        });

        let bead = Bead::from_json(&val, "repo").unwrap();
        assert!(bead.branch.is_none());
        assert!(bead.pr_url.is_none());
        assert!(bead.jj_change_id.is_none());
    }

    #[test]
    fn requires_files_for_implementation_types() {
        // Implementation types require scopes for overlap detection
        assert!(requires_files("bug"));
        assert!(requires_files("task"));
        assert!(requires_files("feature"));
        assert!(requires_files("chore"));
        assert!(requires_files("review"));
    }

    #[test]
    fn planning_types_skip_file_requirement() {
        // Planning beads decompose into children with scopes
        assert!(!requires_files("epic"));
        assert!(!requires_files("design"));
        assert!(!requires_files("research"));
    }

    #[test]
    fn needs_refinement_empty_description() {
        let val = json!({
            "id": "x-1", "title": "fix something",
            "description": "",
            "status": "open", "priority": 1, "issue_type": "bug",
            "created_at": "2026-03-12T00:00:00Z",
            "updated_at": "2026-03-12T00:00:00Z"
        });
        let bead = Bead::from_json(&val, "repo").unwrap();
        assert!(bead.needs_refinement());
    }

    #[test]
    fn needs_refinement_short_description() {
        let val = json!({
            "id": "x-1", "title": "fix something",
            "description": "fix the bug",
            "status": "open", "priority": 1, "issue_type": "task",
            "created_at": "2026-03-12T00:00:00Z",
            "updated_at": "2026-03-12T00:00:00Z"
        });
        let bead = Bead::from_json(&val, "repo").unwrap();
        assert!(bead.needs_refinement());
    }

    #[test]
    fn needs_refinement_adequate_description() {
        let desc = "WHO: CLI users. WHEN: on every dispatch. BLAST RADIUS: low.";
        let val = json!({
            "id": "x-1", "title": "add refinement gate",
            "description": desc,
            "status": "open", "priority": 1, "issue_type": "feature",
            "created_at": "2026-03-12T00:00:00Z",
            "updated_at": "2026-03-12T00:00:00Z"
        });
        let bead = Bead::from_json(&val, "repo").unwrap();
        assert!(!bead.needs_refinement());
    }

    #[test]
    fn needs_refinement_exempt_types() {
        // Research/design/epic beads are exempt — they ARE the research step
        for issue_type in &["epic", "design", "research", "review"] {
            let val = json!({
                "id": "x-1", "title": "plan something",
                "description": "",
                "status": "open", "priority": 1, "issue_type": issue_type,
                "created_at": "2026-03-12T00:00:00Z",
                "updated_at": "2026-03-12T00:00:00Z"
            });
            let bead = Bead::from_json(&val, "repo").unwrap();
            assert!(!bead.needs_refinement(), "{issue_type} should be exempt");
        }
    }

    // -----------------------------------------------------------------------
    // Bug regression: pr_open must be a recognized state
    // -----------------------------------------------------------------------

    #[test]
    fn pr_open_parses_correctly() {
        // BUG: verify.rs writes "pr_open" status but BeadState::from parses it
        // as Open, which means triage could re-dispatch a bead waiting for PR merge.
        assert_ne!(
            BeadState::from("pr_open"),
            BeadState::Open,
            "pr_open must NOT parse as Open — would cause re-dispatch"
        );
    }

    #[test]
    fn pr_open_display_roundtrips() {
        let state = BeadState::from("pr_open");
        let s = state.to_string();
        assert_eq!(BeadState::from(s.as_str()), state, "pr_open must roundtrip");
    }

    #[test]
    fn pr_open_maps_to_linear_in_review() {
        // PR awaiting merge should show as "In Review" in Linear, not "Todo"
        let state = BeadState::from("pr_open");
        let (linear_type, _) = state.to_linear_type();
        assert_eq!(
            linear_type, "started",
            "pr_open should map to Linear 'started' type, not '{linear_type}'"
        );
    }

    #[test]
    fn pr_open_is_not_ready() {
        // A bead with pr_open status should NOT be eligible for dispatch
        let val = json!({
            "id": "x-1", "title": "waiting for merge",
            "status": "pr_open", "priority": 1,
            "created_at": "2026-03-12T00:00:00Z",
            "updated_at": "2026-03-12T00:00:00Z"
        });
        let bead = Bead::from_json(&val, "repo").unwrap();
        assert!(
            !bead.is_ready(),
            "pr_open bead should NOT be ready for dispatch"
        );
    }

    #[test]
    fn bead_pr_fields_display() {
        let val = json!({
            "id": "x-2",
            "title": "with PR",
            "status": "open",
            "priority": 1,
            "pr_url": "https://github.com/org/repo/pull/42",
            "created_at": "2026-03-12T00:00:00Z",
            "updated_at": "2026-03-12T00:00:00Z"
        });

        let bead = Bead::from_json(&val, "repo").unwrap();
        let display = format!("{bead}");
        assert!(
            display.contains("https://github.com/org/repo/pull/42"),
            "display should include pr_url: {display}"
        );
    }

    // -----------------------------------------------------------------------
    // Architecture review harness — bugs found 2026-04-07
    // -----------------------------------------------------------------------

    /// Finding #3: Verifying → PrOpen was missing from valid_transitions().
    /// PrOpen existed as a state with no incoming edge — a bead could never
    /// legally reach pr_open via the declared LTS.
    #[test]
    fn verifying_can_reach_pr_open() {
        assert!(
            BeadState::Verifying.can_transition_to(BeadState::PrOpen),
            "Verifying must be able to reach PrOpen (pipeline complete, PR created)"
        );
        // PrOpen still leads only to Done
        assert!(BeadState::PrOpen.can_transition_to(BeadState::Done));
        assert!(!BeadState::PrOpen.can_transition_to(BeadState::Open));
    }

    /// Finding #2: can_transition_to was #[allow(dead_code)] and unenforced.
    /// Verify that genuinely invalid transitions are rejected.
    #[test]
    fn invalid_transitions_are_rejected() {
        // Cannot jump from Backlog straight to Done
        assert!(!BeadState::Backlog.can_transition_to(BeadState::Done));
        // Done is terminal — no outgoing edges
        assert!(BeadState::Done.is_terminal());
        assert!(!BeadState::Done.can_transition_to(BeadState::Open));
        // PrOpen cannot go back to Verifying
        assert!(!BeadState::PrOpen.can_transition_to(BeadState::Verifying));
        // Queued cannot skip to Done
        assert!(!BeadState::Queued.can_transition_to(BeadState::Done));
    }

    /// Finding #6: DefaultHasher is non-deterministic across process restarts.
    /// Verify that generation() returns a stable value across repeated calls
    /// (cross-restart stability is guaranteed by SHA-256, not testable here,
    /// but determinism within a run is the minimum bar).
    #[test]
    fn generation_is_deterministic() {
        let make = |title: &str| {
            Bead::from_json(
                &json!({
                    "id": "x-1", "title": title, "description": "desc",
                    "status": "open", "priority": 1,
                    "created_at": "2026-03-12T00:00:00Z",
                    "updated_at": "2026-03-12T00:00:00Z"
                }),
                "repo",
            )
            .unwrap()
        };
        let b = make("fix the bug");
        // Repeated calls must return the same value
        assert_eq!(
            b.generation(),
            b.generation(),
            "generation must be idempotent"
        );
        // Same content → same generation
        assert_eq!(
            make("fix the bug").generation(),
            make("fix the bug").generation()
        );
        // Different content → different generation
        assert_ne!(
            make("fix the bug").generation(),
            make("fix the other bug").generation()
        );
    }

    /// Finding #16: from_json silently defaulted every field except id/title.
    /// A bare {"id":"x","title":"y"} created a dispatchable priority-2 open bead.
    /// Now status and priority are required — missing either must return None.
    #[test]
    fn from_json_requires_status_and_priority() {
        // Missing both → None
        assert!(
            Bead::from_json(&json!({"id": "x", "title": "t"}), "repo").is_none(),
            "missing status and priority must return None"
        );
        // Missing priority → None
        assert!(
            Bead::from_json(&json!({"id": "x", "title": "t", "status": "open"}), "repo").is_none(),
            "missing priority must return None"
        );
        // Missing status → None
        assert!(
            Bead::from_json(&json!({"id": "x", "title": "t", "priority": 1}), "repo").is_none(),
            "missing status must return None"
        );
        // Both present → Ok
        let b = Bead::from_json(
            &json!({"id": "x", "title": "t", "status": "open", "priority": 2}),
            "repo",
        );
        assert!(b.is_some(), "id+title+status+priority should be sufficient");
    }
}
