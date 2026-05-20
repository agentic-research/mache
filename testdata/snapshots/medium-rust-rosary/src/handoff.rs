//! Structured handoff between pipeline phases.
//!
//! Written by the orchestrator (not the agent) after each phase completes.
//! The next agent's prompt references this file for context about what
//! the previous phase did, what to review, and where to look.
//!
//! ## Handoff struct
//!
//! Each [`Handoff`] captures one phase's output:
//! - `phase`, `from_agent`, `to_agent` — pipeline position
//! - `bead_id` — the work item being processed
//! - `provider` — execution backend (e.g. "claude", "gemini")
//! - `thread_id` — optional thread from [`HierarchyStore`], giving agents
//!   context about their position in a larger progression of work
//! - `summary`, `files_changed`, `lines_changed` — what changed
//! - `review_hints` — auto-generated focus areas for the reviewing agent
//! - `artifacts` — paths to manifest, log, and previous handoff
//! - `verdict` — review result (filled by staging/prod agents)
//!
//! ## Key operations
//!
//! - [`Handoff::read_chain`] reads the full sequence of handoffs from a workspace
//! - [`Handoff::format_for_prompt`] renders the chain as markdown for agent prompts
//! - [`Handoff::chain_hash`] produces a tamper-evident SHA-256 hash chain
//!
//! ## File naming
//!
//! Handoffs are stored as `.rsry-handoff-{phase}.json` in the workspace directory.
//!
//! Backend-agnostic: works with any orchestrator, provider, or execution backend.

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use std::path::{Path, PathBuf};

/// A single tool permission request recorded during a pipeline phase.
/// Includes both approved and rejected requests — a rejected entry means
/// the agent attempted the tool but the permission profile denied it.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ToolCallRecord {
    pub tool_name: String,
    pub approved: bool,
    pub timestamp: chrono::DateTime<chrono::Utc>,
}

/// A structured handoff between pipeline phases.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Handoff {
    pub schema_version: String,
    pub phase: u32,
    pub from_agent: String,
    pub to_agent: Option<String>,
    pub bead_id: String,
    pub provider: String,
    /// Thread this bead belongs to (from HierarchyStore). Gives agents
    /// context about their position in a larger progression of work.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub thread_id: Option<String>,

    /// One-line summary of what this phase accomplished.
    pub summary: String,
    /// Files modified in this phase.
    pub files_changed: Vec<String>,
    /// Lines added/removed.
    pub lines_changed: LinesChanged,

    /// Hints for the reviewing agent — what to focus on.
    pub review_hints: Vec<String>,

    /// Paths to related artifacts in the workspace.
    pub artifacts: Artifacts,

    /// Review verdict (filled by staging/prod agents, null for dev).
    pub verdict: Option<Verdict>,

    /// Tool calls made by the agent during this phase (approved and rejected).
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub tools_used: Vec<ToolCallRecord>,

    /// Content hash of the previous phase's handoff (hex-encoded SHA-256).
    /// Links the chain by CONTENT, not file path — an attacker cannot replace
    /// the previous handoff file without invalidating this hash.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub previous_chain_hash: Option<String>,

    /// Git commit SHAs produced during this phase.
    /// Binds the provenance chain to the actual committed code — two handoffs
    /// with identical summaries but different commits will have different chain hashes.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub commit_shas: Vec<String>,

    pub timestamp: DateTime<Utc>,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct LinesChanged {
    pub added: u64,
    pub removed: u64,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Artifacts {
    /// Path to .rsry-dispatch.json (SBOM manifest).
    pub manifest: Option<String>,
    /// Path to .rsry-stream.jsonl (agent output log).
    pub log: Option<String>,
    /// Path to previous phase's handoff (handoff chain).
    pub previous_handoff: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Verdict {
    /// "approve", "request_changes", or "reject"
    pub decision: String,
    /// Specific concerns found during review.
    pub concerns: Vec<String>,
    /// Suggestions for improvement (non-blocking).
    pub suggestions: Vec<String>,
}

impl Handoff {
    /// Content hash of this handoff, forming a hash chain with previous phases.
    ///
    /// Covers: phase, from_agent, bead_id, summary, files_changed, previous_chain_hash.
    /// The chain property: each handoff's hash includes the CONTENT HASH of the
    /// previous handoff (not its file path), making the pipeline tamper-evident.
    /// Replacing a previous handoff file changes its content hash, which invalidates
    /// every subsequent handoff in the chain.
    ///
    /// **Invariant**: For `phase > 0`, `previous_chain_hash` MUST be `Some` for
    /// the chain to be tamper-evident. Production callsites enforce this by aborting
    /// handoff creation when the previous handoff can't be read. Test callsites may
    /// pass `None` for convenience.
    ///
    /// Does NOT include timestamp (non-deterministic) or verdict (may be added later).
    pub fn chain_hash(&self) -> [u8; 32] {
        use sha2::{Digest, Sha256};
        let mut hasher = Sha256::new();
        hasher.update(self.phase.to_le_bytes());
        hasher.update(self.from_agent.as_bytes());
        hasher.update(b"\0");
        hasher.update(self.bead_id.as_bytes());
        hasher.update(b"\0");
        hasher.update(self.summary.as_bytes());
        hasher.update(b"\0");
        for f in &self.files_changed {
            hasher.update(f.as_bytes());
            hasher.update(b"\0");
        }
        // Bind to the actual code committed — prevents two handoffs with
        // identical summaries but different code from sharing a chain hash.
        for sha in &self.commit_shas {
            hasher.update(sha.as_bytes());
            hasher.update(b"\0");
        }
        // Chain: include CONTENT HASH of previous phase (not file path).
        // This is the critical security property — replacing a previous handoff
        // file without knowing its hash breaks the chain.
        if let Some(ref prev_hash) = self.previous_chain_hash {
            hasher.update(prev_hash.as_bytes());
        }
        hasher.finalize().into()
    }

    /// Hex-encoded chain hash (for display, storage, and chain references).
    pub fn chain_hash_hex(&self) -> String {
        hex::encode(self.chain_hash())
    }

    /// Create a handoff from orchestrator state after a phase completes.
    ///
    /// `summary` is extracted from the agent's commit messages or final
    /// output. `review_hints` are derived from changed files and bead
    /// description keywords.
    /// Create a handoff, optionally linking to the previous phase's content hash.
    ///
    /// `previous` is the previous phase's handoff (if any). Its `chain_hash_hex()`
    /// is stored in `previous_chain_hash` to create a content-linked chain.
    pub fn new(
        phase: u32,
        from_agent: &str,
        to_agent: Option<&str>,
        bead_id: &str,
        provider: &str,
        work: &crate::manifest::Work,
        previous: Option<&Handoff>,
    ) -> Self {
        // Generate review hints from file patterns
        let mut hints = Vec::new();
        for f in &work.files_changed {
            if f.contains("test") {
                hints.push(format!("Test changes in {f} — verify coverage"));
            }
            if f.ends_with("reconcile.rs") || f.ends_with("dispatch.rs") {
                hints.push(format!(
                    "Core dispatch path changed: {f} — check concurrency"
                ));
            }
        }

        let summary = work
            .commits
            .first()
            .map(|c| c.message.clone())
            .unwrap_or_else(|| {
                format!(
                    "Changed {} files (+{}/-{})",
                    work.files_changed.len(),
                    work.lines_added,
                    work.lines_removed
                )
            });

        Handoff {
            schema_version: "2".to_string(),
            phase,
            from_agent: from_agent.to_string(),
            to_agent: to_agent.map(|s| s.to_string()),
            bead_id: bead_id.to_string(),
            provider: provider.to_string(),
            thread_id: None, // Set by reconciler when hierarchy is available
            summary,
            files_changed: work.files_changed.clone(),
            lines_changed: LinesChanged {
                added: work.lines_added,
                removed: work.lines_removed,
            },
            review_hints: hints,
            artifacts: Artifacts {
                manifest: Some(".rsry-dispatch.json".to_string()),
                log: Some(format!(".rsry-stream-{phase}.jsonl")),
                previous_handoff: if phase > 0 {
                    Some(format!(".rsry-handoff-{}.json", phase - 1))
                } else {
                    None
                },
            },
            tools_used: Vec::new(),
            verdict: None,
            previous_chain_hash: previous.map(|p| p.chain_hash_hex()),
            commit_shas: work.commits.iter().map(|c| c.sha.clone()).collect(),
            timestamp: Utc::now(),
        }
    }

    /// Write the handoff to the workspace directory.
    pub fn write_to(&self, workspace_dir: &Path) -> anyhow::Result<PathBuf> {
        let filename = format!(".rsry-handoff-{}.json", self.phase);
        let path = workspace_dir.join(&filename);
        let content = serde_json::to_string_pretty(self)?;
        std::fs::write(&path, &content)?;
        eprintln!("[handoff] wrote {}", path.display());
        Ok(path)
    }

    /// Read a handoff from a workspace directory.
    pub fn read_from(workspace_dir: &Path, phase: u32) -> anyhow::Result<Self> {
        let filename = format!(".rsry-handoff-{phase}.json");
        let path = workspace_dir.join(filename);
        let content = std::fs::read_to_string(&path)?;
        Ok(serde_json::from_str(&content)?)
    }

    /// Read all handoffs in a workspace (the full chain), verifying hash integrity.
    ///
    /// Reads phases 0, 1, 2, … until the first missing file. For each phase > 0,
    /// verifies that `previous_chain_hash` matches the computed hash of the preceding
    /// handoff. Returns an empty vec and logs a warning on hash mismatch — the
    /// tamper-evident property is only useful if we actually check it.
    pub fn read_chain(workspace_dir: &Path) -> Vec<Self> {
        let mut chain: Vec<Self> = Vec::new();
        for phase in 0.. {
            let h = match Self::read_from(workspace_dir, phase) {
                Ok(h) => h,
                Err(_) => break, // No file for this phase — chain is complete
            };
            // Verify the hash link for all phases after the first
            if phase > 0
                && let Some(prev) = chain.last()
            {
                let expected = prev.chain_hash_hex();
                match &h.previous_chain_hash {
                    Some(actual) if actual == &expected => {} // chain intact
                    Some(actual) => {
                        eprintln!(
                            "[handoff] chain integrity FAIL at phase {phase}: \
                             expected previous_chain_hash={expected}, got={actual}. \
                             A handoff file may have been tampered with."
                        );
                        return chain; // Truncate — don't process past a broken link
                    }
                    None => {
                        eprintln!(
                            "[handoff] chain integrity WARNING at phase {phase}: \
                             previous_chain_hash is missing (expected link to phase {})",
                            phase - 1
                        );
                        // Allow but warn — older handoffs may predate hash chaining
                    }
                }
            }
            chain.push(h);
        }
        chain
    }

    /// Format the handoff chain as context for the next agent's prompt.
    pub fn format_for_prompt(chain: &[Self]) -> String {
        if chain.is_empty() {
            return String::new();
        }

        let mut out = String::from("\n## Previous Phase Context\n\n");
        for h in chain {
            out.push_str(&format!(
                "### Phase {} ({} via {})\n",
                h.phase, h.from_agent, h.provider
            ));
            if let Some(ref tid) = h.thread_id {
                out.push_str(&format!("Thread: {tid}\n"));
            }
            out.push_str(&format!("Summary: {}\n", h.summary));
            if !h.files_changed.is_empty() {
                out.push_str(&format!("Files: {}\n", h.files_changed.join(", ")));
            }
            if !h.review_hints.is_empty() {
                out.push_str("Review hints:\n");
                for hint in &h.review_hints {
                    out.push_str(&format!("- {hint}\n"));
                }
            }
            if !h.tools_used.is_empty() {
                let approved: Vec<&str> = h
                    .tools_used
                    .iter()
                    .filter(|t| t.approved)
                    .map(|t| t.tool_name.as_str())
                    .collect();
                let rejected: Vec<&str> = h
                    .tools_used
                    .iter()
                    .filter(|t| !t.approved)
                    .map(|t| t.tool_name.as_str())
                    .collect();
                if !approved.is_empty() {
                    out.push_str(&format!("Tools used: {}\n", approved.join(", ")));
                }
                if !rejected.is_empty() {
                    out.push_str(&format!("Tools denied: {}\n", rejected.join(", ")));
                }
            }
            if let Some(ref v) = h.verdict {
                out.push_str(&format!("Verdict: {}\n", v.decision));
                for c in &v.concerns {
                    out.push_str(&format!("  Concern: {c}\n"));
                }
            }
            out.push('\n');
        }
        out.push_str("Handoff files are in your working directory. Use mache MCP tools to structurally review the changes.\n");
        out
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::manifest::{CommitInfo, Work};

    fn sample_work() -> Work {
        Work {
            commits: vec![CommitInfo {
                sha: "abc123".to_string(),
                message: "fix(reconcile): handle timeout=0 edge case".to_string(),
                author: "dev-agent".to_string(),
            }],
            files_changed: vec![
                "src/reconcile.rs".to_string(),
                "src/reconcile_test.rs".to_string(),
            ],
            lines_added: 47,
            lines_removed: 12,
            diff_stat: Some("2 files changed".to_string()),
        }
    }

    #[test]
    fn handoff_roundtrip() {
        let work = sample_work();
        let h = Handoff::new(
            0,
            "dev-agent",
            Some("staging-agent"),
            "rosary-abc",
            "claude",
            &work,
            None,
        );

        let json = serde_json::to_string_pretty(&h).unwrap();
        let parsed: Handoff = serde_json::from_str(&json).unwrap();

        assert_eq!(parsed.phase, 0);
        assert_eq!(parsed.from_agent, "dev-agent");
        assert_eq!(parsed.to_agent.as_deref(), Some("staging-agent"));
        assert_eq!(parsed.files_changed.len(), 2);
        assert!(parsed.summary.contains("timeout=0"));
        assert!(parsed.artifacts.previous_handoff.is_none()); // phase 0
    }

    #[test]
    fn handoff_chain_links() {
        let work = sample_work();
        let h1 = Handoff::new(
            1,
            "staging-agent",
            Some("prod-agent"),
            "rosary-abc",
            "gemini",
            &work,
            None,
        );

        assert_eq!(
            h1.artifacts.previous_handoff.as_deref(),
            Some(".rsry-handoff-0.json")
        );
    }

    #[test]
    fn handoff_write_read() {
        let tmp = tempfile::TempDir::new().unwrap();
        let work = sample_work();
        let h = Handoff::new(0, "dev-agent", None, "rosary-test", "claude", &work, None);

        h.write_to(tmp.path()).unwrap();
        let read = Handoff::read_from(tmp.path(), 0).unwrap();
        assert_eq!(read.bead_id, "rosary-test");
    }

    #[test]
    fn handoff_chain_read() {
        let tmp = tempfile::TempDir::new().unwrap();
        let work = sample_work();

        let h0 = Handoff::new(
            0,
            "dev-agent",
            Some("staging-agent"),
            "rosary-x",
            "claude",
            &work,
            None,
        );
        h0.write_to(tmp.path()).unwrap();

        let h1 = Handoff::new(1, "staging-agent", None, "rosary-x", "gemini", &work, None);
        h1.write_to(tmp.path()).unwrap();

        let chain = Handoff::read_chain(tmp.path());
        assert_eq!(chain.len(), 2);
        assert_eq!(chain[0].from_agent, "dev-agent");
        assert_eq!(chain[1].from_agent, "staging-agent");
    }

    #[test]
    fn format_for_prompt_includes_context() {
        let work = sample_work();
        let h = Handoff::new(
            0,
            "dev-agent",
            Some("staging-agent"),
            "rosary-abc",
            "claude",
            &work,
            None,
        );
        let prompt = Handoff::format_for_prompt(&[h]);

        assert!(prompt.contains("Phase 0"));
        assert!(prompt.contains("dev-agent"));
        assert!(prompt.contains("timeout=0"));
        assert!(prompt.contains("mache MCP"));
    }

    // --- chain_hash tests ---

    #[test]
    fn chain_hash_deterministic() {
        let work = sample_work();
        let h = Handoff::new(0, "dev-agent", None, "rsry-abc", "claude", &work, None);
        assert_eq!(h.chain_hash(), h.chain_hash());
    }

    #[test]
    fn chain_hash_changes_with_phase() {
        let work = sample_work();
        let h0 = Handoff::new(0, "dev-agent", None, "rsry-abc", "claude", &work, None);
        let h1 = Handoff::new(1, "dev-agent", None, "rsry-abc", "claude", &work, None);
        assert_ne!(h0.chain_hash(), h1.chain_hash());
    }

    #[test]
    fn chain_hash_content_linked() {
        let work = sample_work();
        let h0 = Handoff::new(
            0,
            "dev-agent",
            Some("staging-agent"),
            "rsry-abc",
            "claude",
            &work,
            None,
        );

        // h1 links to h0 by CONTENT HASH, not file path
        let h1 = Handoff::new(
            1,
            "staging-agent",
            None,
            "rsry-abc",
            "claude",
            &work,
            Some(&h0),
        );

        // The chain hash of h1 includes h0's content hash
        assert!(h1.previous_chain_hash.is_some());
        assert_eq!(
            h1.previous_chain_hash.as_deref(),
            Some(h0.chain_hash_hex().as_str())
        );

        // Modifying h0's summary changes its hash, which would invalidate h1's chain
        let mut h0_tampered = h0.clone();
        h0_tampered.summary = "tampered summary".to_string();
        assert_ne!(h0.chain_hash_hex(), h0_tampered.chain_hash_hex());

        // h1 still references the ORIGINAL h0's hash — tampered h0 won't match
        assert_ne!(
            h1.previous_chain_hash.as_deref(),
            Some(h0_tampered.chain_hash_hex().as_str()),
            "tampering with h0 must break the chain link from h1"
        );
    }

    #[test]
    fn chain_hash_none_for_phase_zero() {
        let work = sample_work();
        let h0 = Handoff::new(0, "dev-agent", None, "rsry-abc", "claude", &work, None);
        assert!(
            h0.previous_chain_hash.is_none(),
            "phase 0 has no previous hash"
        );
    }

    #[test]
    fn chain_hash_changes_with_commit_sha() {
        let work_a = Work {
            commits: vec![CommitInfo {
                sha: "aaaaaa".to_string(),
                message: "same message".to_string(),
                author: "dev-agent".to_string(),
            }],
            files_changed: vec!["src/foo.rs".to_string()],
            lines_added: 10,
            lines_removed: 5,
            diff_stat: None,
        };
        let work_b = Work {
            commits: vec![CommitInfo {
                sha: "bbbbbb".to_string(),
                message: "same message".to_string(),
                author: "dev-agent".to_string(),
            }],
            files_changed: vec!["src/foo.rs".to_string()],
            lines_added: 10,
            lines_removed: 5,
            diff_stat: None,
        };
        let ha = Handoff::new(0, "dev-agent", None, "rsry-x", "claude", &work_a, None);
        let hb = Handoff::new(0, "dev-agent", None, "rsry-x", "claude", &work_b, None);
        // Same summary, files, phase — only commit SHA differs
        assert_eq!(ha.summary, hb.summary);
        assert_ne!(
            ha.chain_hash(),
            hb.chain_hash(),
            "different commit SHAs must produce different chain hashes"
        );
    }

    #[test]
    fn chain_hash_hex_is_64_chars() {
        let work = sample_work();
        let h = Handoff::new(0, "dev-agent", None, "rsry-abc", "claude", &work, None);
        assert_eq!(h.chain_hash_hex().len(), 64);
    }

    #[test]
    fn review_hints_generated() {
        let work = Work {
            commits: vec![],
            files_changed: vec![
                "src/dispatch.rs".to_string(),
                "src/dispatch_test.rs".to_string(),
            ],
            lines_added: 10,
            lines_removed: 5,
            diff_stat: None,
        };
        let h = Handoff::new(0, "dev-agent", None, "rosary-abc", "claude", &work, None);

        assert!(h.review_hints.iter().any(|r| r.contains("concurrency")));
        assert!(h.review_hints.iter().any(|r| r.contains("coverage")));
    }

    // -----------------------------------------------------------------------
    // Architecture review harness — bugs found 2026-04-07
    // -----------------------------------------------------------------------

    /// Finding #4: read_chain was hardcoded to 10 phases via (0..10).
    /// A chain longer than 10 was silently truncated. The map_while already
    /// terminates on missing files, so (0..) is correct.
    #[test]
    fn read_chain_not_limited_to_ten_phases() {
        let tmp = tempfile::TempDir::new().unwrap();
        let work = sample_work();

        // Write 12 handoffs (would be silently truncated by the old (0..10) bound)
        let mut prev: Option<Handoff> = None;
        for phase in 0u32..12 {
            let h = Handoff::new(
                phase,
                "dev-agent",
                None,
                "rsry-x",
                "claude",
                &work,
                prev.as_ref(),
            );
            h.write_to(tmp.path()).unwrap();
            prev = Some(h);
        }

        let chain = Handoff::read_chain(tmp.path());
        assert_eq!(
            chain.len(),
            12,
            "read_chain must not stop at 10 — got {}",
            chain.len()
        );
    }

    /// Finding #5: chain hashes were computed and stored but never verified on read.
    /// A replaced intermediate handoff file went undetected.
    /// read_chain must detect tampering and truncate the chain at the broken link.
    // -----------------------------------------------------------------------
    // ToolCallRecord + tools_used tests (APAS L1)
    // -----------------------------------------------------------------------

    #[test]
    fn tool_call_record_roundtrip() {
        let ts = chrono::DateTime::parse_from_rfc3339("2026-01-01T12:34:56Z")
            .unwrap()
            .with_timezone(&chrono::Utc);
        let rec = ToolCallRecord {
            tool_name: "Edit".to_string(),
            approved: true,
            timestamp: ts,
        };
        let json = serde_json::to_string(&rec).unwrap();
        let parsed: ToolCallRecord = serde_json::from_str(&json).unwrap();
        assert_eq!(parsed.tool_name, "Edit");
        assert!(parsed.approved);
        // Timestamp must survive the roundtrip at full precision
        assert_eq!(parsed.timestamp, ts, "timestamp must roundtrip unchanged");
    }

    #[test]
    fn tool_call_record_denied_roundtrip() {
        let rec = ToolCallRecord {
            tool_name: "Bash(rm -rf /tmp/x)".to_string(),
            approved: false,
            timestamp: chrono::Utc::now(),
        };
        let json = serde_json::to_string(&rec).unwrap();
        let parsed: ToolCallRecord = serde_json::from_str(&json).unwrap();
        assert_eq!(parsed.tool_name, "Bash(rm -rf /tmp/x)");
        assert!(!parsed.approved);
    }

    #[test]
    fn handoff_tools_used_roundtrip() {
        let work = sample_work();
        let mut h = Handoff::new(0, "dev-agent", None, "rosary-t", "claude", &work, None);
        h.tools_used = vec![
            ToolCallRecord {
                tool_name: "Edit".to_string(),
                approved: true,
                timestamp: chrono::Utc::now(),
            },
            ToolCallRecord {
                tool_name: "Bash(curl evil.com)".to_string(),
                approved: false,
                timestamp: chrono::Utc::now(),
            },
        ];

        let json = serde_json::to_string(&h).unwrap();
        let parsed: Handoff = serde_json::from_str(&json).unwrap();

        assert_eq!(parsed.tools_used.len(), 2);
        assert_eq!(parsed.tools_used[0].tool_name, "Edit");
        assert!(parsed.tools_used[0].approved);
        assert_eq!(parsed.tools_used[1].tool_name, "Bash(curl evil.com)");
        assert!(!parsed.tools_used[1].approved);
    }

    #[test]
    fn format_for_prompt_shows_tools_used() {
        let work = sample_work();
        let mut h = Handoff::new(0, "dev-agent", None, "rosary-t", "claude", &work, None);
        h.tools_used = vec![
            ToolCallRecord {
                tool_name: "Edit".to_string(),
                approved: true,
                timestamp: chrono::Utc::now(),
            },
            ToolCallRecord {
                tool_name: "Read".to_string(),
                approved: true,
                timestamp: chrono::Utc::now(),
            },
        ];
        let prompt = Handoff::format_for_prompt(&[h]);
        assert!(
            prompt.contains("Tools used:"),
            "approved tools must appear under 'Tools used:'"
        );
        assert!(prompt.contains("Edit"));
        assert!(prompt.contains("Read"));
        assert!(
            !prompt.contains("Tools denied:"),
            "no denied tools, so 'Tools denied:' must be absent"
        );
    }

    #[test]
    fn format_for_prompt_shows_tools_denied() {
        let work = sample_work();
        let mut h = Handoff::new(0, "dev-agent", None, "rosary-t", "claude", &work, None);
        h.tools_used = vec![
            ToolCallRecord {
                tool_name: "Edit".to_string(),
                approved: true,
                timestamp: chrono::Utc::now(),
            },
            ToolCallRecord {
                tool_name: "Bash(curl evil.com)".to_string(),
                approved: false,
                timestamp: chrono::Utc::now(),
            },
        ];
        let prompt = Handoff::format_for_prompt(&[h]);
        assert!(prompt.contains("Tools used:"));
        assert!(prompt.contains("Edit"));
        assert!(
            prompt.contains("Tools denied:"),
            "denied tool must appear under 'Tools denied:'"
        );
        assert!(prompt.contains("Bash(curl evil.com)"));
    }

    #[test]
    fn format_for_prompt_omits_tools_section_when_empty() {
        let work = sample_work();
        let h = Handoff::new(0, "dev-agent", None, "rosary-t", "claude", &work, None);
        // tools_used is empty by default
        let prompt = Handoff::format_for_prompt(&[h]);
        assert!(
            !prompt.contains("Tools used:"),
            "empty tools_used must not emit 'Tools used:'"
        );
        assert!(!prompt.contains("Tools denied:"));
    }

    #[test]
    fn format_for_prompt_denied_only_omits_tools_used_section() {
        let work = sample_work();
        let mut h = Handoff::new(0, "dev-agent", None, "rosary-t", "claude", &work, None);
        h.tools_used = vec![ToolCallRecord {
            tool_name: "Bash(curl evil.com)".to_string(),
            approved: false,
            timestamp: chrono::Utc::now(),
        }];
        let prompt = Handoff::format_for_prompt(&[h]);
        assert!(
            !prompt.contains("Tools used:"),
            "denied-only tools_used must NOT emit 'Tools used:'"
        );
        assert!(
            prompt.contains("Tools denied:"),
            "denied-only tools_used must emit 'Tools denied:'"
        );
        assert!(prompt.contains("Bash(curl evil.com)"));
    }

    #[test]
    fn read_chain_detects_tampering() {
        let tmp = tempfile::TempDir::new().unwrap();
        let work = sample_work();

        let h0 = Handoff::new(
            0,
            "dev-agent",
            Some("staging-agent"),
            "rsry-x",
            "claude",
            &work,
            None,
        );
        let h1 = Handoff::new(
            1,
            "staging-agent",
            None,
            "rsry-x",
            "claude",
            &work,
            Some(&h0),
        );
        let h2 = Handoff::new(2, "prod-agent", None, "rsry-x", "claude", &work, Some(&h1));

        h0.write_to(tmp.path()).unwrap();
        h1.write_to(tmp.path()).unwrap();
        h2.write_to(tmp.path()).unwrap();

        // Sanity: clean chain reads all 3
        let clean = Handoff::read_chain(tmp.path());
        assert_eq!(clean.len(), 3, "clean chain should have 3 entries");

        // Tamper with phase 0 (change its summary)
        let mut h0_tampered = h0.clone();
        h0_tampered.summary = "TAMPERED".to_string();
        h0_tampered.write_to(tmp.path()).unwrap();

        // read_chain must detect that h1.previous_chain_hash no longer matches h0_tampered
        let tampered = Handoff::read_chain(tmp.path());
        assert!(
            tampered.len() < 3,
            "tampered chain should be truncated — got {} entries (expected < 3)",
            tampered.len()
        );
    }
}
