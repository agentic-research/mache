//! Reconciliation loop — the core of rosary.
//!
//! Implements a Kubernetes-controller-style desired-state loop:
//!   scan → triage → dispatch → verify → report → sleep → repeat
//!
//! Modeled after driftlessaf's workqueue patterns and gem's tiered verification.

mod completion;
mod helpers;
mod orchestration;
mod persistence;
mod threading;
mod triage;
mod vcs;
pub(crate) mod verify;
mod workspace_ops;

use anyhow::Result;
use std::collections::HashMap;
use std::path::PathBuf;
use std::time::{Duration, Instant};

use crate::config::{self, OrchestrationConfig, RepoConfig};
use crate::dispatch::{self, AgentHandle};
use crate::epic;
use crate::orchestrate::FeatureOrchestrator;
use crate::pipeline::PipelineEngine;
use crate::queue::WorkQueue;
use crate::scanner;
#[allow(unused_imports)]
use crate::store::BeadStore;
use crate::store::{DispatchRecord, WorkRef};
use crate::sync::IssueTracker;
use crate::xref;

/// Configuration for the reconciliation loop.
pub struct ReconcilerConfig {
    pub max_concurrent: usize,
    pub scan_interval: Duration,
    pub max_retries: u32,
    pub triage_threshold: f64,
    pub repo: Vec<RepoConfig>,
    pub once: bool,
    pub dry_run: bool,
    /// AI provider name (e.g. "claude", "gemini"). Default "claude".
    pub provider: String,
    /// Overnight mode: prefer small/mechanical beads agents can complete.
    pub overnight: bool,
    /// Compute provider config (from [compute] in rosary.toml).
    pub compute: Option<crate::config::ComputeConfig>,
    /// Backend storage for orchestrator state (decades, threads, pipeline).
    pub backend: Option<crate::config::BackendConfig>,
    /// Target a specific bead — skip triage, only dispatch this one.
    pub target_bead: Option<String>,
    /// Pipeline definitions: issue_type → agent sequence (from config).
    pub pipelines: HashMap<String, Vec<String>>,
    /// Maximum pipeline stages per bead. 0 = unlimited.
    pub max_pipeline_depth: usize,
    /// Scope remote repo loading to a specific user.
    ///
    /// - `None` — local/self-hosted mode: loads repos for all users (or none if no backend).
    /// - `Some(uid)` — hosted/federated mode: loads only repos registered by this user,
    ///   preventing cross-user dispatch.
    pub user_id: Option<String>,
    /// Default base branch for agent PRs when no thread feature branch is found.
    /// Populated from `[github] base` in config. Defaults to "main".
    pub default_branch: String,
    /// Orchestration config: "flat" (default) or "hierarchical".
    /// When hierarchical, beads are managed by FeatureOrchestrators instead
    /// of raw agent dispatches.
    pub orchestration: OrchestrationConfig,
    /// Pipeline plugins from [[plugins]] config.
    /// Each plugin hooks into verify/review/triage stages via subprocess or HTTP.
    pub plugins: Vec<crate::config::PluginConfig>,
    /// APAS L2: optional Ed25519 signing key for handoff envelopes.
    /// When set, every handoff write produces a sibling DSSE envelope.
    pub attestation: Option<crate::config::AttestationConfig>,
    /// When true, dispatch is gated by `RepoConfig.approval`. Beads from
    /// repos lacking `Approved` status are held by triage. `--bead` overrides.
    pub require_approval: bool,
}

impl Default for ReconcilerConfig {
    fn default() -> Self {
        ReconcilerConfig {
            max_concurrent: 5,
            scan_interval: Duration::from_secs(30),
            max_retries: 5,
            triage_threshold: 0.3,
            repo: Vec::new(),
            once: false,
            dry_run: false,
            provider: "claude".to_string(),
            overnight: false,
            compute: None,
            backend: None,
            target_bead: None,
            pipelines: crate::config::default_pipelines(),
            max_pipeline_depth: 0,
            user_id: None,
            default_branch: "main".to_string(),
            orchestration: OrchestrationConfig::default(),
            plugins: Vec::new(),
            attestation: None,
            require_approval: false,
        }
    }
}

/// Tracks state of a bead across loop iterations.
#[derive(Debug)]
struct BeadTracker {
    repo: String,
    /// Team/folder scope (mirrors WorkRef.scope). Captured at dispatch time.
    scope: String,
    last_generation: u64,
    retries: u32,
    consecutive_reverts: u32,
    highest_tier: Option<usize>,
    /// Current agent name (e.g. "dev-agent"). Set on dispatch, used for handoffs.
    current_agent: Option<String>,
    /// Current pipeline phase index (0-based). Advances on phase completion.
    phase_index: u32,
    /// Bead's issue type (e.g. "bug", "task"). Captured at dispatch time so
    /// wait_and_verify can determine pipeline advancement without a fresh scan.
    issue_type: String,
    /// Backend dispatch record ID for this active dispatch (format: "{bead_id}-{started_at_millis}").
    /// Set on dispatch, used to call complete_dispatch when the agent finishes.
    dispatch_id: Option<String>,
}

/// The reconciliation loop orchestrator.
pub struct Reconciler {
    config: ReconcilerConfig,
    queue: WorkQueue,
    active: HashMap<String, AgentHandle>,
    trackers: HashMap<String, BeadTracker>,
    /// Map repo name → (path, lang) for verification
    repo_info: HashMap<String, (PathBuf, String)>,
    /// Stash work_dir + repo when agent completes so verify_agent can find it
    completed_work_dirs: HashMap<String, (PathBuf, String)>,
    /// Stash workspaces from completed agents for checkpoint + teardown
    completed_workspaces: HashMap<String, crate::workspace::Workspace>,
    /// Bead stores keyed by repo name, lazily connected
    dolt_clients: HashMap<String, Box<dyn crate::store::BeadStore>>,
    /// Resolved AI agent provider (claude, gemini, etc).
    provider: Box<dyn dispatch::AgentProvider>,
    /// Optional external issue tracker (Linear, etc.) for status mirroring.
    /// When set, persist_status also pushes state transitions to the tracker.
    issue_tracker: Option<Box<dyn IssueTracker>>,
    /// Compute provider for workspace provisioning (local, sprites, etc).
    compute: Box<dyn crate::backend::ComputeProvider>,
    /// Path to agent definitions directory (from self_managed repo).
    agents_dir: Option<PathBuf>,
    /// Hierarchy store (decades, threads, bead membership).
    /// When set, enables thread-aware dedup and phase context.
    #[allow(dead_code)] // Wired in next phase: thread-aware triage
    hierarchy: Option<Box<dyn crate::store::HierarchyStore>>,
    /// Cross-repo linkage store. When set, triage consults blocking deps (ADR-0009).
    linkage: Option<Box<dyn crate::store::LinkageStore>>,
    /// Schema-driven pipeline engine. Replaces hardcoded agent_pipeline() match.
    /// Uses DispatchStore for persistent pipeline state (survives crashes).
    pipeline: PipelineEngine,
    /// Remote repos registered via rsry_repo_register, successfully cloned.
    /// Merged with config.repo for scanning on each iterate() pass.
    remote_repos: Vec<crate::config::RepoConfig>,
    /// Remote repo URLs whose initial clone failed — retried each iterate() pass.
    pending_remote_urls: Vec<String>,
    /// On-demand clone cache for remote repos (wasteland mode).
    repo_cache: std::sync::Arc<crate::repo_cache::RepoCache>,
    /// Feature orchestrators keyed by bead_id (hierarchical mode only).
    /// Each orchestrator owns one bead's full lifecycle: research → synthesis →
    /// implementation → verification, with mid-flight messaging and session reuse.
    orchestrators: HashMap<String, FeatureOrchestrator>,
    /// Plugin hooks wired into verify/review/triage from [[plugins]] config.
    plugin_registry: crate::plugin::PluginRegistry,
    /// Per-agent model overrides from [dispatch.pipeline_models] config.
    /// Maps agent name (e.g. "scoping-agent") to model string (e.g. "claude-haiku-4-5-20251001").
    pipeline_models: HashMap<String, String>,
    /// Tool call records captured from ACP sessions, keyed by bead_id.
    /// Populated when an agent completes; consumed by checkpoint_and_cleanup.
    pending_tools_used: HashMap<String, Vec<crate::handoff::ToolCallRecord>>,
}

/// Summary of a single reconciliation iteration.
#[derive(Debug, Default)]
pub struct IterationSummary {
    pub scanned: usize,
    pub triaged: usize,
    pub dispatched: usize,
    pub completed: usize,
    pub passed: usize,
    pub failed: usize,
    pub deadlettered: usize,
    /// IDs of beads moved to `dead_letter` THIS iteration. Used by
    /// target-bead mode to distinguish "this run's target bead deadlettered"
    /// (operator should be told and the loop should exit) from "an unrelated
    /// bead deadlettered" (observed via `deadlettered` counter, no exit).
    /// Without this split, the liveness sweep deadlettering a sibling bead
    /// would falsely trip target-bead-mode exit.
    pub deadlettered_ids: Vec<String>,
    /// Beads closed by the agent via MCP (skipped verification).
    pub agent_closed: usize,
    /// Beads transitioned by VCS commit references.
    pub vcs_transitions: usize,
}

impl std::fmt::Display for IterationSummary {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "scanned={} vcs={} triaged={} dispatched={} completed={} passed={} failed={} deadlettered={} agent_closed={}",
            self.scanned,
            self.vcs_transitions,
            self.triaged,
            self.dispatched,
            self.completed,
            self.passed,
            self.failed,
            self.deadlettered,
            self.agent_closed
        )
    }
}

impl Reconciler {
    pub async fn new(config: ReconcilerConfig) -> Self {
        let mut repo_info = HashMap::new();

        // Build repo info map from config
        for repo in &config.repo {
            let path = scanner::expand_path(&repo.path);
            let lang = repo
                .lang
                .clone()
                .unwrap_or_else(|| helpers::detect_language(&path));
            repo_info.insert(repo.name.clone(), (path, lang));
        }

        let global_dispatch = crate::config::load_global().ok().and_then(|c| c.dispatch);
        let binaries = global_dispatch
            .as_ref()
            .map(|d| d.binaries.clone())
            .unwrap_or_default();
        let pipeline_models = global_dispatch
            .map(|d| d.pipeline_models)
            .unwrap_or_default();
        let provider =
            dispatch::provider_by_name(&config.provider, &binaries).unwrap_or_else(|e| {
                eprintln!("[reconcile] {e}, falling back to claude");
                dispatch::provider_by_name("claude", &binaries).unwrap()
            });

        // Build compute provider from config, fall back to local on failure.
        let compute: Box<dyn crate::backend::ComputeProvider> = {
            // Temporarily build a Config with just the compute field for the factory.
            let tmp_cfg = crate::config::Config {
                compute: config.compute.clone(),
                ..Default::default()
            };
            match crate::config::compute_provider_from_config(&tmp_cfg) {
                Ok(p) => p,
                Err(e) => {
                    eprintln!("[reconcile] compute provider failed ({e}), using local");
                    Box::new(crate::backend::LocalProvider)
                }
            }
        };

        // Discover agents_dir from self-managed repo
        let agents_dir = config
            .repo
            .iter()
            .find(|r| r.self_managed)
            .map(|r| scanner::expand_path(&r.path).join("agents"))
            .filter(|p| p.exists());

        if let Some(ref dir) = agents_dir {
            eprintln!("[reconcile] agents_dir: {}", dir.display());
        } else {
            eprintln!(
                "[reconcile] warning: no agents_dir found (no self-managed repo with agents/)"
            );
        }

        // Connect backend if config is present. Two separate instances —
        // Reconciler owns HierarchyStore, PipelineEngine owns DispatchStore.
        // TODO: refactor to Arc<dyn BackendStore> to share a single connection.
        #[allow(clippy::type_complexity)]
        let (hierarchy, dispatch_store, linkage): (
            Option<Box<dyn crate::store::HierarchyStore>>,
            Option<Box<dyn crate::store::DispatchStore>>,
            Option<Box<dyn crate::store::LinkageStore>>,
        ) = if let Some(ref backend_cfg) = config.backend {
            let hierarchy = match backend_cfg.connect().await {
                Ok(backend) => {
                    eprintln!(
                        "[reconcile] hierarchy store connected ({})",
                        backend_cfg.provider
                    );
                    Some(backend as Box<dyn crate::store::HierarchyStore>)
                }
                Err(e) => {
                    eprintln!(
                        "[reconcile] hierarchy store unavailable ({e}), \
                         thread-aware features disabled"
                    );
                    None
                }
            };
            let dispatch = match backend_cfg.connect().await {
                Ok(backend) => {
                    eprintln!(
                        "[reconcile] dispatch store connected ({})",
                        backend_cfg.provider
                    );
                    Some(backend as Box<dyn crate::store::DispatchStore>)
                }
                Err(e) => {
                    eprintln!("[reconcile] dispatch store unavailable ({e})");
                    None
                }
            };
            let linkage = match backend_cfg.connect().await {
                Ok(backend) => Some(backend as Box<dyn crate::store::LinkageStore>),
                Err(e) => {
                    eprintln!(
                        "[reconcile] linkage store unavailable ({e}), cross-repo blocking disabled"
                    );
                    None
                }
            };
            (hierarchy, dispatch, linkage)
        } else {
            eprintln!("[reconcile] no [backend] config, backend stores disabled");
            (None, None, None)
        };

        // Build pipeline engine from config + backend store.
        let pipeline = PipelineEngine::new(
            config.pipelines.clone(),
            dispatch_store,
            config.max_pipeline_depth,
        );

        // Load remote repos registered via rsry_repo_register.
        // Clones them on demand so the reconciler can scan and dispatch against them.
        let repo_cache = std::sync::Arc::new(crate::repo_cache::RepoCache::new());
        let mut remote_repos = Vec::new();
        let mut pending_remote_urls = Vec::new();

        if let Some(ref backend_cfg) = config.backend {
            match backend_cfg.connect_exportable().await {
                Ok(backend) => {
                    // Scoped to a specific user in hosted/federated mode; all users in local mode.
                    let user_repos_result = match config.user_id.as_deref() {
                        Some(uid) => backend.list_user_repos(uid).await,
                        None => backend.all_user_repos().await,
                    };
                    match user_repos_result {
                        Ok(user_repos) => {
                            for user_repo in user_repos {
                                // Skip repos already in config by name (exact match).
                                if repo_info.contains_key(&user_repo.repo_name) {
                                    continue;
                                }
                                // Reject insecure http:// URLs — credentials would transit plaintext.
                                if user_repo.repo_url.starts_with("http://") {
                                    eprintln!(
                                        "[reconcile] skipping remote repo {} — insecure http:// URL",
                                        user_repo.repo_name
                                    );
                                    continue;
                                }
                                match repo_cache.ensure_local(&user_repo.repo_url, None).await {
                                    Ok(local_path) => {
                                        let lang = helpers::detect_language(&local_path);
                                        eprintln!(
                                            "[reconcile] remote repo {} cloned to {}",
                                            user_repo.repo_name,
                                            local_path.display()
                                        );
                                        repo_info.insert(
                                            user_repo.repo_name.clone(),
                                            (local_path.clone(), lang.clone()),
                                        );
                                        remote_repos.push(crate::config::RepoConfig {
                                            name: user_repo.repo_name,
                                            path: local_path,
                                            lang: Some(lang),
                                            self_managed: false,
                                            approval: crate::config::DispatchApproval::Approved,
                                        });
                                    }
                                    Err(e) => {
                                        eprintln!(
                                            "[reconcile] remote repo {} clone failed (will retry): {e}",
                                            user_repo.repo_name
                                        );
                                        // Keep URL so iterate() can retry on next pass.
                                        pending_remote_urls.push(user_repo.repo_url);
                                    }
                                }
                            }
                        }
                        Err(e) => {
                            eprintln!("[reconcile] could not load registered repos: {e}");
                        }
                    }
                }
                Err(e) => {
                    eprintln!("[reconcile] backend unavailable for repo loading: {e}");
                }
            }
        }

        let plugin_registry = crate::plugin::PluginRegistry::new(config.plugins.clone());
        if !plugin_registry.is_empty() {
            eprintln!(
                "[reconcile] {} pipeline plugin(s) loaded",
                config.plugins.len()
            );
        }

        Reconciler {
            config,
            queue: WorkQueue::new(),
            active: HashMap::new(),
            trackers: HashMap::new(),
            repo_info,
            completed_work_dirs: HashMap::new(),
            completed_workspaces: HashMap::new(),
            dolt_clients: HashMap::new(),
            provider,
            issue_tracker: None,
            compute,
            agents_dir,
            hierarchy,
            linkage,
            pipeline,
            remote_repos,
            pending_remote_urls,
            repo_cache,
            orchestrators: HashMap::new(),
            plugin_registry,
            pipeline_models,
            pending_tools_used: HashMap::new(),
        }
    }

    /// Attach an external issue tracker for status mirroring.
    /// When set, every bead state transition also updates the linked Linear issue.
    #[allow(dead_code)] // API surface — called from main.rs when LINEAR_API_KEY is set
    pub fn set_issue_tracker(&mut self, tracker: Box<dyn IssueTracker>) {
        self.issue_tracker = Some(tracker);
    }

    /// Run the reconciliation loop. Returns the last iteration summary.
    pub async fn run(&mut self) -> Result<IterationSummary> {
        if let Some(ref target) = self.config.target_bead {
            eprintln!("[reconcile] targeting bead {target} (pipeline until terminal/deadletter)");
        } else {
            eprintln!(
                "Reconciler started: max_concurrent={}, interval={}s, dry_run={}",
                self.config.max_concurrent,
                self.config.scan_interval.as_secs(),
                self.config.dry_run,
            );
        }

        // Recover orchestrators first — they claim their beads before the
        // stuck-bead sweep runs, preventing accidental resets.
        self.recover_orchestrators().await;

        // Recover beads stuck at 'dispatched' from previous crashed run
        self.recover_stuck_beads().await;

        // Sweep orphaned workspaces from previous runs
        let repo_paths: Vec<PathBuf> = self.repo_info.values().map(|(p, _)| p.clone()).collect();
        let active_ids: Vec<String> = self.active.keys().cloned().collect();
        crate::workspace::sweep_orphaned(&repo_paths, &active_ids);

        let start = Instant::now();
        let mut cumulative = IterationSummary::default();
        loop {
            let summary = self.iterate().await?;
            eprintln!("[reconcile] {summary}");
            cumulative.scanned = summary.scanned; // latest scan count
            cumulative.triaged += summary.triaged;
            cumulative.dispatched += summary.dispatched;
            cumulative.completed += summary.completed;
            cumulative.passed += summary.passed;
            cumulative.failed += summary.failed;
            cumulative.deadlettered += summary.deadlettered;
            // Only accumulate IDs in target-bead mode — that's the only
            // caller that needs set-membership for exit. In daemon mode
            // (`once == false`, no `target_bead`) this Vec would grow
            // unbounded across iterations otherwise. Per-iteration
            // observability is still preserved via the counter above.
            if self.config.target_bead.is_some() {
                cumulative
                    .deadlettered_ids
                    .extend(summary.deadlettered_ids.iter().cloned());
            }

            if self.config.once {
                if !self.active.is_empty() {
                    eprintln!(
                        "[reconcile] waiting for {} active agent(s)...",
                        self.active.len()
                    );
                    self.wait_and_verify().await?;
                }

                // When targeting a specific bead, keep looping until it
                // reaches a terminal state. Check if the bead is still
                // retriable (in backoff queue or was just dispatched).
                if let Some(ref target) = self.config.target_bead {
                    // Exit on terminal outcomes. Check specifically whether
                    // the TARGET bead was dead-lettered — not the counter,
                    // which now includes liveness-sweep deadletters for
                    // unrelated beads (would false-trip otherwise).
                    if cumulative.deadlettered_ids.iter().any(|id| id == target) {
                        eprintln!(
                            "[reconcile] bead {target} deadlettered after {} retries",
                            cumulative.failed
                        );
                        break;
                    }
                    if cumulative.passed > 0 {
                        eprintln!("[reconcile] bead {target} completed pipeline");
                        break;
                    }

                    let target_repo = self
                        .trackers
                        .get(target.as_str())
                        .map(|t| t.repo.clone())
                        .unwrap_or_default();
                    let still_active = summary.dispatched > 0
                        || self.queue.has_backoff(&target_repo, target)
                        || !self.active.is_empty();

                    if still_active {
                        let elapsed = start.elapsed();
                        let reason = if self.queue.has_backoff(&target_repo, target) {
                            "waiting for backoff"
                        } else {
                            "retry pass"
                        };
                        eprintln!(
                            "[reconcile] {reason} ({:.0}s elapsed)",
                            elapsed.as_secs_f64()
                        );
                        // Wait for backoff to expire before next scan
                        tokio::time::sleep(self.config.scan_interval).await;
                        continue;
                    }
                }

                let elapsed = start.elapsed();
                eprintln!("[reconcile] done ({:.0}s elapsed)", elapsed.as_secs_f64());
                break;
            }

            tokio::time::sleep(self.config.scan_interval).await;
        }

        Ok(cumulative)
    }

    /// Query the linkage store for beads blocked by cross-repo deps (ADR-0009).
    ///
    /// Returns a set of bead IDs that should be deferred by triage.
    /// Uses the already-scanned `beads` slice to resolve target status — no extra DB round-trips.
    /// Fails closed: if the linkage store is unavailable, returns empty (no extra blocking).
    async fn compute_cross_repo_blocked(
        &self,
        beads: &[crate::bead::Bead],
    ) -> std::collections::HashSet<String> {
        let Some(ref linkage) = self.linkage else {
            return Default::default();
        };
        let all_blocking = match linkage.all_blocking_deps().await {
            Ok(deps) => deps,
            Err(e) => {
                eprintln!("[triage] cross-repo dep query failed: {e}");
                return Default::default();
            }
        };
        if all_blocking.is_empty() {
            return Default::default();
        }
        // Build a status map from scanned beads for fast lookup.
        let status_map: std::collections::HashMap<(&str, &str), &str> = beads
            .iter()
            .map(|b| ((b.repo.as_str(), b.id.as_str()), b.status.as_str()))
            .collect();
        let mut blocked = std::collections::HashSet::new();
        for dep in &all_blocking {
            let target_status = status_map
                .get(&(dep.to.repo.as_str(), dep.to.bead_id.as_str()))
                .copied()
                .unwrap_or("open"); // unknown repo = assume open (fail closed)
            if target_status != "done" && target_status != "closed" {
                blocked.insert(dep.from.bead_id.clone());
                eprintln!(
                    "[triage] cross-repo block: {}/{} ← {}/{} ({target_status}, tier={})",
                    dep.from.repo, dep.from.bead_id, dep.to.repo, dep.to.bead_id, dep.evidence_tier
                );
            }
        }
        blocked
    }

    /// Execute one full iteration of the reconciliation loop.
    pub async fn iterate(&mut self) -> Result<IterationSummary> {
        let mut summary = IterationSummary::default();

        // Phase 0.5: RETRY PENDING — attempt clones that failed at startup.
        if !self.pending_remote_urls.is_empty() {
            let mut still_pending = Vec::new();
            for url in std::mem::take(&mut self.pending_remote_urls) {
                match self.repo_cache.ensure_local(&url, None).await {
                    Ok(local_path) => {
                        let repo_name = url
                            .trim_end_matches('/')
                            .trim_end_matches(".git")
                            .rsplit('/')
                            .next()
                            .unwrap_or("repo")
                            .to_string();
                        let lang = helpers::detect_language(&local_path);
                        eprintln!("[reconcile] remote repo {repo_name} cloned on retry");
                        self.repo_info
                            .insert(repo_name.clone(), (local_path.clone(), lang.clone()));
                        self.remote_repos.push(crate::config::RepoConfig {
                            name: repo_name,
                            path: local_path,
                            lang: Some(lang),
                            self_managed: false,
                            approval: crate::config::DispatchApproval::Approved,
                        });
                    }
                    Err(_) => still_pending.push(url),
                }
            }
            self.pending_remote_urls = still_pending;
        }

        // Phase 1: SCAN — config repos + remote repos registered via rsry_repo_register
        let all_repos: Vec<_> = self
            .config
            .repo
            .iter()
            .chain(self.remote_repos.iter())
            .cloned()
            .collect();
        let beads = scanner::scan_repos(&all_repos).await?;
        summary.scanned = beads.len();

        // Phase 1.5: VCS SCAN — detect bead refs in recent jj commits
        summary.vcs_transitions = self.scan_vcs(&beads).await;

        // Phase 1.6: PR STATUS POLL — surface review feedback + close merged PRs
        self.poll_pr_status(&beads).await;

        // Phase 1.75: CROSS-REPO SYNC — propagate external refs across repos
        let ext_refs = xref::find_external_refs(&beads);
        if !ext_refs.is_empty() {
            xref::sync_external_refs(&ext_refs, &self.dolt_clients, &beads).await;
        }

        // Phase 1.8: LIVENESS — catch dispatched workers whose process died.
        // Without this, beads that were spawned by `rsry_dispatch` and whose
        // worker has since crashed stay stuck at `dispatched` forever. The
        // sweep moves them to `dead_letter` for operator triage (with
        // forensic context preserved). Per-iteration so the failure mode is
        // caught while the operator is still around, not just at next
        // cold-start.
        let live_sessions = match crate::session::SessionRegistry::load() {
            Ok(r) => r.active().to_vec(),
            Err(e) => {
                // Loud failure: silent fallback to "no sessions" disables
                // the liveness sweep without any signal, and dispatched
                // beads silently accumulate the very state this is meant
                // to clean up. Operators need to know why the sweep is a
                // no-op so they can fix `~/.rsry/sessions.json` perms /
                // corruption.
                eprintln!(
                    "[reconcile] liveness sweep DISABLED — could not load \
                     SessionRegistry: {e}. Dispatched beads will not move \
                     to dead_letter until this is resolved (check \
                     ~/.rsry/sessions.json perms/format)."
                );
                Vec::new()
            }
        };
        let liveness_ids = self.liveness_sweep(&beads, &live_sessions).await;
        summary.deadlettered += liveness_ids.len();
        summary.deadlettered_ids.extend(liveness_ids);

        // Phase 1.9: AUTO-ASSIGN — set owner on beads without one
        // Uses pipeline engine (config-driven) not dispatch::default_agent (hardcoded).
        for bead in &beads {
            if bead.owner.is_some() || bead.status == "closed" || bead.status == "done" {
                continue;
            }
            let agent = self.pipeline.default_agent(&bead.issue_type);
            if let Some(client) = self.dolt_client(&bead.repo).await {
                if let Err(e) = client.set_assignee(&bead.id, &agent).await {
                    eprintln!("[assign] failed for {}: {e}", bead.id);
                } else {
                    eprintln!(
                        "[assign] {} → {agent} (issue_type={})",
                        bead.id, bead.issue_type
                    );
                }
            }
        }

        // Phase 2: CHECK COMPLETED — poll active agents
        let completed = self.check_completed();
        summary.completed = completed.len();

        // Phase 2.1: ORCHESTRATOR TICK — advance orchestrator state machines.
        // Only runs in hierarchical mode. Orchestrators may spawn agents
        // (which go into self.active) or complete/fail their beads.
        if self.is_hierarchical() && !self.orchestrators.is_empty() {
            let orch_result = self.orchestrator_tick(&beads).await;
            summary.dispatched += orch_result.dispatched;
            summary.passed += orch_result.passed;
            summary.failed += orch_result.failed;
            summary.deadlettered += orch_result.deadlettered;
        }

        // Phase 2.5 + 2.75: AUTO-THREAD + BUILD THREAD MAP
        self.auto_thread(&beads).await;
        let thread_map = self.build_thread_map(&beads).await;

        // Phase 2.9: CROSS-REPO BLOCKING — query linkage store for blocking deps
        let cross_repo_blocked = self.compute_cross_repo_blocked(&beads).await;

        // Phase 3: TRIAGE — score open beads, enqueue above threshold
        summary.triaged = self.triage(&beads, &thread_map, &cross_repo_blocked);

        // Phase 4: DISPATCH — fill free slots before verification
        // Dispatch runs BEFORE verify so new agents start working while the
        // reconciler spends time on verification (compile+test+lint = minutes).
        // This keeps all concurrency slots utilized instead of idling during verify.
        let dispatch_now = Instant::now();
        while self.active.len() < self.config.max_concurrent {
            let Some(entry) = self.queue.dequeue(dispatch_now) else {
                break;
            };

            if self.config.dry_run {
                eprintln!(
                    "[dry-run] would dispatch {} (score={:.3}, retries={})",
                    entry.bead_id, entry.score, entry.retries
                );
                summary.dispatched += 1;
                continue;
            }

            // Find the bead and repo path for dispatch
            let bead = beads.iter().find(|b| b.id == entry.bead_id);
            let repo_path = self.repo_info.get(&entry.repo).map(|(p, _)| p.clone());

            if let (Some(bead), Some(path)) = (bead, repo_path) {
                // Re-check file overlap against beads dispatched earlier in this loop.
                // Triage couldn't catch these because they were queued simultaneously.
                let active_beads: Vec<&crate::bead::Bead> = beads
                    .iter()
                    .filter(|b| self.active.contains_key(&b.id))
                    .collect();
                if let Some(blocker) = epic::has_file_overlap(bead, &active_beads) {
                    eprintln!(
                        "[file-overlap] deferring {} — files conflict with just-dispatched {blocker}",
                        entry.bead_id
                    );
                    continue;
                }

                // Ensure bead.owner matches pipeline's first agent.
                // Beads created before pipeline changes may have stale owner.
                let mut dispatch_bead = bead.clone();
                let pipeline_agent = self.pipeline.default_agent(&bead.issue_type);
                if dispatch_bead.owner.as_deref() != Some(&pipeline_agent) {
                    dispatch_bead.owner = Some(pipeline_agent.clone());
                    // Update in Dolt so future scans see the correct owner
                    if let Some(client) = self.dolt_client(&entry.repo).await {
                        let _ = client.set_assignee(&bead.id, &pipeline_agent).await;
                    }
                }

                // ── Hierarchical mode: create orchestrator instead of raw dispatch ──
                if self.is_hierarchical() {
                    self.create_orchestrator(
                        &entry.bead_id,
                        &entry.repo,
                        &bead.scope,
                        &bead.issue_type,
                        path.clone(),
                    );
                    self.persist_status(&entry.bead_id, &entry.repo, "dispatched")
                        .await;
                    summary.dispatched += 1;
                    // Orchestrator will request its first agent spawn on next tick.
                    continue;
                }

                // ── Flat mode: direct agent dispatch ────────────────────────────────
                match dispatch::spawn(
                    &dispatch_bead,
                    &path,
                    true,
                    entry.generation,
                    self.provider.as_ref(),
                    self.agents_dir.as_deref(),
                    None, // compute: local subprocess (default)
                    self.pipeline_models
                        .get(dispatch_bead.owner.as_deref().unwrap_or(""))
                        .cloned(),
                )
                .await
                {
                    Ok(handle) => {
                        let agent_label = bead.owner.as_deref().unwrap_or("generic");
                        eprintln!(
                            "[dispatch] {} (gen={}, retries={}, provider={}, agent={})",
                            entry.bead_id,
                            entry.generation,
                            entry.retries,
                            self.provider.name(),
                            agent_label,
                        );
                        // Audit FIRST: observations and events must precede the state transition.
                        // If the process crashes, the audit trail must already exist.
                        self.append_observation(
                            &entry.bead_id,
                            &entry.repo,
                            agent_label,
                            0,
                            crate::dolt::observations::Verdict::Dispatched,
                            "agent dispatched",
                        )
                        .await;

                        // Record the dispatch branch and workspace path
                        let branch = format!("fix/{}", entry.bead_id);
                        if let Some(client) = self.dolt_client(&entry.repo).await {
                            client
                                .log_event(&entry.bead_id, "dispatch_branch", &branch)
                                .await;
                            // Record workspace_path so agents can resume in the same workspace
                            if let Some(ref ws_path) = handle.workspace_path {
                                client
                                    .log_event(&entry.bead_id, "workspace_path", ws_path)
                                    .await;
                            }
                            // Record HEAD SHA at dispatch time (APAS chain integrity, L1 anchor)
                            if let Some(ref sha) = handle.chain_hash {
                                client.log_event(&entry.bead_id, "chain_hash", sha).await;
                            }
                        }

                        // State transition LAST: after all audit events are persisted.
                        self.persist_status(&entry.bead_id, &entry.repo, "dispatched")
                            .await;

                        // Unique per-dispatch ID: bead_id + started_at millis (generation
                        // is a content hash that doesn't change on retry, so it's not unique).
                        let dispatch_id =
                            format!("{}-{}", entry.bead_id, handle.started_at.timestamp_millis());
                        // Record dispatch to backend store (captures chain_hash + workspace).
                        // Use dispatch_bead.owner (may differ from bead.owner if the pipeline
                        // corrected a stale assignee above).
                        let dispatch_record = DispatchRecord {
                            id: dispatch_id.clone(),
                            bead_ref: WorkRef {
                                repo: entry.repo.clone(),
                                scope: String::new(),
                                bead_id: entry.bead_id.clone(),
                            },
                            agent: dispatch_bead
                                .owner
                                .as_deref()
                                .unwrap_or("generic")
                                .to_string(),
                            provider: self.provider.name().to_string(),
                            started_at: handle.started_at,
                            completed_at: None,
                            outcome: None,
                            work_dir: handle.work_dir.display().to_string(),
                            session_id: None,
                            workspace_path: handle.workspace_path.clone(),
                            chain_hash: handle.chain_hash.clone(),
                        };
                        self.pipeline.record_dispatch(&dispatch_record).await;

                        self.active.insert(entry.bead_id.clone(), handle);
                        let agent_name = bead.owner.clone();
                        let tracker =
                            self.trackers
                                .entry(entry.bead_id.clone())
                                .or_insert(BeadTracker {
                                    repo: entry.repo.clone(),
                                    scope: bead.scope.clone(),
                                    last_generation: entry.generation,
                                    retries: entry.retries,
                                    consecutive_reverts: 0,
                                    highest_tier: None,
                                    current_agent: None,
                                    phase_index: 0,
                                    issue_type: bead.issue_type.clone(),
                                    dispatch_id: None,
                                });
                        tracker.last_generation = entry.generation;
                        tracker.repo = entry.repo.clone();
                        tracker.scope = bead.scope.clone();
                        tracker.current_agent = agent_name;
                        tracker.issue_type = bead.issue_type.clone();
                        tracker.dispatch_id = Some(dispatch_id);

                        // Persist initial pipeline state to backend store
                        let bead_ref = WorkRef {
                            repo: entry.repo.clone(),
                            scope: bead.scope.clone(),
                            bead_id: entry.bead_id.clone(),
                        };
                        let pipeline_state =
                            self.pipeline.initial_state(bead_ref, &bead.issue_type);
                        self.pipeline.upsert_state(&pipeline_state).await;

                        summary.dispatched += 1;
                    }
                    Err(e) => {
                        eprintln!("[dispatch] failed for {}: {e}", entry.bead_id);
                    }
                }
            }
        }

        // Phase 5: VERIFY completed agents + pipeline decisions
        let vr = self.verify_completed(&completed, &beads, &thread_map).await;
        summary.passed += vr.passed;
        summary.failed += vr.failed;
        summary.deadlettered += vr.deadlettered;
        summary.agent_closed += vr.agent_closed;

        // Phase 5.5: FEATURE ASSEMBLY — when the last bead in a thread reaches
        // pr_open, open the feature branch PR (feature/{thread} → default_branch).
        // Dev PRs from this iteration that just landed are the trigger set.
        let newly_pr_open: Vec<(String, String)> = vr
            .status_updates
            .iter()
            .filter(|(_, _, status)| status == "pr_open")
            .map(|(bead_id, repo, _)| (bead_id.clone(), repo.clone()))
            .collect();
        if !newly_pr_open.is_empty() {
            self.assemble_feature_prs(&newly_pr_open).await;
        }

        // Phase 6: PERSIST orchestrator state for crash recovery
        if self.is_hierarchical() && !self.orchestrators.is_empty() {
            self.persist_orchestrator_records();
        }

        // Phase 6: PERSIST orchestrator state for crash recovery
        if self.is_hierarchical() && !self.orchestrators.is_empty() {
            self.persist_orchestrator_records();
        }

        Ok(summary)
    }
}

/// Entry point for `rsry run`.
#[allow(clippy::too_many_arguments)]
pub async fn run(
    config_path: &str,
    concurrency: usize,
    interval: u64,
    once: bool,
    dry_run: bool,
    provider: &str,
    overnight: bool,
    target_bead: Option<&str>,
) -> Result<()> {
    let cfg = config::load(config_path)?;

    // Extract linear config before cfg.repo is moved
    let linear_team = std::env::var("LINEAR_TEAM").unwrap_or_else(|_| {
        cfg.linear
            .as_ref()
            .map(|l| l.team.clone())
            .unwrap_or_else(|| "AGE".to_string())
    });
    let linear_state_overrides = cfg
        .linear
        .as_ref()
        .map(|l| l.states.clone())
        .unwrap_or_default();

    let orchestration = cfg.orchestration.clone().unwrap_or_default();
    let reconciler_config = ReconcilerConfig {
        max_concurrent: concurrency,
        scan_interval: Duration::from_secs(interval),
        repo: cfg.repo,
        once,
        dry_run,
        provider: provider.to_string(),
        overnight,
        compute: cfg.compute,
        backend: cfg.backend,
        target_bead: target_bead.map(|s| s.to_string()),
        pipelines: cfg.pipelines,
        max_pipeline_depth: cfg.max_pipeline_depth,
        default_branch: cfg
            .github
            .as_ref()
            .map(|g| g.base.clone())
            .unwrap_or_else(|| "main".to_string()),
        orchestration,
        plugins: cfg.plugins,
        attestation: cfg.attestation,
        require_approval: cfg.dispatch.as_ref().is_some_and(|d| d.require_approval),
        ..Default::default()
    };

    let mut reconciler = Reconciler::new(reconciler_config).await;
    if let Ok(api_key) = std::env::var("LINEAR_API_KEY") {
        match crate::linear_tracker::LinearTracker::with_overrides(
            &api_key,
            &linear_team,
            linear_state_overrides,
        )
        .await
        {
            Ok(tracker) => {
                eprintln!("[linear] attached tracker for team {linear_team}");
                reconciler.set_issue_tracker(Box::new(tracker));
            }
            Err(e) => {
                eprintln!("[linear] failed to attach tracker: {e} (continuing without)");
            }
        }
    }

    reconciler.run().await?;
    Ok(())
}

#[cfg(test)]
mod tests;
