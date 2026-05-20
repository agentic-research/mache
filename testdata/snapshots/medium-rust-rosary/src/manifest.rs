//! Dispatch manifest — structured record of agent work.
//!
//! `.rsry-dispatch.json` written to workspace after agent completion.
//! Backend-agnostic: works with any orchestrator (Rust reconciler,
//! Elixir conductor) and any execution backend (local, sprites, etc.).
//!
//! See `docs/design/dispatch-manifest-schema.md` for the full specification.

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use std::path::Path;

/// Schema version. Bump on breaking changes only.
pub const SCHEMA_VERSION: &str = "1";

/// The dispatch manifest — everything about what happened during a dispatch.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Manifest {
    pub schema_version: String,
    pub identity: Identity,
    pub session: Session,
    pub work: Work,
    pub quality: Quality,
    pub cost: Cost,
    pub vcs: Vcs,
    pub outcome: Outcome,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Identity {
    pub dispatch_id: String,
    pub bead_id: String,
    pub repo: String,
    pub agent: String,
    pub provider: String,
    /// Model name (from stream-json init event). Null until captured.
    pub model: Option<String>,
    pub pipeline_phase: u32,
    pub issue_type: String,
    pub permission_profile: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Session {
    /// Claude Code session ID (enables --resume).
    pub session_id: Option<String>,
    pub workspace_path: Option<String>,
    pub work_dir: String,
    pub repo_path: String,
    pub vcs_kind: String,
    pub started_at: DateTime<Utc>,
    pub completed_at: Option<DateTime<Utc>>,
    pub duration_ms: Option<u64>,
    pub pid: Option<u32>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CommitInfo {
    pub sha: String,
    pub message: String,
    pub author: String,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Work {
    pub commits: Vec<CommitInfo>,
    pub files_changed: Vec<String>,
    pub lines_added: u64,
    pub lines_removed: u64,
    pub diff_stat: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TierResult {
    pub name: String,
    pub result: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub detail: Option<String>,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Quality {
    pub verification_passed: bool,
    pub highest_passing_tier: Option<usize>,
    pub tiers: Vec<TierResult>,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Cost {
    pub total_cost_usd: Option<f64>,
    pub input_tokens: Option<u64>,
    pub output_tokens: Option<u64>,
    pub cache_read_tokens: Option<u64>,
    pub cache_write_tokens: Option<u64>,
    pub num_turns: Option<u32>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Vcs {
    pub jj_change_id: Option<String>,
    pub git_branch: Option<String>,
    pub bookmark: Option<String>,
    pub base_commit: Option<String>,
    pub head_commit: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Outcome {
    pub success: bool,
    pub bead_closed: bool,
    pub stop_reason: Option<String>,
    pub agent_closed_via_mcp: bool,
    pub error: Option<String>,
    pub retries: u32,
    pub deadlettered: bool,
}

impl Manifest {
    /// Create a manifest pre-populated with spawn-time fields.
    /// Remaining fields are filled in after agent completion.
    #[allow(clippy::too_many_arguments)]
    pub fn at_spawn(
        dispatch_id: &str,
        bead_id: &str,
        repo: &str,
        agent: &str,
        provider: &str,
        issue_type: &str,
        permission_profile: &str,
        pipeline_phase: u32,
        work_dir: &str,
        repo_path: &str,
        vcs_kind: &str,
        pid: Option<u32>,
    ) -> Self {
        let git_branch = format!("fix/{bead_id}");
        Manifest {
            schema_version: SCHEMA_VERSION.to_string(),
            identity: Identity {
                dispatch_id: dispatch_id.to_string(),
                bead_id: bead_id.to_string(),
                repo: repo.to_string(),
                agent: agent.to_string(),
                provider: provider.to_string(),
                model: None,
                pipeline_phase,
                issue_type: issue_type.to_string(),
                permission_profile: permission_profile.to_string(),
            },
            session: Session {
                session_id: None,
                workspace_path: Some(work_dir.to_string()),
                work_dir: work_dir.to_string(),
                repo_path: repo_path.to_string(),
                vcs_kind: vcs_kind.to_string(),
                started_at: Utc::now(),
                completed_at: None,
                duration_ms: None,
                pid,
            },
            work: Work::default(),
            quality: Quality::default(),
            cost: Cost::default(),
            vcs: Vcs {
                jj_change_id: None,
                git_branch: Some(git_branch),
                bookmark: None,
                base_commit: None,
                head_commit: None,
            },
            outcome: Outcome {
                success: false,
                bead_closed: false,
                stop_reason: None,
                agent_closed_via_mcp: false,
                error: None,
                retries: 0,
                deadlettered: false,
            },
        }
    }

    /// Fill in completion-time fields from the agent exit.
    pub fn complete(&mut self, success: bool, stop_reason: Option<&str>) {
        self.session.completed_at = Some(Utc::now());
        self.session.duration_ms = Some(
            (Utc::now() - self.session.started_at)
                .num_milliseconds()
                .unsigned_abs(),
        );
        self.outcome.success = success;
        self.outcome.stop_reason = stop_reason.map(|s| s.to_string());
    }

    /// Write the manifest to the workspace directory.
    pub fn write_to(&self, workspace_dir: &Path) -> anyhow::Result<()> {
        let path = workspace_dir.join(".rsry-dispatch.json");
        let content = serde_json::to_string_pretty(self)?;
        std::fs::write(&path, content)?;
        eprintln!("[manifest] wrote {}", path.display());
        Ok(())
    }

    /// Read a manifest from a workspace directory.
    pub fn read_from(workspace_dir: &Path) -> anyhow::Result<Self> {
        let path = workspace_dir.join(".rsry-dispatch.json");
        let content = std::fs::read_to_string(&path)?;
        Ok(serde_json::from_str(&content)?)
    }
}

/// Parse cost fields from a Claude Code stream-json result line.
impl Cost {
    pub fn from_stream_json_result(result: &serde_json::Value) -> Self {
        let usage = &result["usage"];
        Cost {
            total_cost_usd: result["total_cost_usd"].as_f64(),
            input_tokens: usage["input_tokens"].as_u64(),
            output_tokens: usage["output_tokens"].as_u64(),
            cache_read_tokens: usage["cache_read_input_tokens"].as_u64(),
            cache_write_tokens: usage["cache_creation_input_tokens"].as_u64(),
            num_turns: result["num_turns"].as_u64().map(|n| n as u32),
        }
    }
}

/// Populate work fields from git state in a workspace.
impl Work {
    pub fn from_git(workspace_dir: &Path, base_commit: Option<&str>) -> Self {
        let base = base_commit.unwrap_or("HEAD~1");

        let diff_stat = std::process::Command::new("git")
            .args(["diff", "--stat", &format!("{base}..HEAD")])
            .current_dir(workspace_dir)
            .output()
            .ok()
            .map(|o| String::from_utf8_lossy(&o.stdout).trim().to_string())
            .filter(|s| !s.is_empty());

        let files = std::process::Command::new("git")
            .args(["diff", "--name-only", &format!("{base}..HEAD")])
            .current_dir(workspace_dir)
            .output()
            .ok()
            .map(|o| {
                String::from_utf8_lossy(&o.stdout)
                    .lines()
                    .filter(|l| !l.is_empty())
                    .map(|l| l.to_string())
                    .collect::<Vec<_>>()
            })
            .unwrap_or_default();

        let numstat = std::process::Command::new("git")
            .args(["diff", "--numstat", &format!("{base}..HEAD")])
            .current_dir(workspace_dir)
            .output()
            .ok()
            .map(|o| String::from_utf8_lossy(&o.stdout).to_string())
            .unwrap_or_default();

        let (added, removed) = numstat
            .lines()
            .filter_map(|line| {
                let parts: Vec<&str> = line.split_whitespace().collect();
                if parts.len() >= 2 {
                    Some((
                        parts[0].parse::<u64>().unwrap_or(0),
                        parts[1].parse::<u64>().unwrap_or(0),
                    ))
                } else {
                    None
                }
            })
            .fold((0u64, 0u64), |(a, r), (da, dr)| (a + da, r + dr));

        Work {
            commits: Vec::new(), // Populated separately
            files_changed: files,
            lines_added: added,
            lines_removed: removed,
            diff_stat,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn manifest_roundtrip() {
        let m = Manifest::at_spawn(
            "d-test-001",
            "rosary-abc",
            "rosary",
            "dev-agent",
            "claude",
            "bug",
            "implement",
            0,
            "/tmp/ws",
            "/tmp/repo",
            "git",
            Some(42),
        );

        let json = serde_json::to_string_pretty(&m).unwrap();
        let parsed: Manifest = serde_json::from_str(&json).unwrap();

        assert_eq!(parsed.schema_version, "1");
        assert_eq!(parsed.identity.bead_id, "rosary-abc");
        assert_eq!(parsed.identity.provider, "claude");
        assert_eq!(parsed.session.pid, Some(42));
        assert_eq!(parsed.vcs.git_branch, Some("fix/rosary-abc".to_string()));
        assert!(!parsed.outcome.success);
    }

    #[test]
    fn manifest_complete() {
        let mut m = Manifest::at_spawn(
            "d-test-002",
            "rosary-xyz",
            "rosary",
            "staging-agent",
            "gemini",
            "review",
            "read_only",
            1,
            "/tmp/ws",
            "/tmp/repo",
            "jj",
            None,
        );

        m.complete(true, Some("end_turn"));

        assert!(m.outcome.success);
        assert_eq!(m.outcome.stop_reason.as_deref(), Some("end_turn"));
        assert!(m.session.completed_at.is_some());
        assert!(m.session.duration_ms.is_some());
    }

    #[test]
    fn cost_from_stream_json() {
        let result = serde_json::json!({
            "total_cost_usd": 0.042,
            "num_turns": 7,
            "usage": {
                "input_tokens": 18500,
                "output_tokens": 3200,
                "cache_read_input_tokens": 12000,
                "cache_creation_input_tokens": 5000
            }
        });

        let cost = Cost::from_stream_json_result(&result);
        assert_eq!(cost.total_cost_usd, Some(0.042));
        assert_eq!(cost.input_tokens, Some(18500));
        assert_eq!(cost.output_tokens, Some(3200));
        assert_eq!(cost.num_turns, Some(7));
    }

    #[test]
    fn manifest_write_read() {
        let tmp = tempfile::TempDir::new().unwrap();
        let m = Manifest::at_spawn(
            "d-test-003",
            "rosary-io",
            "rosary",
            "dev-agent",
            "claude",
            "task",
            "implement",
            0,
            &tmp.path().display().to_string(),
            "/tmp/repo",
            "git",
            Some(99),
        );

        m.write_to(tmp.path()).unwrap();
        let read = Manifest::read_from(tmp.path()).unwrap();
        assert_eq!(read.identity.dispatch_id, "d-test-003");
    }
}
