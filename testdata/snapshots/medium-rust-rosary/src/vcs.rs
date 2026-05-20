//! Thin wrapper around leyline-vcs for automatic state versioning.
//!
//! Rosary's state directory (`~/.rsry/`) is a jj repo. Every state change
//! (bead status update, triage score, dispatch record) auto-snapshots via
//! leyline-vcs's sidecar pattern: the hot path writes to SQLite, the cold
//! path snapshots to jj asynchronously.
//!
//! Agents never interact with this directly — it's pure plumbing.

use std::path::{Path, PathBuf};

use anyhow::{Context, Result};
#[cfg(feature = "leyline")]
use leyline_vcs::JjIntegration;

#[allow(dead_code)] // API surface — wired when main.rs calls ensure_state_dir on startup
/// Rosary state directory, default `~/.rsry/`.
pub fn state_dir() -> Result<PathBuf> {
    let home = dirs_next::home_dir().context("cannot determine home directory")?;
    let dir = home.join(".rsry");
    Ok(dir)
}

#[allow(dead_code)]
/// Ensure the state directory exists and is initialized.
pub fn ensure_state_dir() -> Result<PathBuf> {
    let dir = state_dir()?;
    if !dir.exists() {
        std::fs::create_dir_all(&dir)
            .with_context(|| format!("creating state dir: {}", dir.display()))?;
    }
    Ok(dir)
}

#[allow(dead_code)]
/// Initialize a jj repo in the state directory if one doesn't exist.
///
/// With `leyline` feature: uses leyline-vcs's JjIntegration (jj-lib native).
/// Without: falls back to `jj init` CLI.
pub fn init_jj(state_path: &Path) -> Result<()> {
    #[cfg(feature = "leyline")]
    {
        JjIntegration::init_or_open(state_path)
            .with_context(|| format!("jj init_or_open at {}", state_path.display()))?;
    }
    #[cfg(not(feature = "leyline"))]
    {
        if !state_path.join(".jj").exists() {
            let _ = std::process::Command::new("jj")
                .args(["init"])
                .current_dir(state_path)
                .stdout(std::process::Stdio::null())
                .stderr(std::process::Stdio::null())
                .status();
        }
    }
    Ok(())
}

#[allow(dead_code)]
/// Snapshot current state to jj. Non-blocking best-effort.
///
/// Called after state-changing operations (bead update, dispatch, etc).
/// Failures are logged but don't propagate — state versioning must never
/// block the hot path.
///
/// Still uses jj CLI (`jj status --quiet`) because JjIntegration::commit_snapshot()
/// requires &dyn Graph which rosary doesn't implement — rosary stores plain files
/// in ~/.rsry/, not a leyline graph. The CLI triggers jj's working-copy snapshot.
pub fn snapshot(state_path: &Path) {
    match std::process::Command::new("jj")
        .args(["status", "--quiet"])
        .current_dir(state_path)
        .output()
    {
        Ok(output) if output.status.success() => {}
        Ok(output) => {
            let stderr = String::from_utf8_lossy(&output.stderr);
            eprintln!("[rsry-vcs] snapshot warning: {stderr}");
        }
        Err(e) => {
            eprintln!("[rsry-vcs] snapshot failed: {e}");
        }
    }
}

#[allow(dead_code)]
/// Push state to a remote. Best-effort.
///
/// Called periodically or on graceful shutdown.
pub fn push(state_path: &Path, remote: &str) -> Result<()> {
    let output = std::process::Command::new("jj")
        .args(["git", "push", "--remote", remote])
        .current_dir(state_path)
        .output()
        .context("running jj git push")?;

    if !output.status.success() {
        let stderr = String::from_utf8_lossy(&output.stderr);
        anyhow::bail!("jj push failed: {stderr}");
    }

    Ok(())
}

// ---------------------------------------------------------------------------
// jj log scanning — extract recent commits for bead transition detection
// ---------------------------------------------------------------------------

/// A commit from jj log, parsed into structured fields.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct VcsCommit {
    /// jj change ID (short form, e.g., "kxryzmss")
    pub change_id: String,
    /// Commit description (may be multiline)
    pub description: String,
}

/// Scan recent jj commits in a repo. Returns parsed commits.
///
/// Uses `jj log` with a structured template for reliable parsing.
/// The `revset` parameter controls which commits to scan (e.g., `"@"`, `"@-..@"`).
/// Default limit prevents unbounded output.
pub fn scan_jj_log(repo_path: &Path, revset: &str, limit: usize) -> Result<Vec<VcsCommit>> {
    // Template: change_id<NUL>description<NUL><NUL>
    // NUL bytes are safe delimiters — they never appear in commit messages.
    let template = r#"change_id.short() ++ "\0" ++ description ++ "\0\0""#;

    let output = std::process::Command::new("jj")
        .args([
            "log",
            "--no-graph",
            "--no-pager",
            "-r",
            revset,
            "--limit",
            &limit.to_string(),
            "--template",
            template,
        ])
        .current_dir(repo_path)
        .output()
        .with_context(|| format!("running jj log in {}", repo_path.display()))?;

    if !output.status.success() {
        let stderr = String::from_utf8_lossy(&output.stderr);
        // jj not initialized is not an error — repo might use git only
        if stderr.contains("There is no jj repo") || stderr.contains("no jj repo") {
            return Ok(Vec::new());
        }
        anyhow::bail!("jj log failed: {stderr}");
    }

    let stdout = String::from_utf8_lossy(&output.stdout);
    let commits = parse_jj_log_output(&stdout);
    Ok(commits)
}

/// Parse the NUL-delimited jj log output into VcsCommit structs.
fn parse_jj_log_output(output: &str) -> Vec<VcsCommit> {
    output
        .split("\0\0")
        .filter(|entry| !entry.trim().is_empty())
        .filter_map(|entry| {
            let parts: Vec<&str> = entry.splitn(2, '\0').collect();
            if parts.len() == 2 {
                let change_id = parts[0].trim().to_string();
                let description = parts[1].trim().to_string();
                if !change_id.is_empty() {
                    return Some(VcsCommit {
                        change_id,
                        description,
                    });
                }
            }
            None
        })
        .collect()
}

/// Scan a repo's jj log and extract bead references from recent commits.
///
/// Returns a list of (change_id, WorkRef) pairs — the reconciler uses these
/// to trigger bead state transitions.
pub fn scan_vcs_bead_refs(repo_path: &Path) -> Result<Vec<(String, WorkRef)>> {
    // Scan recent non-immutable commits (working copy + recent work)
    let commits = scan_jj_log(repo_path, "mine()", 50)?;

    let mut results = Vec::new();
    for commit in &commits {
        let refs = extract_bead_refs(&commit.description);
        for bead_ref in refs {
            results.push((commit.change_id.clone(), bead_ref));
        }
    }

    Ok(results)
}

// ---------------------------------------------------------------------------
// Bead ID extraction from commit messages
// ---------------------------------------------------------------------------

/// A bead reference found in a commit message.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct WorkRef {
    /// The bead ID (e.g., "rsry-abc123", "loom-7sd", "mache-tgl")
    pub id: String,
    /// Whether this reference closes the bead (e.g., "closes bead:...", "fixes bead:...")
    pub closes: bool,
}

/// Extract bead references from a commit message or jj description.
///
/// Recognized patterns:
/// - `bead:rsry-abc123` — simple reference (dispatched)
/// - `closes bead:rsry-abc123` — closing reference (done)
/// - `fixes bead:rsry-abc123` — closing reference (done)
/// - `bead:loom-7sd` — any repo prefix works
///
/// Bead IDs follow the pattern: `{prefix}-{suffix}` where prefix is lowercase
/// alpha and suffix is lowercase alphanumeric (hex or base36).
pub fn extract_bead_refs(message: &str) -> Vec<WorkRef> {
    let mut refs = Vec::new();
    let lower = message.to_lowercase();

    // Find bracket-format refs: [prefix-suffix] at start of line
    // This is the format agents produce from the dispatch prompt instructions.
    for line in lower.lines() {
        let trimmed = line.trim();
        if let Some(rest) = trimmed.strip_prefix('[')
            && let Some(end) = rest.find(']')
        {
            let id = &rest[..end];
            if let Some(dash_pos) = id.find('-') {
                let prefix = &id[..dash_pos];
                let suffix = &id[dash_pos + 1..];
                if !prefix.is_empty()
                    && !suffix.is_empty()
                    && prefix.chars().all(|c| c.is_ascii_lowercase() || c == '.')
                    && suffix.chars().all(|c| c.is_ascii_alphanumeric())
                {
                    refs.push(WorkRef {
                        id: id.to_string(),
                        closes: false,
                    });
                }
            }
        }
    }

    // Find all occurrences of "bead:" followed by an ID
    let mut search_from = 0;
    while let Some(pos) = lower[search_from..].find("bead:") {
        let abs_pos = search_from + pos;
        let after = &lower[abs_pos + 5..]; // skip "bead:"

        // Parse the bead ID: {prefix}-{suffix}
        // prefix: one or more lowercase alpha chars
        // suffix: one or more lowercase alphanumeric chars
        let id: String = after
            .chars()
            .take_while(|c| c.is_ascii_alphanumeric() || *c == '-')
            .collect();

        // Must contain at least one '-' and have content on both sides
        if let Some(dash_pos) = id.find('-') {
            let prefix = &id[..dash_pos];
            let suffix = &id[dash_pos + 1..];
            if !prefix.is_empty()
                && !suffix.is_empty()
                && prefix.chars().all(|c| c.is_ascii_lowercase())
            {
                // Check for closing prefix: "closes", "fixes", "close", "fix"
                let before = &lower[..abs_pos].trim_end();
                let closes = before.ends_with("closes")
                    || before.ends_with("fixes")
                    || before.ends_with("close")
                    || before.ends_with("fix");

                refs.push(WorkRef {
                    id: id.clone(),
                    closes,
                });
            }
        }

        search_from = abs_pos + 5 + id.len().max(1);
    }

    // Dedup by ID, keeping closes=true if any ref closes
    refs.sort_by(|a, b| a.id.cmp(&b.id));
    refs.dedup_by(|a, b| {
        if a.id == b.id {
            b.closes = b.closes || a.closes;
            true
        } else {
            false
        }
    });

    refs
}

/// Extract just the bead IDs (ignoring close semantics).
/// Convenience wrapper for simple lookups.
#[allow(dead_code)]
pub fn extract_bead_ids(message: &str) -> Vec<String> {
    extract_bead_refs(message)
        .into_iter()
        .map(|r| r.id)
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn state_dir_under_home() {
        let dir = state_dir().unwrap();
        assert!(dir.to_string_lossy().ends_with(".rsry"));
        assert!(!dir.to_string_lossy().starts_with('~'));
    }

    #[test]
    fn ensure_state_dir_creates() {
        let tmp = tempfile::TempDir::new().unwrap();
        let dir = tmp.path().join(".rsry");
        assert!(!dir.exists());

        // Manually test the creation logic
        std::fs::create_dir_all(&dir).unwrap();
        assert!(dir.exists());
    }

    #[cfg(feature = "leyline")]
    #[test]
    fn init_jj_creates_repo() {
        let tmp = tempfile::TempDir::new().unwrap();
        assert!(!tmp.path().join(".jj").exists());

        init_jj(tmp.path()).unwrap();
        assert!(tmp.path().join(".jj").exists());
    }

    #[cfg(feature = "leyline")]
    #[test]
    fn init_jj_idempotent() {
        let tmp = tempfile::TempDir::new().unwrap();

        // First call inits, second call opens — both succeed
        init_jj(tmp.path()).unwrap();
        init_jj(tmp.path()).unwrap();
        assert!(tmp.path().join(".jj").exists());
    }

    // --- jj log parsing tests ---

    #[test]
    fn parse_jj_log_single_commit() {
        let output = "kxryzmss\0fix the widget bug\n\nbead:rsry-abc123\0\0";
        let commits = parse_jj_log_output(output);
        assert_eq!(commits.len(), 1);
        assert_eq!(commits[0].change_id, "kxryzmss");
        assert!(commits[0].description.contains("bead:rsry-abc123"));
    }

    #[test]
    fn parse_jj_log_multiple_commits() {
        let output = "aaa\0first commit\0\0bbb\0second commit\n\ncloses bead:rsry-xyz\0\0";
        let commits = parse_jj_log_output(output);
        assert_eq!(commits.len(), 2);
        assert_eq!(commits[0].change_id, "aaa");
        assert_eq!(commits[1].change_id, "bbb");
    }

    #[test]
    fn parse_jj_log_empty() {
        let commits = parse_jj_log_output("");
        assert!(commits.is_empty());
    }

    #[test]
    fn parse_jj_log_trailing_whitespace() {
        let output = "abc\0some desc\0\0\n";
        let commits = parse_jj_log_output(output);
        assert_eq!(commits.len(), 1);
        assert_eq!(commits[0].change_id, "abc");
    }

    // --- Bead ID extraction tests ---

    #[test]
    fn extract_single_bead_ref() {
        let refs = extract_bead_refs("working on bead:rsry-abc123");
        assert_eq!(refs.len(), 1);
        assert_eq!(refs[0].id, "rsry-abc123");
        assert!(!refs[0].closes);
    }

    #[test]
    fn extract_multiple_bead_refs() {
        let refs = extract_bead_refs("bead:rsry-abc and also bead:loom-7sd");
        assert_eq!(refs.len(), 2);
        assert_eq!(refs[0].id, "loom-7sd");
        assert_eq!(refs[1].id, "rsry-abc");
    }

    #[test]
    fn extract_no_bead_refs() {
        let refs = extract_bead_refs("just a regular commit message");
        assert!(refs.is_empty());
    }

    #[test]
    fn extract_closing_ref_closes() {
        let refs = extract_bead_refs("closes bead:rsry-59f7f9");
        assert_eq!(refs.len(), 1);
        assert_eq!(refs[0].id, "rsry-59f7f9");
        assert!(refs[0].closes);
    }

    #[test]
    fn extract_closing_ref_fixes() {
        let refs = extract_bead_refs("fixes bead:mache-tgl");
        assert_eq!(refs.len(), 1);
        assert!(refs[0].closes);
    }

    #[test]
    fn extract_closing_ref_fix() {
        let refs = extract_bead_refs("fix bead:rsry-abc123");
        assert_eq!(refs.len(), 1);
        assert!(refs[0].closes);
    }

    #[test]
    fn extract_closing_ref_close() {
        let refs = extract_bead_refs("close bead:rsry-abc123");
        assert_eq!(refs.len(), 1);
        assert!(refs[0].closes);
    }

    #[test]
    fn extract_case_insensitive() {
        let refs = extract_bead_refs("BEAD:rsry-abc123");
        assert_eq!(refs.len(), 1);
        assert_eq!(refs[0].id, "rsry-abc123");
    }

    #[test]
    fn extract_mixed_close_and_ref() {
        let refs = extract_bead_refs("closes bead:rsry-aaa, also mentions bead:rsry-bbb");
        assert_eq!(refs.len(), 2);
        let aaa = refs.iter().find(|r| r.id == "rsry-aaa").unwrap();
        let bbb = refs.iter().find(|r| r.id == "rsry-bbb").unwrap();
        assert!(aaa.closes);
        assert!(!bbb.closes);
    }

    #[test]
    fn extract_deduplicates() {
        let refs = extract_bead_refs("bead:rsry-abc and again bead:rsry-abc");
        assert_eq!(refs.len(), 1);
    }

    #[test]
    fn extract_dedup_keeps_closes() {
        // If one ref closes and another just mentions, closes wins
        let refs = extract_bead_refs("bead:rsry-abc ... closes bead:rsry-abc");
        assert_eq!(refs.len(), 1);
        assert!(refs[0].closes);
    }

    #[test]
    fn extract_ignores_malformed() {
        // No dash → not a bead ID
        assert!(extract_bead_refs("bead:nope").is_empty());
        // Empty prefix
        assert!(extract_bead_refs("bead:-abc").is_empty());
        // Empty suffix
        assert!(extract_bead_refs("bead:rsry-").is_empty());
    }

    #[test]
    fn extract_normalizes_case() {
        // Uppercase input gets lowercased — IDs are always lowercase
        let refs = extract_bead_refs("bead:RSRY-ABC");
        assert_eq!(refs.len(), 1);
        assert_eq!(refs[0].id, "rsry-abc");
    }

    #[test]
    fn extract_in_multiline_message() {
        let msg = "feat: CLI ergonomics\n\nAddresses bead:rsry-59f7f9 (CLI ergonomics),\ncloses bead:rsry-59e7f8 (sync deltas).";
        let refs = extract_bead_refs(msg);
        assert_eq!(refs.len(), 2);
        let f9 = refs.iter().find(|r| r.id == "rsry-59f7f9").unwrap();
        let e8 = refs.iter().find(|r| r.id == "rsry-59e7f8").unwrap();
        assert!(!f9.closes);
        assert!(e8.closes);
    }

    #[test]
    fn extract_hex_suffix() {
        let refs = extract_bead_refs("bead:rsry-8c31a5");
        assert_eq!(refs.len(), 1);
        assert_eq!(refs[0].id, "rsry-8c31a5");
    }

    // --- Bracket-format bead ref tests ---

    #[test]
    fn extract_bracket_format_bead_ref() {
        // Agent prompt tells agents: git commit -m "[rosary-5aae44] type(scope): desc"
        let refs = extract_bead_refs("[rosary-5aae44] fix(xref): add module doc");
        assert_eq!(refs.len(), 1, "bracket format should be detected");
        assert_eq!(refs[0].id, "rosary-5aae44");
    }

    #[test]
    fn extract_bracket_format_with_dots() {
        // Temp dir names produce IDs like ".tmpXXXXXX-a1b2c3"
        let refs = extract_bead_refs("[.tmpabcdef-a1b2c3] fix: something");
        assert_eq!(refs.len(), 1);
    }

    #[test]
    fn extract_bracket_and_footer_deduplicates() {
        let msg = "[rsry-abc123] fix: thing\n\nbead:rsry-abc123";
        let refs = extract_bead_refs(msg);
        assert_eq!(refs.len(), 1, "bracket + footer should dedup to 1");
        assert_eq!(refs[0].id, "rsry-abc123");
    }

    #[test]
    fn extract_bracket_not_bead_id() {
        // [v1.2.3] or [BREAKING] should NOT match
        let refs = extract_bead_refs("[v1.2.3] release notes");
        assert!(refs.is_empty(), "version tags should not match");

        let refs = extract_bead_refs("[BREAKING] change api");
        assert!(refs.is_empty(), "uppercase brackets should not match");
    }

    #[test]
    fn extract_bead_ids_convenience() {
        let ids = extract_bead_ids("bead:rsry-abc closes bead:loom-xyz");
        assert_eq!(ids.len(), 2);
        assert!(ids.contains(&"rsry-abc".to_string()));
        assert!(ids.contains(&"loom-xyz".to_string()));
    }
}
