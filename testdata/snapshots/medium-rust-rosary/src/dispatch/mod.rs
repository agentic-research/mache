//! Dispatch beads to AI agent providers for execution.
//!
//! Two entry points:
//! - `run()`: Original blocking dispatch (reads Dolt, spawns agent, waits).
//! - `spawn()`: Async dispatch returning an `AgentHandle` for the reconciliation loop.
//!
//! The `AgentProvider` trait abstracts over different AI backends (Claude, Gemini,
//! Codex, etc). `ClaudeProvider` is the default implementation.

pub mod prompt;
pub mod providers;
pub mod session;
pub mod sweep;

#[cfg(test)]
pub(crate) mod tests;

// Re-export public API so callers can still use `dispatch::X`.
#[allow(unused_imports)] // API surface — not all re-exports consumed within crate yet
pub use prompt::{
    PROMPT_VERSION, build_prompt, build_system_prompt, load_agent_prompt, strip_frontmatter,
};
#[allow(unused_imports)] // API surface
pub use providers::{
    AcpCliProvider, AgentProvider, ClaudeProvider, GeminiProvider, provider_by_name,
};
#[allow(unused_imports)] // API surface
pub use session::{AgentSession, CliSession};

use anyhow::{Context, Result};
use std::path::{Path, PathBuf};

use crate::bead::Bead;
use crate::scanner::expand_path;
#[allow(unused_imports)] // Used when dispatch migrates fully to BeadStore
use crate::store::BeadStore;
use session::ComputeSession;

/// Permission profile for dispatched agents.
///
/// Derived from bead metadata (issue_type or explicit field), not the provider.
/// Each provider translates this to its own CLI flags.
///
/// Profiles are intentionally simple -- 3 levels. Complex per-tool rules
/// belong in a schema/config file, not in Rust match arms.
#[derive(Debug, Clone, Copy, Default, PartialEq, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PermissionProfile {
    /// Read + analyze only. For review, survey, audit.
    ReadOnly,
    /// Read + edit + test + commit. For bug, task, feature.
    #[default]
    Implement,
    /// Bead/project management via MCP. For planning, triage.
    Plan,
}

impl PermissionProfile {
    /// Claude `--allowedTools` flag value.
    ///
    /// rsry tools are scoped per role -- agents cannot close beads or merge
    /// workspaces. Only the reconciler/feature-agent does that.
    pub fn claude_allowed_tools(&self) -> &str {
        match self {
            // Dev/implement: can read code, edit, commit, comment on beads.
            // Cannot close beads, merge workspaces, or dispatch other agents.
            Self::Implement => concat!(
                "Read,Edit,Write,Bash(cargo *),Bash(go *),Bash(git *),Bash(task *),Glob,Grep,",
                "mcp__mache__*,",
                "mcp__rsry__rsry_bead_comment,mcp__rsry__rsry_bead_search,",
                "mcp__rsry__rsry_status,mcp__rsry__rsry_list_beads,mcp__rsry__rsry_active"
            ),
            // Review/audit: read-only code access + bead comments.
            Self::ReadOnly => concat!(
                "Read,Glob,Grep,",
                "mcp__mache__*,",
                "mcp__rsry__rsry_bead_comment,mcp__rsry__rsry_bead_search,",
                "mcp__rsry__rsry_status,mcp__rsry__rsry_list_beads"
            ),
            // Planning/triage: read code + full bead management (create, update, link).
            // Can create/update beads but still cannot close or merge.
            Self::Plan => concat!(
                "Read,Glob,Grep,",
                "mcp__mache__*,",
                "mcp__rsry__rsry_bead_create,mcp__rsry__rsry_bead_update,",
                "mcp__rsry__rsry_bead_comment,mcp__rsry__rsry_bead_search,",
                "mcp__rsry__rsry_bead_link,",
                "mcp__rsry__rsry_status,mcp__rsry__rsry_list_beads,",
                "mcp__rsry__rsry_decompose"
            ),
        }
    }

    /// Gemini `--approval-mode` flag value.
    pub fn gemini_approval_mode(&self) -> &str {
        match self {
            Self::ReadOnly => "plan",
            Self::Implement => "auto_edit",
            Self::Plan => "plan",
        }
    }
}

/// Filename for the agent stdout stream log within a workspace.
pub const STREAM_LOG_FILENAME: &str = ".rsry-stream.jsonl";

/// Handle to a running agent session.
pub struct AgentHandle {
    #[allow(dead_code)]
    pub bead_id: String,
    #[allow(dead_code)]
    pub generation: u64,
    pub session: Box<dyn AgentSession>,
    pub work_dir: PathBuf,
    pub started_at: chrono::DateTime<chrono::Utc>,
    pub workspace: Option<crate::workspace::Workspace>,
    /// Claude Code session ID (from --output-format json). Set after capture.
    /// Enables `--resume` on retry to preserve agent context across failures.
    #[allow(dead_code)]
    pub session_id: Option<String>,
    /// Path to the workspace directory (jj workspace or git worktree).
    /// Recorded in DispatchRecord for resume and debugging.
    #[allow(dead_code)]
    pub workspace_path: Option<String>,
    /// Path to the JSONL stream log capturing agent stdout.
    /// Contains init, assistant, and result events from `--output-format json`.
    #[allow(dead_code)]
    pub log_path: Option<PathBuf>,
    /// HEAD commit SHA of the target repo at dispatch time (APAS chain integrity, L1).
    #[allow(dead_code)]
    pub chain_hash: Option<String>,
}

impl AgentHandle {
    /// Non-blocking check: has the agent completed? Returns Some(success).
    pub fn try_wait(&mut self) -> Result<Option<bool>> {
        self.session.try_wait()
    }

    /// Block until the agent completes. Returns success.
    pub async fn wait(&mut self) -> Result<bool> {
        self.session.wait().await
    }

    /// Kill the agent.
    pub fn kill(&mut self) -> Result<()> {
        self.session.kill()
    }

    /// Process ID (if applicable).
    #[allow(dead_code)] // Used by reconciler path
    pub fn pid(&self) -> Option<u32> {
        self.session.pid()
    }

    /// Set the session ID (captured from agent output after spawn).
    /// Enables `--resume` on retry to preserve agent context.
    #[allow(dead_code)]
    pub fn set_session_id(&mut self, session_id: String) {
        self.session_id = Some(session_id);
    }

    /// Elapsed time since dispatch.
    pub fn elapsed(&self) -> chrono::Duration {
        chrono::Utc::now() - self.started_at
    }
}

// ---------------------------------------------------------------------------
// Agent pipeline -- phase progression
// ---------------------------------------------------------------------------

/// The default agent pipeline for a given issue type.
///
/// Must match `config::default_pipelines()` exactly — divergence here causes
/// `dispatch::default_agent()` (used by CLI and MCP handlers) to dispatch a
/// different first agent than the reconciler's PipelineEngine, silently skipping
/// the scoping phase for bugs and features dispatched via CLI.
fn agent_pipeline(issue_type: &str) -> &'static [&'static str] {
    match issue_type {
        "bug" => &["scoping-agent", "dev-agent", "staging-agent"],
        "feature" => &["scoping-agent", "dev-agent", "staging-agent", "prod-agent"],
        "task" | "chore" => &["dev-agent"],
        "review" => &["staging-agent"],
        "design" | "research" => &["architect-agent"],
        "epic" => &["pm-agent"],
        _ => &["dev-agent"],
    }
}

/// The default (first) agent for a given issue type.
pub fn default_agent(issue_type: &str) -> &'static str {
    agent_pipeline(issue_type)
        .first()
        .copied()
        .unwrap_or("dev-agent")
}

/// Derive the permission profile from the bead's issue type.
pub fn permission_profile(issue_type: &str) -> PermissionProfile {
    match issue_type {
        "review" | "survey" | "audit" => PermissionProfile::ReadOnly,
        "epic" | "plan" | "triage" => PermissionProfile::Plan,
        _ => PermissionProfile::Implement,
    }
}

/// Resolve agents_dir from config by finding the self-managed repo.
pub fn resolve_agents_dir() -> Option<PathBuf> {
    let cfg = crate::config::load_global().ok()?;
    cfg.repo
        .iter()
        .find(|r| r.self_managed)
        .map(|r| expand_path(&r.path).join("agents"))
        .filter(|p| p.exists())
}

// ---------------------------------------------------------------------------
// Dispatch
// ---------------------------------------------------------------------------

/// Spawn an AI agent for a bead. Returns a handle without waiting.
///
/// This is the async entry point for the reconciliation loop.
/// The `provider` argument controls which AI backend is used.
/// The `agents_dir` enables agent-aware system prompts from definition files.
///
/// Isolation uses `Workspace` which tries jj first, git worktree second,
/// then falls back to in-place if neither is available.
#[allow(clippy::too_many_arguments)] // Reconciler-driven entry point; each arg is load-bearing.
pub async fn spawn(
    bead: &Bead,
    repo_path: &Path,
    isolate: bool,
    generation: u64,
    provider: &dyn AgentProvider,
    agents_dir: Option<&Path>,
    compute: Option<&dyn crate::backend::ComputeProvider>,
    model: Option<String>,
) -> Result<AgentHandle> {
    let path = expand_path(repo_path);
    let repo_name = path
        .file_name()
        .map(|n| n.to_string_lossy().to_string())
        .unwrap_or_default();

    let workspace = crate::workspace::Workspace::create(&bead.id, &repo_name, &path, isolate)
        .await
        .with_context(|| format!("creating workspace for {}", bead.id))?;

    let work_dir = workspace.work_dir.clone();

    // Write bead ID to worktree so the commit-msg hook can inject the
    // [bead-id] prefix instead of rejecting commits.
    let _ = std::fs::write(work_dir.join(".rsry-bead-id"), &bead.id);

    // Exclude dispatch artifacts from git -- these are rosary metadata,
    // not part of the agent's work. Uses .git/info/exclude (local to this
    // worktree, not committed to the repo).
    // Worktrees have .git as a file (not a dir) pointing to the real gitdir.
    // Resolve the actual info/exclude path for either layout.
    let exclude_dir = if work_dir.join(".git").is_dir() {
        work_dir.join(".git").join("info")
    } else if let Ok(gitfile) = std::fs::read_to_string(work_dir.join(".git"))
        && let Some(gitdir) = gitfile.trim().strip_prefix("gitdir: ")
    {
        std::path::PathBuf::from(gitdir).join("info")
    } else {
        work_dir.join(".git").join("info") // fallback
    };
    let _ = (|| {
        use std::io::Write;
        std::fs::create_dir_all(&exclude_dir)?;
        let exclude_path = exclude_dir.join("exclude");
        let existing = std::fs::read_to_string(&exclude_path).unwrap_or_default();
        if !existing.lines().any(|l| l.trim() == ".rsry-*") {
            let mut file = std::fs::OpenOptions::new()
                .create(true)
                .append(true)
                .open(&exclude_path)?;
            if !existing.is_empty() && !existing.ends_with('\n') {
                writeln!(file)?;
            }
            writeln!(file, "# rosary dispatch artifacts")?;
            writeln!(file, ".rsry-*")?;
        }
        Ok::<(), std::io::Error>(())
    })();

    // Install per-dispatch git hooks into .rsry-hooks/ (already excluded
    // from git via .rsry-* in info/exclude). This sets core.hooksPath to
    // the per-worktree dir, giving us both bead-id injection (commit-msg)
    // and a language-aware fast check (pre-commit) without touching the
    // repo's own hooks or requiring ~/.rsry/hooks/ to exist.
    if isolate {
        install_hooks(&work_dir, &path);
    }

    let prompt = build_prompt(
        bead,
        &path.display().to_string(),
        Some(&work_dir),
        bead.owner.as_deref(),
    );

    // Build agent-aware system prompt from bead.owner
    let system_prompt = build_system_prompt(bead.owner.as_deref(), agents_dir);

    // Agent-specific permission override: scoping-agent is always ReadOnly
    let permissions = match bead.owner.as_deref() {
        Some("scoping-agent") => PermissionProfile::ReadOnly,
        Some("staging-agent") => PermissionProfile::ReadOnly,
        Some("pm-agent") => PermissionProfile::Plan,
        Some("architect-agent") => PermissionProfile::Plan,
        _ => permission_profile(&bead.issue_type),
    };

    // Apply per-phase model override if provided. The Box must outlive
    // the borrow we hand to `effective_provider`, so it lives at the
    // outer scope.
    let model_provider: Option<Box<dyn AgentProvider>> = if model.is_some() {
        Some(provider.with_model(model))
    } else {
        None
    };
    let effective_provider: &dyn AgentProvider = model_provider.as_deref().unwrap_or(provider);

    let agent_label = bead.owner.as_deref().unwrap_or("generic");
    eprintln!(
        "[dispatch] {} -> {} (agent={}, perms={:?})",
        bead.id,
        effective_provider.name(),
        agent_label,
        permissions
    );

    let session: Box<dyn AgentSession> = if let Some(compute) = compute {
        // Container dispatch: build command, provision, exec, destroy.
        // Synchronous -- spawn() blocks for exec duration. Session is already resolved.
        let (bin, args) = effective_provider.build_command(&prompt, &permissions, &system_prompt);
        anyhow::ensure!(
            !bin.is_empty(),
            "{} does not support build_command()",
            effective_provider.name()
        );

        let opts = crate::backend::ProvisionOpts::new(&bead.id, &repo_name);
        let exec_handle = compute
            .provision(&opts)
            .await
            .with_context(|| format!("provisioning {} for {}", compute.name(), bead.id))?;

        let mut cmd: Vec<String> = vec![bin];
        cmd.extend(args);

        let bead_id_clone = bead.id.clone();
        let handle_id = exec_handle.id.clone();
        let _backend_name = compute.name().to_string();

        // Background task: exec -> destroy (always, even on failure)
        let (tx, rx) = tokio::sync::oneshot::channel::<bool>();
        // We need to move the exec_handle into the spawned task, but
        // compute is borrowed. Use the ExecHandle + backend name to
        // call docker CLI directly in the task. This is a known limitation --
        // the real fix is making ComputeProvider: 'static + Clone.
        // For now, exec synchronously before spawning (same as before but
        // with proper cleanup).
        let cmd_refs: Vec<&str> = cmd.iter().map(|s| s.as_str()).collect();
        let exec_result = compute.exec(&exec_handle, &cmd_refs).await;

        // Always destroy, even on exec failure
        if let Err(e) = compute.destroy(&exec_handle).await {
            eprintln!("[dispatch] cleanup {}: {e}", handle_id);
        }

        let success = match exec_result {
            Ok(r) => {
                let ok = r.success();
                eprintln!(
                    "[dispatch] {} container {} exited {}",
                    bead_id_clone,
                    handle_id,
                    if ok { "ok" } else { "fail" }
                );
                ok
            }
            Err(e) => {
                eprintln!("[dispatch] {} exec failed: {e}", bead_id_clone);
                false
            }
        };

        // Send result -- if rx was dropped (kill), this is a no-op
        let _ = tx.send(success);

        Box::new(ComputeSession {
            rx: Some(rx),
            result: None,
        })
    } else {
        // Local dispatch: spawn agent process directly (existing behavior).
        // On failure, clean up the workspace so no orphaned worktrees are left.
        match effective_provider
            .spawn_agent(&prompt, &work_dir, &permissions, &system_prompt)
            .with_context(|| format!("spawning {} for {}", effective_provider.name(), bead.id))
        {
            Ok(session) => session,
            Err(e) => {
                workspace.cleanup();
                return Err(e);
            }
        }
    };

    // Record workspace path for dispatch tracking (resume + debugging).
    // This is the isolated work_dir, not the original repo root.
    let workspace_path = if work_dir != path {
        Some(work_dir.display().to_string())
    } else {
        None
    };

    let log_path = work_dir.join(STREAM_LOG_FILENAME);

    // Capture HEAD commit SHA for APAS chain integrity (L1 anchor).
    // Runs after workspace creation so the SHA reflects the isolated worktree state.
    let chain_hash = tokio::process::Command::new("git")
        .args(["rev-parse", "HEAD"])
        .current_dir(&work_dir)
        .output()
        .await
        .ok()
        .filter(|o| o.status.success())
        .map(|o| String::from_utf8_lossy(&o.stdout).trim().to_string());

    Ok(AgentHandle {
        bead_id: bead.id.clone(),
        generation,
        session,
        work_dir,
        started_at: chrono::Utc::now(),
        workspace: Some(workspace),
        session_id: None,
        workspace_path,
        log_path: Some(log_path),
        chain_hash,
    })
}

/// Original blocking dispatch -- reads Dolt, spawns agent, waits for completion.
/// Kept for `rsry dispatch` CLI command.
pub async fn run(bead_id: &str, repo_path: &Path, isolate: bool) -> Result<()> {
    let path = expand_path(repo_path);
    let beads_dir = path.join(".beads");

    let client = crate::bead_sqlite::connect_bead_store(&beads_dir).await?;

    let bead = client
        .get_bead(bead_id, &path.display().to_string())
        .await?
        .ok_or_else(|| anyhow::anyhow!("bead {bead_id} not found"))?;

    client.update_status(bead_id, "dispatched").await?;

    let agents_dir = resolve_agents_dir();
    let mut handle = spawn(
        &bead,
        &path,
        isolate,
        bead.generation(),
        &ClaudeProvider::default(),
        agents_dir.as_deref(),
        None, // compute: local subprocess (default)
        None, // model: use provider default
    )
    .await?;
    let success = handle.wait().await?;

    // The pipeline is authoritative for lifecycle transitions — not the agent.
    // If the agent already transitioned the bead (via MCP tools), respect that.
    // If it's still Dispatched after exit, the pipeline infers the next state
    // from artifacts so the bead never gets permanently stuck.
    let current_state = client
        .get_bead(bead_id, &path.display().to_string())
        .await
        .ok()
        .flatten()
        .map(|b| b.status);

    let still_dispatched = current_state
        .as_deref()
        .map(|s| crate::bead::BeadState::from(s) == crate::bead::BeadState::Dispatched)
        .unwrap_or(true);

    if !still_dispatched {
        eprintln!(
            "[dispatch] {bead_id} agent already transitioned to {:?} — pipeline defers",
            current_state
        );
        return Ok(());
    }

    // Bead is still Dispatched — pipeline takes over.
    let has_commits = if let Some(ref ws_path) = handle.workspace_path {
        tokio::process::Command::new("git")
            .args(["log", "--oneline", "HEAD", "--not", "origin/HEAD", "--"])
            .current_dir(ws_path)
            .output()
            .await
            .map(|o| o.status.success() && !o.stdout.is_empty())
            .unwrap_or(false)
    } else {
        false
    };

    if success {
        if has_commits {
            eprintln!("[dispatch] {bead_id} exited clean with commits — marking verifying");
            let _ = client.update_status(bead_id, "verifying").await;
        } else {
            eprintln!("[dispatch] {bead_id} exited clean, no new commits — closing");
            let _ = client.update_status(bead_id, "closed").await;
        }
    } else if has_commits {
        eprintln!("[dispatch] {bead_id} failed but left commits -- marking blocked for review");
        let _ = client
            .add_comment(
                bead_id,
                "agent",
                "Agent exited with failure but produced commits. Needs human review.",
            )
            .await;
        let _ = client.update_status(bead_id, "blocked").await;
    } else {
        eprintln!("[dispatch] {bead_id} crashed silently -- no commits, no artifacts");
        let _ = client
            .add_comment(
                bead_id,
                "agent",
                "Agent crashed silently -- no commits produced. Returning to open for retry.",
            )
            .await;
        let _ = client.update_status(bead_id, "open").await;
    }

    Ok(())
}

// ---------------------------------------------------------------------------
// Hook installation
// ---------------------------------------------------------------------------

/// Detect the primary language of a repo from well-known marker files.
pub(crate) fn detect_language(repo_path: &Path) -> &'static str {
    if repo_path.join("Cargo.toml").exists() {
        "rust"
    } else if repo_path.join("go.mod").exists() {
        "go"
    } else {
        "unknown"
    }
}

/// Install per-dispatch git hooks into `work_dir/.rsry-hooks/`.
///
/// Writes:
/// - `commit-msg`: injects `[bead-id]` prefix from `.rsry-bead-id` (Golden Rule 11)
/// - `pre-commit`: runs a fast language-specific compile check before each commit,
///   so agents can't commit code that fails `cargo check` / `go build ./...`
///
/// Sets `core.hooksPath` to `.rsry-hooks` (relative) so these take effect
/// without touching the repo's own hooks or requiring `~/.rsry/hooks/` to exist.
/// `.rsry-hooks/` is already excluded from git via `.rsry-*` in info/exclude.
fn install_hooks(work_dir: &Path, repo_path: &Path) {
    let hooks_dir = work_dir.join(".rsry-hooks");
    if std::fs::create_dir_all(&hooks_dir).is_err() {
        return;
    }

    let lang = detect_language(repo_path);

    // commit-msg: bead-id injection (Golden Rule 11 — every commit references a bead).
    // Checks only the first line for existing refs (body/footer may contain bead: references
    // without the prefix format). Uses awk to prefix only the subject line, preserving the
    // full message body unchanged.
    let commit_msg_hook = r#"#!/usr/bin/env bash
# Golden Rule 11: every commit must reference a bead.
# If .rsry-bead-id exists (agent worktree), inject the prefix automatically.
if head -n1 "$1" | grep -qiE "^Merge |^initial commit"; then exit 0; fi
if head -n1 "$1" | grep -qE '^\[[-a-zA-Z0-9]+\] '; then exit 0; fi
if grep -qiE "bead:" "$1"; then exit 0; fi
TOPLEVEL="$(git rev-parse --show-toplevel 2>/dev/null || echo '')"
if [ -z "$TOPLEVEL" ]; then
  echo "error: not in a git repo" >&2
  exit 1
fi
BEAD_ID_FILE="$TOPLEVEL/.rsry-bead-id"
if [ -f "$BEAD_ID_FILE" ]; then
  BEAD_ID=$(cat "$BEAD_ID_FILE" | tr -d '[:space:]')
  if [ -n "$BEAD_ID" ]; then
    tmp="$1.rsry-tmp"
    awk -v bead="$BEAD_ID" 'NR==1{$0="["bead"] "$0}1' "$1" > "$tmp" && mv "$tmp" "$1"
    exit 0
  fi
fi
echo "error: commit message must reference a bead (Golden Rule 11)" >&2
exit 1
"#;

    // pre-commit: fast compile check — catches broken code before the verify pipeline.
    let pre_commit_hook = match lang {
        "rust" => {
            "#!/usr/bin/env bash\n\
             # Rosary pre-commit: fast compile check (cargo check).\n\
             # Prevents committing code that fails to compile.\n\
             exec cargo check 2>&1\n"
        }
        "go" => {
            "#!/usr/bin/env bash\n\
             # Rosary pre-commit: fast compile check (go build ./...).\n\
             # Prevents committing code that fails to compile.\n\
             exec go build ./... 2>&1\n"
        }
        _ => "#!/usr/bin/env bash\nexit 0\n",
    };

    let write_executable = |name: &str, content: &str| {
        #[cfg(unix)]
        use std::os::unix::fs::PermissionsExt;
        let path = hooks_dir.join(name);
        if std::fs::write(&path, content).is_ok() {
            #[cfg(unix)]
            let _ = std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o755));
        }
    };

    write_executable("commit-msg", commit_msg_hook);
    write_executable("pre-commit", pre_commit_hook);

    // Point this worktree at the per-dispatch hooks dir (relative path keeps
    // the config valid regardless of where the worktree is on disk).
    // Try --worktree first (git ≥2.20, requires extensions.worktreeConfig)
    // to write only to this worktree's config without affecting the main repo.
    // Fall back to --local if --worktree isn't supported.
    let worktree_ok = std::process::Command::new("git")
        .args(["config", "--worktree", "core.hooksPath", ".rsry-hooks"])
        .current_dir(work_dir)
        .output()
        .map(|o| o.status.success())
        .unwrap_or(false);
    if !worktree_ok {
        let _ = std::process::Command::new("git")
            .args(["config", "core.hooksPath", ".rsry-hooks"])
            .current_dir(work_dir)
            .output();
    }

    eprintln!(
        "[hooks] installed {lang} pre-commit hook in {}",
        work_dir.display()
    );
}

// ---------------------------------------------------------------------------
// Detached spawn — for the MCP `rsry_dispatch` path (rosary-748f07).
//
// The orchestrator's `spawn_agent` returns a `Child` whose lifetime is bound
// to the caller. That works when the caller is `rsry run` (a long-lived
// orchestrator) but breaks when the caller is itself a Claude Code session
// invoking the MCP tool: the harness's safety classifier blocks the caller
// from spawning a sibling agent, and even if it didn't, the spawned child
// would die when the MCP request returned.
//
// `spawn_detached` solves this by:
//   1. Putting the child in a new session via `setsid` (Unix). No
//      controlling TTY; survives parent (rosary) death; reparents to PID 1
//      where init reaps it after rosary exits.
//   2. Spawning via `tokio::process::Command` and `tokio::spawn`-ing a
//      reaper task that awaits `child.wait()`. While rosary is alive the
//      tokio reaper handles waitpid so the child never becomes a zombie
//      in rosary's process table. If rosary exits first, setsid + init
//      take over reaping.
//   3. Matching the Claude CLI's validated stdio configuration: piped
//      stdin closed immediately after spawn (per the providers.rs PR #110
//      note — null stdin triggers a different SDK detection path that
//      breaks OAuth).
// ---------------------------------------------------------------------------

/// Pid plus the stream-log path the caller can tail to observe the agent.
#[derive(Debug)]
pub struct DetachedSpawn {
    pub pid: u32,
    pub stream_log: PathBuf,
}

/// Spawn an agent as a detached, session-leader subprocess.
///
/// Unlike `provider.spawn_agent()` (orchestrator path), this:
/// - calls `setsid(2)` in the child via `pre_exec` so the child is fully
///   detached from rosary's process group;
/// - moves the `Child` into a tokio reaper task that awaits exit (prevents
///   zombies while rosary is alive; if rosary dies first, setsid + init
///   take over);
/// - returns just the pid + log path (no `Box<dyn AgentSession>`).
///
/// Uses the provider's `build_command` to derive the binary + args, so
/// providers that don't implement it cannot be detached this way — the
/// caller gets an error rather than silently no-op'ing.
#[cfg(unix)]
pub async fn spawn_detached(
    provider: &dyn providers::AgentProvider,
    prompt: &str,
    work_dir: &Path,
    permissions: &PermissionProfile,
    system_prompt: &str,
) -> Result<DetachedSpawn> {
    let (binary, args) = provider.build_command(prompt, permissions, system_prompt);
    anyhow::ensure!(
        !binary.is_empty(),
        "{} does not support build_command(); detached spawn from the MCP path is not supported for this provider",
        provider.name(),
    );

    let log_path = work_dir.join(STREAM_LOG_FILENAME);
    let err_path = work_dir.join(".rsry-stderr.log");
    let log_file = std::fs::File::create(&log_path)
        .with_context(|| format!("creating stream log {}", log_path.display()))?;
    let err_file = std::fs::File::create(&err_path)
        .with_context(|| format!("creating stderr log {}", err_path.display()))?;

    let mut cmd = tokio::process::Command::new(&binary);
    cmd.args(&args)
        .current_dir(work_dir)
        .env_remove("GIT_DIR")
        .env_remove("GIT_WORK_TREE")
        .env_remove("GIT_INDEX_FILE")
        // CC harness markers — must not leak into the spawned child or the
        // child will think it's itself a CC subagent.
        .env_remove("CLAUDECODE")
        .env_remove("CLAUDE_CODE_ENTRYPOINT")
        // Piped stdin matches the orchestrator's claude provider invocation
        // (see providers.rs:88-91 — null stdin triggers CC's SDK detection
        // path which fails OAuth). We close the pipe immediately after
        // spawn so the child sees EOF on stdin via the SDK-compatible path.
        .stdin(std::process::Stdio::piped())
        .stdout(std::process::Stdio::from(log_file))
        .stderr(std::process::Stdio::from(err_file));

    if let Some(token) = providers::resolve_auth_token(work_dir) {
        cmd.env("CLAUDE_CODE_OAUTH_TOKEN", &token);
    }

    // SAFETY: the closure runs in the child between fork and exec. It must
    // be async-signal-safe — no allocator, no mutex, no global state. We
    // call only `setsid(2)`, which is on the async-signal-safe list.
    // tokio::process::Command exposes `pre_exec` directly, no trait import
    // required.
    unsafe {
        cmd.pre_exec(|| {
            if libc::setsid() == -1 {
                return Err(std::io::Error::last_os_error());
            }
            Ok(())
        });
    }

    let mut child = cmd
        .spawn()
        .with_context(|| format!("spawning {} (detached) in {}", binary, work_dir.display()))?;
    let pid = child
        .id()
        .ok_or_else(|| anyhow::anyhow!("spawned child has no pid (already exited?)"))?;

    // Close stdin immediately so the child sees EOF on read but the
    // SDK-detection path matches the piped-stdin OAuth-validated invocation.
    drop(child.stdin.take());

    // Hand the Child to a tokio task that reaps on exit. While rosary is
    // alive, this task performs `waitpid` so the child doesn't accumulate
    // as a zombie in rosary's process table. If rosary exits first,
    // setsid already moved the child to its own session — init/launchd
    // reaps after rosary's death.
    let provider_name = provider.name().to_string();
    tokio::spawn(async move {
        match child.wait().await {
            Ok(status) => eprintln!(
                "[dispatch-detached] {} pid={} exited with {}",
                provider_name, pid, status
            ),
            Err(e) => eprintln!(
                "[dispatch-detached] {} pid={} wait error: {}",
                provider_name, pid, e
            ),
        }
    });

    eprintln!(
        "[dispatch-detached] {} pid={} stream={}",
        provider.name(),
        pid,
        log_path.display(),
    );
    Ok(DetachedSpawn {
        pid,
        stream_log: log_path,
    })
}

#[cfg(test)]
mod detached_tests {
    use super::*;
    use std::time::Duration;

    /// Minimal test provider that exposes a shell command via
    /// `build_command`. Avoids needing a real `claude` binary in tests.
    struct ShellProvider {
        binary: String,
        args: Vec<String>,
    }

    impl providers::AgentProvider for ShellProvider {
        fn spawn_agent(
            &self,
            _prompt: &str,
            _work_dir: &Path,
            _permissions: &PermissionProfile,
            _system_prompt: &str,
        ) -> Result<Box<dyn session::AgentSession>> {
            unreachable!("spawn_detached uses build_command, not spawn_agent")
        }
        fn build_command(
            &self,
            _prompt: &str,
            _permissions: &PermissionProfile,
            _system_prompt: &str,
        ) -> (String, Vec<String>) {
            (self.binary.clone(), self.args.clone())
        }
        fn name(&self) -> &str {
            "shell-test"
        }
        fn with_model(&self, _model: Option<String>) -> Box<dyn providers::AgentProvider> {
            Box::new(ShellProvider {
                binary: self.binary.clone(),
                args: self.args.clone(),
            })
        }
    }

    async fn wait_for<F: Fn() -> bool>(check: F, timeout: Duration) -> bool {
        let start = std::time::Instant::now();
        while start.elapsed() < timeout {
            if check() {
                return true;
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        check()
    }

    /// Reap a process by sending SIGTERM and waiting via `waitpid`. Tests
    /// own this rather than relying on test-binary-exit reaping, which can
    /// leave zombies across sequential tests.
    fn kill_and_reap(pid: u32) {
        unsafe {
            libc::kill(pid as i32, libc::SIGTERM);
        }
        // The detached spawn's tokio reaper task does the actual waitpid
        // for us in production. In tests we ALSO call waitpid here in case
        // the reaper hasn't run yet (test binary may exit before tokio
        // scheduler ticks). WNOHANG + retry loop avoids hanging if the
        // tokio reaper already won the race.
        let deadline = std::time::Instant::now() + Duration::from_secs(2);
        while std::time::Instant::now() < deadline {
            let mut status: libc::c_int = 0;
            let r = unsafe { libc::waitpid(pid as i32, &mut status, libc::WNOHANG) };
            if r == pid as i32 || r == -1 {
                // r == pid: we reaped it. r == -1: tokio reaper already
                // reaped (ECHILD) — also a success for our purposes.
                return;
            }
            std::thread::sleep(Duration::from_millis(20));
        }
    }

    #[tokio::test]
    async fn detached_child_runs_and_survives_handle_drop() {
        let dir = tempfile::tempdir().unwrap();
        // Child writes a marker, sleeps long enough for assertions to run.
        let provider = ShellProvider {
            binary: "sh".into(),
            args: vec!["-c".into(), "printf started > marker; sleep 5".into()],
        };
        let perms = PermissionProfile::default();

        let result = spawn_detached(&provider, "ignored", dir.path(), &perms, "ignored")
            .await
            .unwrap();
        let pid = result.pid;

        // Child must reach its first statement (write marker) — proves the
        // spawn actually exec'd, not just forked-and-died.
        assert!(
            wait_for(
                || dir.path().join("marker").exists(),
                Duration::from_secs(2)
            )
            .await,
            "child never created marker file (did spawn fail?)"
        );
        assert_eq!(
            std::fs::read_to_string(dir.path().join("marker")).unwrap(),
            "started"
        );

        // Child still alive AFTER spawn returned and Child handle was moved
        // into the reaper task. This is the load-bearing property —
        // `tool_dispatch` returns to the MCP caller while the agent keeps
        // running, with the tokio reaper preventing zombies.
        assert!(
            crate::session::is_pid_alive(pid),
            "child died before assertions ran"
        );

        // setsid worked → child is its own session leader (sid == pid).
        let sid = unsafe { libc::getsid(pid as i32) };
        assert_eq!(
            sid as u32, pid,
            "child should be its own session leader after setsid"
        );

        // Stream log file exists (we redirected stdout there).
        assert!(result.stream_log.exists());

        kill_and_reap(pid);
    }

    #[tokio::test]
    async fn detached_errors_when_provider_has_no_build_command() {
        // Provider with empty build_command (the trait default) — must fail
        // clearly rather than silently spawning nothing.
        struct NoBuildProvider;
        impl providers::AgentProvider for NoBuildProvider {
            fn spawn_agent(
                &self,
                _: &str,
                _: &Path,
                _: &PermissionProfile,
                _: &str,
            ) -> Result<Box<dyn session::AgentSession>> {
                unreachable!()
            }
            fn name(&self) -> &str {
                "no-build"
            }
            fn with_model(&self, _: Option<String>) -> Box<dyn providers::AgentProvider> {
                Box::new(NoBuildProvider)
            }
            // No build_command override → trait default returns empty.
        }

        let dir = tempfile::tempdir().unwrap();
        let perms = PermissionProfile::default();
        let err = spawn_detached(&NoBuildProvider, "p", dir.path(), &perms, "s")
            .await
            .unwrap_err();
        assert!(
            err.to_string().contains("build_command"),
            "expected build_command error, got: {err}"
        );
    }

    #[tokio::test]
    async fn detached_writes_stdout_to_stream_log() {
        let dir = tempfile::tempdir().unwrap();
        let provider = ShellProvider {
            binary: "sh".into(),
            args: vec!["-c".into(), "echo hello-from-child; sleep 1".into()],
        };
        let perms = PermissionProfile::default();

        let result = spawn_detached(&provider, "p", dir.path(), &perms, "s")
            .await
            .unwrap();

        // Wait for stdout to flush to the log.
        assert!(
            wait_for(
                || {
                    std::fs::read_to_string(&result.stream_log)
                        .map(|s| s.contains("hello-from-child"))
                        .unwrap_or(false)
                },
                Duration::from_secs(2),
            )
            .await
        );

        kill_and_reap(result.pid);
    }

    /// Reaper-task does its job: a short-lived child exits and gets
    /// `waitpid`'d so it doesn't accumulate as a zombie in rosary's process
    /// table while rosary is still running. Verified by checking that the
    /// pid is gone (not zombie) from /proc/<pid>/stat or via `kill(pid, 0)`
    /// returning ESRCH within a bounded window.
    #[tokio::test]
    async fn detached_short_lived_child_gets_reaped() {
        let dir = tempfile::tempdir().unwrap();
        // Exit immediately — gives the reaper task something to wait on.
        let provider = ShellProvider {
            binary: "sh".into(),
            args: vec!["-c".into(), "exit 0".into()],
        };
        let perms = PermissionProfile::default();

        let result = spawn_detached(&provider, "p", dir.path(), &perms, "s")
            .await
            .unwrap();
        let pid = result.pid;

        // Within a bounded window, the reaper task must waitpid the exited
        // child. After reaping, `kill(pid, 0)` returns ESRCH (pid no longer
        // exists at all — zombies still respond to kill(0) with success).
        let reaped = wait_for(
            || {
                let r = unsafe { libc::kill(pid as i32, 0) };
                r == -1 && std::io::Error::last_os_error().raw_os_error() == Some(libc::ESRCH)
            },
            Duration::from_secs(3),
        )
        .await;
        assert!(
            reaped,
            "child pid {pid} was not reaped within 3s — reaper task didn't run?"
        );
    }
}
