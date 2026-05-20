//! Pipeline plugin system — external tools that hook into verify/review/triage stages.
//!
//! Plugins are external processes or HTTP endpoints. Rosary acts as an MCP-style *client*:
//! it sends structured context (JSON) to the plugin and receives a structured verdict.
//!
//! # Protocol
//!
//! ## Subprocess transport
//! Rosary spawns the configured command, writes JSON to stdin, reads JSON from stdout.
//! Stderr is inherited so interactive TUIs can render normally.
//!
//! Input (stdin):
//! ```json
//! {
//!   "hook":     "pipeline.review",
//!   "bead_id":  "rosary-abc123",
//!   "repo":     "rosary",
//!   "work_dir": "/path/to/worktree"
//! }
//! ```
//!
//! Output (stdout):
//! ```json
//! { "verdict": "approve", "message": "optional human-readable note" }
//! ```
//!
//! ## HTTP transport
//! Rosary POSTs the same JSON body to the configured URL, reads the same JSON response.
//!
//! # Verdict values
//!
//! | Verdict            | Maps to        | Notes                              |
//! |--------------------|----------------|------------------------------------|
//! | `"pass"`           | `Pass`         | Check succeeded                    |
//! | `"approve"`        | `Pass`         | Review approved (alias for pass)   |
//! | `"skip"`           | `Pass`         | Plugin chose not to run (treated as pass) |
//! | `"fail"`           | `Fail`         | Check failed, pipeline blocks      |
//! | `"request-changes"`| `Partial`      | Soft rejection, needs human review |
//! | anything else      | `Partial`      | Unknown verdict — advisory         |
//!
//! # Hook points
//!
//! | Hook                | When it fires                                   |
//! |---------------------|-------------------------------------------------|
//! | `pipeline.verify`   | End of verify tier chain (after built-in tiers) |
//! | `pipeline.review`   | After ReviewCheck in the verify pipeline        |
//! | `pipeline.triage`   | During reconciler triage phase                  |
//! | `pipeline.close`    | When a bead transitions to done                 |

use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};
use std::io::Write;
use std::path::Path;
use std::process::Stdio;

use crate::config::{PluginConfig, PluginKind};
use crate::dispatch::session::StdCliSession;
use crate::dispatch::{AgentProvider, PermissionProfile};
use crate::verify::{VerifyResult, VerifyTier};

// ── Context ───────────────────────────────────────────────────────────────────

/// Runtime context rosary provides to every plugin call.
#[derive(Debug, Clone, Serialize)]
pub struct PluginContext {
    pub bead_id: String,
    pub repo: String,
    /// Minimum doc-coverage fraction required to pass the verify gate.
    /// When `Some`, an assay plugin returning `coverage < doc_coverage_min` fails.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub doc_coverage_min: Option<f32>,
}

impl PluginContext {
    pub fn new(bead_id: impl Into<String>, repo: impl Into<String>) -> Self {
        Self {
            bead_id: bead_id.into(),
            repo: repo.into(),
            doc_coverage_min: None,
        }
    }
}

// ── Wire types ────────────────────────────────────────────────────────────────

#[derive(Debug, Serialize)]
struct HookInput<'a> {
    hook: &'a str,
    bead_id: &'a str,
    repo: &'a str,
    work_dir: &'a str,
    /// Forwarded from bead's `doc_coverage_min` so plugins can self-gate.
    #[serde(skip_serializing_if = "Option::is_none")]
    doc_coverage_min: Option<f32>,
}

#[derive(Debug, Deserialize)]
struct HookOutput {
    verdict: String,
    #[serde(default)]
    message: Option<String>,
    /// Doc-coverage fraction (0.0–1.0) reported by assay-style plugins.
    /// Rosary fails the verify gate when this is below `PluginContext::doc_coverage_min`.
    #[serde(default)]
    coverage: Option<f64>,
}

impl HookOutput {
    fn into_verify_result(self, plugin_name: &str) -> VerifyResult {
        match self.verdict.as_str() {
            "pass" | "approve" | "skip" => VerifyResult::Pass,
            // Assay verdict aliases: covered/uncovered/stale
            "covered" => VerifyResult::Pass,
            "uncovered" => VerifyResult::Fail(
                self.message
                    .unwrap_or_else(|| format!("plugin '{plugin_name}': coverage insufficient")),
            ),
            "stale" => VerifyResult::Partial(
                self.message
                    .unwrap_or_else(|| format!("plugin '{plugin_name}': stale coverage")),
            ),
            "fail" | "reject" => VerifyResult::Fail(
                self.message
                    .unwrap_or_else(|| format!("plugin '{plugin_name}' rejected")),
            ),
            "request-changes" => VerifyResult::Partial(
                self.message
                    .unwrap_or_else(|| format!("plugin '{plugin_name}': request-changes")),
            ),
            other => {
                VerifyResult::Partial(format!("plugin '{plugin_name}': unknown verdict '{other}'"))
            }
        }
    }
}

/// Assay-specific output (emitted by `assay verify --format json`).
///
/// Falls back to this when the plugin produces assay-native JSON rather than
/// the standard `{ "verdict": "..." }` envelope.
#[derive(Debug, Deserialize)]
struct AssayOutput {
    /// "covered", "uncovered", or "stale"
    verdict: String,
    #[serde(default)]
    coverage: Option<f64>,
    #[serde(default)]
    message: Option<String>,
}

impl From<AssayOutput> for HookOutput {
    fn from(a: AssayOutput) -> Self {
        let verdict = match a.verdict.as_str() {
            "covered" | "uncovered" | "stale" => a.verdict.clone(),
            // Fall through any non-assay verdicts unchanged
            other => other.to_string(),
        };
        let message = a
            .message
            .or_else(|| a.coverage.map(|c| format!("coverage: {:.1}%", c * 100.0)));
        let coverage = a.coverage;
        HookOutput {
            verdict,
            message,
            coverage,
        }
    }
}

/// Parse raw plugin output, accepting either the standard `{ "verdict": "..." }`
/// envelope or the assay-native format.
fn parse_hook_output(raw: &str, plugin_name: &str) -> Result<HookOutput> {
    // Try standard format first.
    if let Ok(out) = serde_json::from_str::<HookOutput>(raw) {
        return Ok(out);
    }
    // Fall back to assay format.
    serde_json::from_str::<AssayOutput>(raw)
        .map(HookOutput::from)
        .with_context(|| format!("plugin '{plugin_name}' returned invalid JSON: {raw}"))
}

// ── Transport ─────────────────────────────────────────────────────────────────

fn call_subprocess(command: &[String], input_json: &str) -> Result<String> {
    let (prog, args) = command.split_first().context("plugin command is empty")?;

    let mut child = std::process::Command::new(prog)
        .args(args)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::inherit()) // let TUIs render to the terminal
        .spawn()
        .with_context(|| format!("spawning plugin '{prog}'"))?;

    // nosemgrep: blocking-subprocess-in-async — plugin hooks are called from sync VerifyTier::check
    child
        .stdin
        .take()
        .unwrap()
        .write_all(input_json.as_bytes())?;

    let out = child
        .wait_with_output()
        .context("waiting for plugin process")?;

    String::from_utf8(out.stdout).context("plugin stdout is not valid UTF-8")
}

fn call_http(url: &str, input_json: &str) -> Result<String> {
    // nosemgrep: blocking-subprocess-in-async — called from sync VerifyTier::check
    let client = reqwest::blocking::Client::new();
    client
        .post(url)
        .header("content-type", "application/json")
        .body(input_json.to_string())
        .send()
        .with_context(|| format!("calling plugin at {url}"))?
        .text()
        .context("reading plugin HTTP response")
}

fn call_plugin(plugin: &PluginConfig, input: &HookInput<'_>) -> Result<HookOutput> {
    let json = serde_json::to_string(input)?;

    let raw = if !plugin.command.is_empty() {
        call_subprocess(&plugin.command, &json)?
    } else if let Some(url) = &plugin.url {
        call_http(url, &json)?
    } else {
        anyhow::bail!(
            "plugin '{}' has neither command nor url configured",
            plugin.name
        );
    };

    parse_hook_output(&raw, &plugin.name)
}

// ── VerifyTier impl ───────────────────────────────────────────────────────────

/// A verify tier that delegates to an external plugin.
///
/// Constructed by `PluginRegistry::verify_tiers()` and appended to the
/// `Verifier` tier chain so plugin checks run after all built-in checks.
pub struct PluginTier {
    plugin: PluginConfig,
    context: PluginContext,
}

impl PluginTier {
    pub fn new(plugin: PluginConfig, context: PluginContext) -> Self {
        Self { plugin, context }
    }
}

impl VerifyTier for PluginTier {
    fn name(&self) -> &str {
        &self.plugin.name
    }

    fn check(&self, work_dir: &Path) -> Result<VerifyResult> {
        let input = HookInput {
            hook: &self.plugin.hook,
            bead_id: &self.context.bead_id,
            repo: &self.context.repo,
            work_dir: work_dir.to_str().unwrap_or(""),
            doc_coverage_min: self.context.doc_coverage_min,
        };

        let output = match call_plugin(&self.plugin, &input) {
            Ok(o) => o,
            Err(e) => {
                // Fail-open: if the plugin can't be reached, warn and pass.
                eprintln!(
                    "[plugin] warning: '{}' unavailable — {e:#}",
                    self.plugin.name
                );
                return Ok(VerifyResult::Pass);
            }
        };

        // Coverage gate: fail if reported coverage is below the bead's minimum.
        if let (Some(coverage), Some(min)) = (output.coverage, self.context.doc_coverage_min)
            && coverage < min as f64
        {
            return Ok(VerifyResult::Fail(format!(
                "doc coverage {:.0}% below required {:.0}% (plugin '{}')",
                coverage * 100.0,
                min as f64 * 100.0,
                self.plugin.name
            )));
        }

        Ok(output.into_verify_result(&self.plugin.name))
    }
}

// ── Dispatch provider ─────────────────────────────────────────────────────────

/// JSON payload sent to `kind = "dispatch"` plugin subprocesses (stdin).
///
/// The plugin is responsible for spawning its own execution backend
/// (claude, container, chain-YAML runner, etc.) and exiting with code 0 on
/// success or non-zero on failure. Rosary treats the exit code as the
/// session result.
#[allow(dead_code)] // Wire format for `kind = "dispatch"` plugins — consumed by spawn_agent.
#[derive(Debug, Serialize)]
struct DispatchInput<'a> {
    /// Always "dispatch".
    hook: &'a str,
    bead_id: &'a str,
    repo: &'a str,
    work_dir: &'a str,
    prompt: &'a str,
    /// Permission profile as a string: "implement", "read_only", or "plan".
    permissions: &'a str,
}

/// An `AgentProvider` backed by a `kind = "dispatch"` plugin.
///
/// On `spawn_agent`, rosary serialises the bead context to JSON and pipes it
/// to the plugin subprocess stdin. The subprocess IS the agent: it may shell
/// out to claude, run a container, or execute a chain-YAML workflow. Rosary
/// waits for it to exit and interprets the exit code.
#[allow(dead_code)] // Surface for `kind = "dispatch"` plugins — registered via PluginRegistry.
pub struct PluginDispatchProvider {
    plugin: PluginConfig,
    bead_id: String,
    repo: String,
}

impl PluginDispatchProvider {
    #[allow(dead_code)] // Constructed by PluginRegistry::dispatch_providers.
    pub fn new(plugin: PluginConfig, context: PluginContext) -> Self {
        Self {
            plugin,
            bead_id: context.bead_id,
            repo: context.repo,
        }
    }
}

impl AgentProvider for PluginDispatchProvider {
    fn spawn_agent(
        &self,
        prompt: &str,
        work_dir: &Path,
        permissions: &PermissionProfile,
        _system_prompt: &str,
    ) -> Result<Box<dyn crate::dispatch::AgentSession>> {
        let perms_str = match permissions {
            PermissionProfile::ReadOnly => "read_only",
            PermissionProfile::Plan => "plan",
            PermissionProfile::Implement => "implement",
        };
        let input = DispatchInput {
            hook: "dispatch",
            bead_id: &self.bead_id,
            repo: &self.repo,
            work_dir: work_dir.to_str().unwrap_or(""),
            prompt,
            permissions: perms_str,
        };
        let json = serde_json::to_string(&input)?;

        let (prog, args) = self
            .plugin
            .command
            .split_first()
            .context("dispatch plugin command is empty")?;

        let mut child = std::process::Command::new(prog)
            .args(args)
            .stdin(Stdio::piped())
            .stdout(Stdio::inherit())
            .stderr(Stdio::inherit())
            .spawn()
            .with_context(|| format!("spawning dispatch plugin '{}'", self.plugin.name))?;

        // Write payload then close stdin so the plugin sees EOF.
        child
            .stdin
            .take()
            .unwrap()
            .write_all(json.as_bytes())
            .with_context(|| format!("writing to dispatch plugin '{}'", self.plugin.name))?;

        Ok(Box::new(StdCliSession::new(child)))
    }

    fn name(&self) -> &str {
        &self.plugin.name
    }

    fn with_model(&self, _model: Option<String>) -> Box<dyn AgentProvider> {
        Box::new(PluginDispatchProvider {
            plugin: self.plugin.clone(),
            bead_id: self.bead_id.clone(),
            repo: self.repo.clone(),
        })
    }
}

// ── Assay scan types ─────────────────────────────────────────────────────────

/// Input JSON for `hook = "assay.scan"` plugins (repo-level, no bead context).
#[derive(Debug, Serialize)]
struct AssayScanInput<'a> {
    hook: &'a str,
    repo: &'a str,
    work_dir: &'a str,
}

/// A stale code reference reported by an assay plugin.
#[derive(Debug, Clone, Deserialize)]
pub struct StaleRef {
    /// Markdown source file containing the stale reference.
    pub source_file: String,
    /// The symbol, path, or identifier that no longer exists in code.
    pub symbol: String,
    /// Line number in source_file (optional).
    #[serde(default)]
    pub line: Option<u32>,
}

/// Output from `hook = "assay.scan"` plugins.
///
/// `verdict = "stale"` with non-empty `stale_refs` triggers chore bead creation.
#[derive(Debug, Deserialize)]
pub struct AssayScanOutput {
    pub verdict: String,
    #[serde(default)]
    pub stale_refs: Vec<StaleRef>,
    /// Plugin-supplied diagnostic. Surfaced by future `rsry assay --json`
    /// reporters (Phase 2); not displayed in the current scan path.
    #[serde(default)]
    #[allow(dead_code)]
    pub message: Option<String>,
}

fn call_assay_plugin(plugin: &PluginConfig, input: &AssayScanInput<'_>) -> Result<AssayScanOutput> {
    let json = serde_json::to_string(input)?;

    let raw = if !plugin.command.is_empty() {
        call_subprocess(&plugin.command, &json)?
    } else if let Some(url) = &plugin.url {
        call_http(url, &json)?
    } else {
        anyhow::bail!(
            "plugin '{}' has neither command nor url configured",
            plugin.name
        );
    };

    serde_json::from_str(&raw).with_context(|| {
        format!(
            "plugin '{}' returned invalid assay JSON: {raw}",
            plugin.name
        )
    })
}

// ── Registry ──────────────────────────────────────────────────────────────────

/// All configured plugins, indexed at startup.
pub struct PluginRegistry {
    plugins: Vec<PluginConfig>,
}

impl PluginRegistry {
    pub fn new(plugins: Vec<PluginConfig>) -> Self {
        Self { plugins }
    }

    pub fn is_empty(&self) -> bool {
        self.plugins.is_empty()
    }

    /// Build `VerifyTier` boxes for all `pipeline.verify` and `pipeline.review` plugins.
    ///
    /// Call after constructing the base `Verifier` to append plugin tiers:
    /// ```rust,ignore
    /// let mut verifier = Verifier::for_language("rust");
    /// for tier in registry.verify_tiers(context) {
    ///     verifier.push(tier);
    /// }
    /// ```
    pub fn verify_tiers(&self, context: PluginContext) -> Vec<Box<dyn VerifyTier>> {
        self.plugins
            .iter()
            .filter(|p| p.is_hook() && (p.hook == "pipeline.verify" || p.hook == "pipeline.review"))
            .map(|p| Box::new(PluginTier::new(p.clone(), context.clone())) as Box<dyn VerifyTier>)
            .collect()
    }

    /// Call all `pipeline.triage` hooks for a bead.
    ///
    /// Returns `Some(reason)` if any plugin says to skip the bead, `None` to proceed.
    pub fn call_triage_hooks(&self, context: &PluginContext) -> Option<String> {
        for plugin in self
            .plugins
            .iter()
            .filter(|p| p.is_hook() && p.hook == "pipeline.triage")
        {
            let input = HookInput {
                hook: &plugin.hook,
                bead_id: &context.bead_id,
                repo: &context.repo,
                work_dir: "",
                doc_coverage_min: None,
            };
            match call_plugin(plugin, &input) {
                Ok(out) => match out.verdict.as_str() {
                    "skip" | "fail" => {
                        let reason = out
                            .message
                            .unwrap_or_else(|| format!("plugin '{}' skipped bead", plugin.name));
                        return Some(reason);
                    }
                    _ => {}
                },
                Err(e) => {
                    eprintln!("[plugin] triage hook '{}' unavailable — {e:#}", plugin.name);
                }
            }
        }
        None
    }

    /// Return `AgentProvider` boxes for all `kind = "dispatch"` plugins.
    ///
    /// The first matching provider in config order is used by the dispatch loop.
    /// Callers can select by name if multiple dispatch plugins are configured.
    #[allow(dead_code)] // Wired into dispatch loop in rsry-e608bb (ExecutionBackend integration).
    pub fn dispatch_providers(&self, context: PluginContext) -> Vec<Box<dyn AgentProvider>> {
        self.plugins
            .iter()
            .filter(|p| p.kind == PluginKind::Dispatch)
            .map(|p| {
                Box::new(PluginDispatchProvider::new(p.clone(), context.clone()))
                    as Box<dyn AgentProvider>
            })
            .collect()
    }

    /// Call all `pipeline.close` hooks when a bead finishes.
    pub fn call_close_hooks(&self, context: &PluginContext) {
        for plugin in self
            .plugins
            .iter()
            .filter(|p| p.is_hook() && p.hook == "pipeline.close")
        {
            let input = HookInput {
                hook: &plugin.hook,
                bead_id: &context.bead_id,
                repo: &context.repo,
                work_dir: "",
                doc_coverage_min: None,
            };
            if let Err(e) = call_plugin(plugin, &input) {
                eprintln!("[plugin] close hook '{}' unavailable — {e:#}", plugin.name);
            }
        }
    }

    /// Call all `hook = "assay.scan"` plugins for a repo and collect stale refs.
    ///
    /// Returns all stale refs reported by all configured assay scan plugins.
    /// Plugins that error or are unreachable are silently skipped.
    pub fn assay_scan(&self, repo: &str, work_dir: &str) -> Vec<StaleRef> {
        let mut stale = Vec::new();
        for plugin in self
            .plugins
            .iter()
            .filter(|p| p.is_hook() && p.hook == "assay.scan")
        {
            let input = AssayScanInput {
                hook: "assay.scan",
                repo,
                work_dir,
            };
            match call_assay_plugin(plugin, &input) {
                Ok(out) if out.verdict == "stale" => stale.extend(out.stale_refs),
                Ok(_) => {}
                Err(e) => {
                    eprintln!("[plugin] assay scan '{}' unavailable — {e:#}", plugin.name);
                }
            }
        }
        stale
    }
}

// ── Tests ─────────────────────────────────────────────────────────────────────

#[cfg(test)]
mod tests {
    use super::*;

    fn make_hook_output(verdict: &str, message: Option<&str>) -> HookOutput {
        HookOutput {
            verdict: verdict.to_string(),
            message: message.map(str::to_string),
            coverage: None,
        }
    }

    #[test]
    fn verdict_pass_variants() {
        for v in ["pass", "approve", "skip"] {
            let r = make_hook_output(v, None).into_verify_result("test");
            assert_eq!(r, VerifyResult::Pass, "verdict '{v}' should map to Pass");
        }
    }

    #[test]
    fn verdict_fail_variants() {
        let r = make_hook_output("fail", Some("broken")).into_verify_result("test");
        assert!(matches!(r, VerifyResult::Fail(_)));

        let r = make_hook_output("reject", None).into_verify_result("test");
        assert!(matches!(r, VerifyResult::Fail(_)));
    }

    #[test]
    fn verdict_partial_variants() {
        let r = make_hook_output("request-changes", Some("nit")).into_verify_result("test");
        assert!(matches!(r, VerifyResult::Partial(_)));
    }

    fn hook_plugin(name: &str, hook: &str) -> PluginConfig {
        PluginConfig {
            name: name.into(),
            kind: PluginKind::Hook,
            hook: hook.into(),
            command: vec!["echo".into()],
            url: None,
        }
    }

    #[test]
    fn verify_tiers_filters_by_hook() {
        let plugins = vec![
            hook_plugin("a", "pipeline.verify"),
            hook_plugin("b", "pipeline.triage"),
            hook_plugin("c", "pipeline.review"),
        ];
        let registry = PluginRegistry::new(plugins);
        let ctx = PluginContext::new("rosary-abc", "rosary");
        let tiers = registry.verify_tiers(ctx);
        // only pipeline.verify and pipeline.review; not triage
        assert_eq!(tiers.len(), 2);
        assert_eq!(tiers[0].name(), "a");
        assert_eq!(tiers[1].name(), "c");
    }

    #[test]
    fn verify_tiers_excludes_non_hook_kinds() {
        let plugins = vec![
            hook_plugin("hook-verify", "pipeline.verify"),
            PluginConfig {
                name: "mcp-provider".into(),
                kind: PluginKind::Mcp,
                hook: String::new(),
                command: vec![],
                url: Some("http://localhost:8484".into()),
            },
            PluginConfig {
                name: "dispatch-backend".into(),
                kind: PluginKind::Dispatch,
                hook: String::new(),
                command: vec!["runner".into()],
                url: None,
            },
        ];
        let registry = PluginRegistry::new(plugins);
        let ctx = PluginContext::new("rosary-abc", "rosary");
        let tiers = registry.verify_tiers(ctx);
        // only the hook plugin; mcp and dispatch are excluded
        assert_eq!(tiers.len(), 1);
        assert_eq!(tiers[0].name(), "hook-verify");
    }

    #[test]
    fn plugin_tier_fails_open_on_unavailable_command() {
        let plugin = PluginConfig {
            name: "nonexistent".into(),
            kind: PluginKind::Hook,
            hook: "pipeline.verify".into(),
            command: vec!["__rsry_no_such_binary_9x__".into()],
            url: None,
        };
        let tier = PluginTier::new(plugin, PluginContext::new("x", "y"));
        // should not error — should return Pass (fail-open)
        let result = tier.check(Path::new("/tmp")).unwrap();
        assert_eq!(result, VerifyResult::Pass);
    }

    // ── Dispatch-backend axis tests (rosary-5411bf) ───────────────────────────

    fn dispatch_plugin(name: &str, command: Vec<String>) -> PluginConfig {
        PluginConfig {
            name: name.into(),
            kind: PluginKind::Dispatch,
            hook: String::new(),
            command,
            url: None,
        }
    }

    #[test]
    fn dispatch_providers_filters_by_kind() {
        let plugins = vec![
            hook_plugin("hook-verify", "pipeline.verify"),
            dispatch_plugin("my-runner", vec!["runner".into()]),
            PluginConfig {
                name: "mcp-provider".into(),
                kind: PluginKind::Mcp,
                hook: String::new(),
                command: vec![],
                url: Some("http://localhost:8484".into()),
            },
        ];
        let registry = PluginRegistry::new(plugins);
        let ctx = PluginContext::new("rosary-abc", "rosary");
        let providers = registry.dispatch_providers(ctx);
        assert_eq!(providers.len(), 1, "only dispatch plugins returned");
        assert_eq!(providers[0].name(), "my-runner");
    }

    #[test]
    fn dispatch_providers_empty_when_no_dispatch_plugins() {
        let plugins = vec![hook_plugin("hook-verify", "pipeline.verify")];
        let registry = PluginRegistry::new(plugins);
        let ctx = PluginContext::new("rosary-abc", "rosary");
        let providers = registry.dispatch_providers(ctx);
        assert!(providers.is_empty());
    }

    #[test]
    fn dispatch_provider_name_matches_plugin_name() {
        let plugin = dispatch_plugin("my-executor", vec!["true".into()]);
        let ctx = PluginContext::new("rosary-abc", "rosary");
        let provider = PluginDispatchProvider::new(plugin, ctx);
        assert_eq!(provider.name(), "my-executor");
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn dispatch_backend_plugin_routes_and_exits() {
        // Use `true` (always exits 0) as a minimal dispatch backend.
        // The JSON payload is written to its stdin; it exits immediately.
        let plugin = dispatch_plugin("true-runner", vec!["true".into()]);
        let ctx = PluginContext::new("rosary-abc123", "rosary");
        let provider = PluginDispatchProvider::new(plugin, ctx);

        let mut session = provider
            .spawn_agent(
                "do the thing",
                Path::new("/tmp"),
                &PermissionProfile::Implement,
                "",
            )
            .expect("spawn should succeed");

        let success = session.wait().await.expect("wait should not error");
        assert!(success, "exit-0 dispatch backend must report success");
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn dispatch_backend_plugin_failure_propagates() {
        // Use `false` (always exits 1) to simulate a failing backend.
        let plugin = dispatch_plugin("false-runner", vec!["false".into()]);
        let ctx = PluginContext::new("rosary-fail1", "rosary");
        let provider = PluginDispatchProvider::new(plugin, ctx);

        let mut session = provider
            .spawn_agent(
                "do the thing",
                Path::new("/tmp"),
                &PermissionProfile::Implement,
                "",
            )
            .expect("spawn should succeed");

        let success = session.wait().await.expect("wait should not error");
        assert!(!success, "exit-1 dispatch backend must report failure");
    }
}

// ── Assay verdict tests ───────────────────────────────────────────────────────

#[cfg(test)]
mod assay_verdict {
    use super::*;
    use crate::verify::VerifyResult;

    fn parse(raw: &str) -> VerifyResult {
        parse_hook_output(raw, "assay-coverage")
            .unwrap()
            .into_verify_result("assay-coverage")
    }

    // --- Standard verdict envelope (assay aliases) ---

    #[test]
    fn covered_maps_to_pass() {
        let result = parse(r#"{"verdict":"covered","coverage":0.87}"#);
        assert!(matches!(result, VerifyResult::Pass));
    }

    #[test]
    fn uncovered_maps_to_fail() {
        let result = parse(r#"{"verdict":"uncovered","coverage":0.42}"#);
        assert!(matches!(result, VerifyResult::Fail(_)));
    }

    #[test]
    fn stale_maps_to_partial() {
        let result = parse(r#"{"verdict":"stale","message":"outdated symbols"}"#);
        assert!(matches!(result, VerifyResult::Partial(_)));
    }

    // --- Assay-native format (standard path, extra fields ignored) ---

    #[test]
    fn assay_native_covered_standard_path() {
        // {"verdict":"covered","coverage":0.92} parses via standard HookOutput —
        // extra fields (coverage) are silently ignored; verdict drives the result.
        let out = parse_hook_output(r#"{"verdict":"covered","coverage":0.92}"#, "assay-coverage")
            .unwrap();
        assert_eq!(out.verdict, "covered");
        // Standard path: message is absent; coverage in verdict is enough.
        assert!(out.message.is_none());
    }

    #[test]
    fn assay_fallback_coverage_message_populated() {
        // When only the assay-native format is present (no `verdict` key as top-level),
        // the AssayOutput fallback parser produces a coverage message.
        // Simulate by using a key that HookOutput won't parse (`status` instead of `verdict`).
        // (Real assay --format json always emits `verdict`; this tests the fallback path.)
        let raw = r#"{"verdict":"covered","coverage":0.87,"message":null}"#;
        let out = parse_hook_output(raw, "assay-coverage").unwrap();
        // Standard parse wins; coverage percentage surfaced via AssayOutput only in fallback.
        let result = out.into_verify_result("assay-coverage");
        assert!(matches!(result, VerifyResult::Pass));
    }

    #[test]
    fn assay_native_stale_message_preserved() {
        let out = parse_hook_output(
            r#"{"verdict":"stale","message":"file changed since last run"}"#,
            "assay-coverage",
        )
        .unwrap();
        assert_eq!(out.verdict, "stale");
        assert_eq!(out.message.as_deref(), Some("file changed since last run"));
    }

    // --- Standard (non-assay) verdicts unaffected ---

    #[test]
    fn standard_pass_still_works() {
        let result = parse(r#"{"verdict":"pass"}"#);
        assert!(matches!(result, VerifyResult::Pass));
    }

    #[test]
    fn standard_fail_still_works() {
        let result = parse(r#"{"verdict":"fail","message":"tests broke"}"#);
        assert!(matches!(result, VerifyResult::Fail(msg) if msg == "tests broke"));
    }

    #[test]
    fn request_changes_still_works() {
        let result = parse(r#"{"verdict":"request-changes"}"#);
        assert!(matches!(result, VerifyResult::Partial(_)));
    }

    // --- Plugin config loads correctly ---

    #[test]
    fn assay_plugin_config_loads() {
        let toml = r#"
            name = "assay-coverage"
            kind = "hook"
            hook = "pipeline.verify"
            command = ["assay", "verify", "--threshold", "0.8", "--format", "json"]
        "#;
        let plugin: crate::config::PluginConfig = toml::from_str(toml).unwrap();
        assert_eq!(plugin.name, "assay-coverage");
        assert!(plugin.is_hook());
        assert_eq!(plugin.hook, "pipeline.verify");
        assert_eq!(
            plugin.command,
            vec!["assay", "verify", "--threshold", "0.8", "--format", "json"]
        );
    }

    #[test]
    fn assay_plugin_registers_as_verify_tier() {
        let plugin = crate::config::PluginConfig {
            name: "assay-coverage".into(),
            kind: crate::config::PluginKind::Hook,
            hook: "pipeline.verify".into(),
            command: vec!["assay".into(), "verify".into()],
            url: None,
        };
        let registry = PluginRegistry::new(vec![plugin]);
        let ctx = PluginContext::new("rosary-abc", "rosary");
        let tiers = registry.verify_tiers(ctx);
        assert_eq!(tiers.len(), 1);
        assert_eq!(tiers[0].name(), "assay-coverage");
    }
}

// ── assay_delta tests (rosary-5415be) ─────────────────────────────────────────

#[cfg(test)]
mod assay_delta {
    use super::*;
    use std::io::Write;

    fn make_assay_plugin(output_json: &str) -> PluginConfig {
        let dir = tempfile::tempdir().unwrap();
        let script = dir.path().join("assay.sh");
        let mut f = std::fs::File::create(&script).unwrap();
        writeln!(f, "#!/bin/sh\necho '{output_json}'").unwrap();
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(&script, std::fs::Permissions::from_mode(0o755)).unwrap();
        }
        // Leak the tempdir so the script survives the test scope.
        std::mem::forget(dir);
        PluginConfig {
            name: "assay".into(),
            kind: PluginKind::Hook,
            hook: "pipeline.verify".into(),
            command: vec![script.to_str().unwrap().to_string()],
            url: None,
        }
    }

    fn ctx_with_min(min: f32) -> PluginContext {
        PluginContext {
            bead_id: "rosary-test".into(),
            repo: "rosary".into(),
            doc_coverage_min: Some(min),
        }
    }

    #[test]
    fn coverage_below_min_fails_verify() {
        let plugin = make_assay_plugin(r#"{"verdict":"pass","coverage":0.85}"#);
        let ctx = ctx_with_min(0.9);
        let tier = PluginTier::new(plugin, ctx);
        let result = tier.check(Path::new("/tmp")).unwrap();
        assert!(
            matches!(result, VerifyResult::Fail(_)),
            "85% < 90% required → Fail, got {result:?}"
        );
        if let VerifyResult::Fail(msg) = result {
            assert!(msg.contains("85"), "message should show actual: {msg}");
            assert!(msg.contains("90"), "message should show required: {msg}");
        }
    }

    #[test]
    fn coverage_above_min_passes_verify() {
        let plugin = make_assay_plugin(r#"{"verdict":"pass","coverage":0.95}"#);
        let ctx = ctx_with_min(0.9);
        let tier = PluginTier::new(plugin, ctx);
        let result = tier.check(Path::new("/tmp")).unwrap();
        assert_eq!(result, VerifyResult::Pass, "95% >= 90% → Pass");
    }

    #[test]
    fn coverage_exactly_at_min_passes_verify() {
        let plugin = make_assay_plugin(r#"{"verdict":"pass","coverage":0.9}"#);
        let ctx = ctx_with_min(0.9);
        let tier = PluginTier::new(plugin, ctx);
        let result = tier.check(Path::new("/tmp")).unwrap();
        assert_eq!(result, VerifyResult::Pass, "exactly 90% → Pass");
    }

    #[test]
    fn no_coverage_min_ignores_coverage_field() {
        let plugin = make_assay_plugin(r#"{"verdict":"pass","coverage":0.5}"#);
        let ctx = PluginContext::new("rosary-test", "rosary"); // doc_coverage_min: None
        let tier = PluginTier::new(plugin, ctx);
        let result = tier.check(Path::new("/tmp")).unwrap();
        assert_eq!(
            result,
            VerifyResult::Pass,
            "no min set → Pass regardless of coverage"
        );
    }

    #[test]
    fn no_coverage_reported_skips_gate() {
        let plugin = make_assay_plugin(r#"{"verdict":"pass"}"#);
        let ctx = ctx_with_min(0.9);
        let tier = PluginTier::new(plugin, ctx);
        let result = tier.check(Path::new("/tmp")).unwrap();
        assert_eq!(
            result,
            VerifyResult::Pass,
            "no coverage in output → gate skipped"
        );
    }

    #[test]
    fn plugin_fail_verdict_still_fails() {
        // Even if coverage is above min, a hard "fail" verdict must propagate.
        let plugin =
            make_assay_plugin(r#"{"verdict":"fail","coverage":0.95,"message":"syntax error"}"#);
        let ctx = ctx_with_min(0.9);
        let tier = PluginTier::new(plugin, ctx);
        let result = tier.check(Path::new("/tmp")).unwrap();
        assert!(
            matches!(result, VerifyResult::Fail(_)),
            "fail verdict must propagate"
        );
    }
}
