//! Agent provider implementations — Claude, Gemini, ACP CLI.
//!
//! The `AgentProvider` trait abstracts over different AI backends.
//! Each provider translates `PermissionProfile` to its own CLI flags.

use anyhow::{Context, Result};
use std::path::Path;

use super::session::{AgentSession, CliSession};
use super::{PermissionProfile, STREAM_LOG_FILENAME};

/// Trait for AI agent providers. Implementations handle spawning and
/// communicating with different AI backends (Claude, Gemini, Codex, etc).
///
/// The `permissions` argument comes from the bead — the provider just
/// translates it to CLI flags. This keeps schema/config decisions out
/// of the provider code.
pub trait AgentProvider: Send + Sync {
    /// Spawn an agent session with the given prompt, working directory,
    /// permission profile (derived from the bead), and system prompt
    /// (assembled from agent definitions + golden rules).
    fn spawn_agent(
        &self,
        prompt: &str,
        work_dir: &Path,
        permissions: &PermissionProfile,
        system_prompt: &str,
    ) -> Result<Box<dyn AgentSession>>;

    /// Build the CLI command that would be passed to the agent, without spawning.
    /// Returns (binary, args). Used by ComputeProvider to run in a container.
    #[allow(dead_code)] // API surface — used when compute != local
    fn build_command(
        &self,
        prompt: &str,
        permissions: &PermissionProfile,
        system_prompt: &str,
    ) -> (String, Vec<String>) {
        // Default: not supported — providers override if they can be containerized
        let _ = (prompt, permissions, system_prompt);
        (String::new(), Vec::new())
    }

    /// Human-readable name of this provider.
    fn name(&self) -> &str;

    /// Clone this provider with a model override for a single dispatch.
    ///
    /// Providers that support model selection (Claude) use the model;
    /// others return a copy of themselves unchanged.
    fn with_model(&self, model: Option<String>) -> Box<dyn AgentProvider>;
}

/// Provider that shells out to the Claude Code CLI (`claude -p`).
///
/// Uses `--allowedTools` with permission rule syntax to grant the agent
/// the tools it needs without interactive prompts.
pub struct ClaudeProvider {
    /// Absolute path to the claude binary. If empty, uses PATH lookup.
    pub binary: String,
    /// Optional model override (e.g. "claude-haiku-4-5-20251001").
    /// Passed as `--model` to the Claude CLI.
    pub model: Option<String>,
}

impl Default for ClaudeProvider {
    fn default() -> Self {
        Self {
            binary: "claude".to_string(),
            model: None,
        }
    }
}

impl AgentProvider for ClaudeProvider {
    fn spawn_agent(
        &self,
        prompt: &str,
        work_dir: &Path,
        permissions: &PermissionProfile,
        system_prompt: &str,
    ) -> Result<Box<dyn AgentSession>> {
        let log_path = work_dir.join(STREAM_LOG_FILENAME);
        let err_path = work_dir.join(".rsry-stderr.log");
        let err_file = std::fs::File::create(&err_path)
            .with_context(|| format!("creating stderr log {}", err_path.display()))?;

        // Exact pattern from PR #110 that was validated working with OAuth.
        // Key: -p mode, --output-format json, piped stdin, ENTRYPOINT removed.
        let allowed = permissions.claude_allowed_tools();
        let log_file = std::fs::File::create(&log_path)
            .with_context(|| format!("creating stream log {}", log_path.display()))?;
        eprintln!(
            "[spawn] {} -p <prompt> --allowedTools '{}' --output-format json (cwd={})",
            self.binary,
            allowed,
            work_dir.display()
        );

        // Resolve OAuth token for launchd context where Keychain OAuth
        // isn't available. Check env vars, then .envrc in the repo root.
        let auth_token = resolve_auth_token(work_dir);

        let mut cmd = tokio::process::Command::new(&self.binary);
        let mut base_args = vec![
            "-p",
            prompt,
            "--allowedTools",
            allowed,
            "--append-system-prompt",
            system_prompt,
            "--output-format",
            "json",
        ];
        if let Some(ref m) = self.model {
            base_args.extend_from_slice(&["--model", m.as_str()]);
        }
        cmd.args(base_args)
            .current_dir(work_dir)
            .env_remove("GIT_DIR")
            .env_remove("GIT_WORK_TREE")
            .env_remove("GIT_INDEX_FILE")
            .env_remove("CLAUDECODE")
            .env_remove("CLAUDE_CODE_ENTRYPOINT")
            .stdin(std::process::Stdio::piped())
            .stdout(std::process::Stdio::from(log_file))
            .stderr(std::process::Stdio::from(err_file));

        if let Some(ref token) = auth_token {
            cmd.env("CLAUDE_CODE_OAUTH_TOKEN", token);
            eprintln!("[spawn] passing CLAUDE_CODE_OAUTH_TOKEN to agent");
        }

        let child = cmd
            .spawn()
            .with_context(|| format!("spawning claude CLI in {}", work_dir.display()))?;

        let pid = child.id().unwrap_or(0);
        eprintln!("[spawn] claude started (pid={pid})");

        Ok(Box::new(CliSession::new(child)))
    }

    fn build_command(
        &self,
        prompt: &str,
        permissions: &PermissionProfile,
        system_prompt: &str,
    ) -> (String, Vec<String>) {
        let mut args = vec![
            "-p".to_string(),
            prompt.to_string(),
            "--allowedTools".to_string(),
            permissions.claude_allowed_tools().to_string(),
            "--append-system-prompt".to_string(),
            system_prompt.to_string(),
            "--output-format".to_string(),
            "json".to_string(),
        ];
        if let Some(ref m) = self.model {
            args.push("--model".to_string());
            args.push(m.clone());
        }
        (self.binary.clone(), args)
    }

    fn name(&self) -> &str {
        "claude"
    }

    fn with_model(&self, model: Option<String>) -> Box<dyn AgentProvider> {
        Box::new(ClaudeProvider {
            binary: self.binary.clone(),
            model,
        })
    }
}

/// Provider that shells out to the Gemini CLI (`gemini -p`).
///
/// Uses `--approval-mode` to control permission prompts.
#[derive(Default)]
pub struct GeminiProvider {
    /// Path to the gemini binary. Defaults to "gemini".
    #[allow(dead_code)]
    pub binary: String,
    /// Extra CLI args beyond permissions.
    pub extra_args: Vec<String>,
}

impl GeminiProvider {
    /// Resolve the binary path, defaulting to `gemini` from PATH.
    fn resolved_binary(&self) -> &str {
        if self.binary.is_empty() {
            "gemini"
        } else {
            self.binary.as_str()
        }
    }
}

impl AgentProvider for GeminiProvider {
    fn build_command(
        &self,
        prompt: &str,
        permissions: &PermissionProfile,
        system_prompt: &str,
    ) -> (String, Vec<String>) {
        // Mirror spawn_agent's invocation so detached spawn (MCP path)
        // produces the same wire as the orchestrator path. Gemini doesn't
        // support --append-system-prompt; prepend to user prompt instead.
        let full_prompt = format!("{system_prompt}\n\n---\n\n{prompt}");
        let mut args = vec![
            "-p".to_string(),
            full_prompt,
            "-o".to_string(),
            "json".to_string(),
            "--approval-mode".to_string(),
            permissions.gemini_approval_mode().to_string(),
        ];
        args.extend(self.extra_args.iter().cloned());
        (self.resolved_binary().to_string(), args)
    }

    fn spawn_agent(
        &self,
        prompt: &str,
        work_dir: &Path,
        permissions: &PermissionProfile,
        system_prompt: &str,
    ) -> Result<Box<dyn AgentSession>> {
        // Gemini CLI doesn't have --append-system-prompt; prepend to user prompt.
        let full_prompt = format!("{system_prompt}\n\n---\n\n{prompt}");
        let log_path = work_dir.join(STREAM_LOG_FILENAME);
        let log_file = std::fs::File::create(&log_path)
            .with_context(|| format!("creating stream log {}", log_path.display()))?;
        let err_path = work_dir.join(".rsry-stderr.log");
        let err_file = std::fs::File::create(&err_path)
            .with_context(|| format!("creating stderr log {}", err_path.display()))?;
        let bin = if self.binary.is_empty() {
            "gemini"
        } else {
            &self.binary
        };
        let mut cmd = tokio::process::Command::new(bin);
        cmd.args([
            "-p",
            &full_prompt,
            "-o",
            "json",
            "--approval-mode",
            permissions.gemini_approval_mode(),
        ]);
        for arg in &self.extra_args {
            cmd.arg(arg);
        }
        let child = cmd
            .current_dir(work_dir)
            .env_remove("GIT_DIR")
            .env_remove("GIT_WORK_TREE")
            .env_remove("GIT_INDEX_FILE")
            .env_remove("CLAUDECODE")
            .env_remove("CLAUDE_CODE_ENTRYPOINT")
            .stdin(std::process::Stdio::null())
            .stdout(std::process::Stdio::from(log_file))
            .stderr(std::process::Stdio::from(err_file))
            .spawn()
            .with_context(|| format!("spawning gemini CLI in {}", work_dir.display()))?;
        Ok(Box::new(CliSession::new(child)))
    }

    fn name(&self) -> &str {
        "gemini"
    }

    fn with_model(&self, _model: Option<String>) -> Box<dyn AgentProvider> {
        Box::new(GeminiProvider {
            binary: self.binary.clone(),
            extra_args: self.extra_args.clone(),
        })
    }
}

/// Provider that spawns an ACP-compatible agent binary.
///
/// The binary must implement the Agent Client Protocol (JSON-RPC over stdio).
/// Permission handling happens via `RosaryClient::request_permission()` in
/// the ACP session, not via CLI flags.
///
/// Example binaries: `claude-agent-acp` (npm), custom ACP agents.
#[allow(dead_code)] // Legacy stub — replaced by AcpNativeProvider
pub struct AcpCliProvider {
    /// Path or name of the ACP agent binary.
    pub binary: String,
}

impl AgentProvider for AcpCliProvider {
    fn spawn_agent(
        &self,
        _prompt: &str,
        work_dir: &Path,
        _permissions: &PermissionProfile,
        _system_prompt: &str,
    ) -> Result<Box<dyn AgentSession>> {
        // ACP agents are spawned as subprocesses with stdio piped.
        // The prompt and permissions are sent via ACP protocol (JSON-RPC),
        // not CLI args. The caller must establish a ClientSideConnection
        // after spawning and use Agent::prompt() to send the task.
        let log_path = work_dir.join(STREAM_LOG_FILENAME);
        let log_file = std::fs::File::create(&log_path)
            .with_context(|| format!("creating stream log {}", log_path.display()))?;
        let err_path = work_dir.join(".rsry-stderr.log");
        let err_file = std::fs::File::create(&err_path)
            .with_context(|| format!("creating stderr log {}", err_path.display()))?;
        let child = tokio::process::Command::new(&self.binary)
            .current_dir(work_dir)
            .env_remove("GIT_DIR")
            .env_remove("GIT_WORK_TREE")
            .env_remove("GIT_INDEX_FILE")
            .env_remove("CLAUDECODE")
            .env_remove("CLAUDE_CODE_ENTRYPOINT")
            // null stdin — piped stdin triggers CC's SDK detection which
            // uses different auth handling and fails with "Not logged in".
            // -p mode with null stdin uses standard OAuth from Keychain.
            .stdin(std::process::Stdio::null())
            .stdout(std::process::Stdio::from(log_file))
            .stderr(std::process::Stdio::from(err_file))
            .spawn()
            .with_context(|| format!("spawning ACP agent: {}", self.binary))?;
        Ok(Box::new(CliSession::new(child)))
    }

    fn name(&self) -> &str {
        "acp"
    }

    fn with_model(&self, _model: Option<String>) -> Box<dyn AgentProvider> {
        Box::new(AcpCliProvider {
            binary: self.binary.clone(),
        })
    }
}

/// Provider that uses the ACP protocol natively via `ClientSideConnection`.
///
/// Spawns the agent binary as a subprocess, establishes a JSON-RPC connection,
/// and runs the full ACP lifecycle (initialize → new_session → prompt).
/// Works with any ACP-compatible binary: claude-agent-acp, gemini-agent-acp, etc.
pub struct AcpNativeProvider {
    /// Path or name of the ACP agent binary.
    pub binary: String,
}

impl AgentProvider for AcpNativeProvider {
    fn spawn_agent(
        &self,
        prompt: &str,
        work_dir: &Path,
        permissions: &PermissionProfile,
        system_prompt: &str,
    ) -> Result<Box<dyn AgentSession>> {
        let log_path = work_dir.join(STREAM_LOG_FILENAME);
        eprintln!(
            "[spawn] ACP native: {} (cwd={})",
            self.binary,
            work_dir.display()
        );
        let auth_token = resolve_auth_token(work_dir);
        if auth_token.is_some() {
            eprintln!("[spawn] passing auth token to ACP agent");
        }
        let session = crate::acp::spawn_acp_session(
            &self.binary,
            prompt,
            work_dir,
            *permissions,
            system_prompt,
            &log_path,
            auth_token.as_deref(),
        )?;
        Ok(Box::new(session))
    }

    fn name(&self) -> &str {
        "acp"
    }

    fn with_model(&self, _model: Option<String>) -> Box<dyn AgentProvider> {
        Box::new(AcpNativeProvider {
            binary: self.binary.clone(),
        })
    }
}

/// Resolve auth token for agent spawning. Launchd services can't access
/// Keychain OAuth, so we read CLAUDE_CODE_OAUTH_TOKEN from env or .envrc.
///
/// Priority:
/// 1. `CLAUDE_CODE_OAUTH_TOKEN` env var
/// 2. `ANTHROPIC_API_KEY` env var
/// 3. `.envrc` in work_dir
/// 4. `.envrc` in git repo root (for worktrees)
/// 5. `dispatch.anthropic_api_key` in `~/.rsry/config.toml` (wasteland / hosted rigs)
pub(crate) fn resolve_auth_token(work_dir: &Path) -> Option<String> {
    // 1. Env vars (set by direnv, shell profile, or launchd plist)
    if let Ok(token) = std::env::var("CLAUDE_CODE_OAUTH_TOKEN") {
        return Some(token);
    }
    if let Ok(token) = std::env::var("ANTHROPIC_API_KEY") {
        return Some(token);
    }

    // 2-4. Read from .envrc (direnv pattern) — check work_dir and git origin
    let mut paths = vec![work_dir.join(".envrc")];
    if let Ok(output) = std::process::Command::new("git")
        .args(["rev-parse", "--git-common-dir"])
        .current_dir(work_dir)
        .output()
    {
        let git_common = String::from_utf8_lossy(&output.stdout).trim().to_string();
        if let Some(repo_root) = std::path::Path::new(&git_common).parent() {
            paths.push(repo_root.join(".envrc"));
        }
    }

    for path in &paths {
        if let Ok(content) = std::fs::read_to_string(path) {
            for line in content.lines() {
                if let Some(val) = line
                    .strip_prefix("export CLAUDE_CODE_OAUTH_TOKEN=")
                    .or_else(|| line.strip_prefix("export ANTHROPIC_API_KEY="))
                {
                    let val = val.trim().trim_matches('"').trim_matches('\'');
                    if !val.is_empty() {
                        return Some(val.to_string());
                    }
                }
            }
        }
    }

    // 5. Global config fallback — dispatch.anthropic_api_key in ~/.rsry/config.toml.
    // Used by wasteland rigs and hosted services where no per-repo .envrc exists.
    // Uses load_global() so $RSRY_CONFIG env var doesn't redirect to a project config.
    if let Ok(cfg) = crate::config::load_global()
        && let Some(key) = cfg.dispatch.and_then(|d| d.anthropic_api_key)
        && !key.is_empty()
    {
        return Some(key);
    }

    None
}

/// Resolve a provider by name string, with optional binary path overrides from config.
pub fn provider_by_name(
    name: &str,
    binaries: &std::collections::HashMap<String, String>,
) -> Result<Box<dyn AgentProvider>> {
    match name {
        "claude" => {
            let binary = binaries
                .get("claude")
                .cloned()
                .unwrap_or_else(|| "claude".to_string());
            Ok(Box::new(ClaudeProvider {
                binary,
                model: None,
            }))
        }
        "gemini" => {
            let binary = binaries
                .get("gemini")
                .cloned()
                .unwrap_or_else(|| "gemini".to_string());
            Ok(Box::new(GeminiProvider {
                binary,
                ..Default::default()
            }))
        }
        "acp" => {
            let binary = binaries
                .get("acp")
                .cloned()
                .unwrap_or_else(|| "claude-agent-acp".to_string());
            Ok(Box::new(AcpNativeProvider { binary }))
        }
        other => anyhow::bail!("unknown provider: {other} (available: claude, gemini, acp)"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn claude_build_command_produces_dash_p_invocation() {
        let provider = ClaudeProvider::default();
        let perms = PermissionProfile::default();
        let (bin, args) = provider.build_command("the prompt", &perms, "the sys prompt");
        assert_eq!(bin, "claude");
        // -p flag + prompt
        assert!(args.iter().any(|a| a == "-p"));
        assert!(args.iter().any(|a| a == "the prompt"));
        // System prompt is appended via --append-system-prompt (claude-specific)
        assert!(args.iter().any(|a| a == "--append-system-prompt"));
        assert!(args.iter().any(|a| a == "the sys prompt"));
        // Allowed tools wired through
        assert!(args.iter().any(|a| a == "--allowedTools"));
        // Output format pins JSON for stream parsing
        assert!(args.iter().any(|a| a == "--output-format"));
        assert!(args.iter().any(|a| a == "json"));
    }

    /// Regression for Copilot finding on PR #200: `GeminiProvider` used the
    /// trait-default `build_command` (empty), so `rsry_dispatch
    /// provider=gemini` errored at spawn time even though the tool schema
    /// advertised gemini as a valid choice. Gemini now implements
    /// `build_command` mirroring its `spawn_agent` invocation.
    #[test]
    fn gemini_build_command_is_non_empty_and_mirrors_spawn_agent() {
        let provider = GeminiProvider {
            binary: String::new(), // default → "gemini" from PATH
            extra_args: vec!["--foo".to_string()],
        };
        let perms = PermissionProfile::default();
        let (bin, args) = provider.build_command("the prompt", &perms, "the sys prompt");

        assert_eq!(bin, "gemini", "default binary should resolve to gemini");
        assert!(
            !args.is_empty(),
            "gemini build_command must not fall through to trait-default empty (rosary-748f07)"
        );
        // Gemini has no --append-system-prompt; system prompt is prepended.
        let p_index = args
            .iter()
            .position(|a| a == "-p")
            .expect("gemini build_command must include -p flag");
        let full_prompt = &args[p_index + 1];
        assert!(
            full_prompt.contains("the sys prompt"),
            "system prompt must be prepended into -p arg, got: {full_prompt}"
        );
        assert!(
            full_prompt.contains("the prompt"),
            "user prompt must be in -p arg, got: {full_prompt}"
        );
        // Extra args appended verbatim
        assert!(args.iter().any(|a| a == "--foo"));
        // Approval mode wired through
        assert!(args.iter().any(|a| a == "--approval-mode"));
    }

    #[test]
    fn gemini_resolved_binary_falls_back_to_path_lookup() {
        let p = GeminiProvider {
            binary: String::new(),
            extra_args: vec![],
        };
        assert_eq!(p.resolved_binary(), "gemini");
        let p = GeminiProvider {
            binary: "/opt/gemini-cli".to_string(),
            extra_args: vec![],
        };
        assert_eq!(p.resolved_binary(), "/opt/gemini-cli");
    }
}
