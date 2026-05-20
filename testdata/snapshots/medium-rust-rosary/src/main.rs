#![recursion_limit = "256"]
use anyhow::{Context, Result};
use clap::{Parser, Subcommand};
use serde_json::json;
use std::path::{Path, PathBuf};
use std::time::SystemTime;

// Generated capnp bindings for the cloister↔rosary IPC wire (rosary-6371e3).
// Source schema: schemas/cloister.capnp (vendored from cloister/wire/cloister.capnp).
#[allow(clippy::all, dead_code, unused_imports)]
mod cloister_capnp {
    include!(concat!(env!("OUT_DIR"), "/cloister_capnp.rs"));
}

mod acp;
#[allow(dead_code)] // API surface — wired in rsry-e608bb (reconciler integration)
mod backend;
mod bdr_enrich;
mod bead;
mod bead_dolt;
mod bead_sqlite;
mod capture;
mod cli;
mod config;
mod decompose;
mod dispatch;
mod dolt;
mod dsse;
#[allow(dead_code)] // API surface for PM agent (loom-w8c.4); is_dominated_by used by reconciler
mod epic;
#[allow(dead_code)] // API surface — PR creation from dispatch pipeline
mod github;
mod github_mirror;
#[allow(dead_code)] // API surface — wired into pipeline phase transitions
mod handoff;
mod import;
mod linear;
#[allow(dead_code)]
mod linear_tracker;
#[allow(dead_code)] // API surface — consumed by orchestrator after dispatch
mod manifest;
#[allow(dead_code)]
mod migrate;
mod notes;
#[allow(dead_code)] // ADR-0010 substrate; observers wired in obs-* follow-up beads
mod observation;
mod orchestrate;
mod pipeline;
mod plugin;
mod pool;
mod queue;
mod reconcile;
mod repo_cache;
mod scan_assay;
mod scanner;
// `ScopeId` for rosary-b5da2f scope abstraction. Pure type + parsing in
// PR 1; threaded through stores + MCP handlers in later PRs. Allow
// dead_code while the call sites are still on `repo_path: &str`.
#[allow(dead_code)]
mod scope;
mod secrets;
mod serve;
mod session;
#[allow(dead_code)] // API surface — wired in rsry-e599fb (SpritesProvider)
mod sprites;
#[allow(dead_code)] // API surface — wired in rsry-e608bb (reconciler integration)
mod sprites_provider;
#[allow(dead_code)] // Phase 1: traits + impl, wired in Phase 2
mod store;
#[allow(dead_code)] // Phase 1: Dolt backend, wired in Phase 2
mod store_dolt;
#[allow(dead_code)] // Phase 1: SQLite backend, wired alongside Dolt
mod store_sqlite;
#[allow(dead_code)]
mod sync;
#[cfg(test)]
mod testutil;
mod vcs;
mod verify;
#[allow(dead_code)] // API surface — replaces dispatch.rs worktree logic
mod workspace;
mod xref;

#[derive(Parser)]
#[command(
    name = "rsry",
    about = "Strings beads, repos, and review layers into coordinated work",
    version = concat!(
        env!("CARGO_PKG_VERSION"),
        " (",
        env!("RSRY_BUILD_HASH"),
        " ",
        env!("RSRY_BUILD_TIME"),
        ")"
    ),
)]
struct Cli {
    #[command(subcommand)]
    command: Command,
}

#[derive(Subcommand)]
enum Command {
    /// Scan repos for issues, create beads (bottom-up discovery)
    Scan {
        /// Config file listing repos to scan
        #[arg(short, long, default_value = "rosary.toml")]
        config: String,
        /// Filter to specific repos (comma-separated)
        #[arg(long)]
        repo: Option<String>,
        /// Run assay.scan plugins and file P3 chore beads for stale refs
        #[arg(long)]
        assay: bool,
    },
    /// Decompose a Linear ticket into repo-scoped beads (top-down planning)
    Plan {
        /// Linear ticket ID or URL
        ticket: String,
    },
    /// Bidirectional sync: beads ↔ Linear status
    Sync {
        /// Preview changes without executing
        #[arg(long)]
        dry_run: bool,
        /// Filter to specific repos (comma-separated)
        #[arg(long)]
        repo: Option<String>,
        /// Mirror bead context to linked GitHub PRs/issues as structured comments
        #[arg(long)]
        github: bool,
    },
    /// Show aggregated status across all repos
    Status {
        /// Filter to specific repos (comma-separated)
        #[arg(long)]
        repo: Option<String>,
        /// Output as JSON (for scripts/statusline)
        #[arg(long)]
        json: bool,
    },
    /// Dispatch a bead to a Claude Code agent in an isolated worktree
    Dispatch {
        /// Bead ID to work on
        bead_id: String,
        /// Repo path containing .beads/
        #[arg(short, long, default_value = ".")]
        repo: String,
        /// Use isolated jj workspace
        #[arg(long, default_value_t = true)]
        isolate: bool,
    },
    /// Run the reconciliation loop (scan → triage → dispatch → verify → report)
    Run {
        /// Config file listing repos
        #[arg(short, long, default_value = "rosary.toml")]
        config: String,
        /// Max concurrent Claude Code agents
        #[arg(long, default_value_t = 3)]
        concurrency: usize,
        /// Seconds between scan iterations
        #[arg(long, default_value_t = 30)]
        interval: u64,
        /// Single pass (no loop)
        #[arg(long)]
        once: bool,
        /// Print what would be dispatched without actually spawning agents
        #[arg(long)]
        dry_run: bool,
        /// AI provider to use for dispatch (claude, gemini)
        #[arg(long, default_value = "claude")]
        provider: String,
        /// Overnight mode: prefer small/mechanical beads, concurrency=1, interval=120s
        #[arg(long)]
        overnight: bool,
        /// Target a specific bead (skip triage, dispatch only this bead)
        #[arg(long)]
        bead: Option<String>,
    },
    /// Start the reconciliation daemon in the background
    Start {
        /// Config file listing repos
        #[arg(short, long, default_value = "rosary.toml")]
        config: String,
        /// Max concurrent agents
        #[arg(long, default_value_t = 3)]
        concurrency: usize,
        /// Seconds between scan iterations
        #[arg(long, default_value_t = 30)]
        interval: u64,
        /// AI provider (claude, gemini)
        #[arg(long, default_value = "claude")]
        provider: String,
        /// Overnight mode
        #[arg(long)]
        overnight: bool,
    },
    /// Stop the running daemon
    Stop,
    /// Tail the daemon log
    Logs,
    /// Start MCP server exposing rosary as tools
    Serve {
        /// Transport: stdio or http
        #[arg(long, default_value = "stdio")]
        transport: String,
        /// Port for HTTP transport
        #[arg(long, default_value = "8383")]
        port: u16,
    },
    /// Start MCP server over a Unix Domain Socket (capnp ToolCall/Result).
    ///
    /// Invoked by cloister's `cluster.capnp` with
    /// `args = ["mcp", "--ipc-socket", "/run/cloister-uds/rosary.sock"]`.
    /// Wire format is the intra-cluster amendment of ADR-0005: plain capnp
    /// ToolCall in, ToolResult out, no Manifest envelope, no AEAD. The UDS
    /// permissions are the trust boundary.
    Mcp {
        /// Path to bind the UDS at. Stale socket files at this path are
        /// removed before bind.
        #[arg(long, value_name = "PATH")]
        ipc_socket: PathBuf,
    },
    /// Send a single capnp ToolCall to a rosary IPC server (smoke + ops).
    ///
    /// Connects to `--ipc-socket`, sends one ToolCall, prints the
    /// `text` content of the ToolResult to stdout. Exit 0 on
    /// `isError = false`, 1 otherwise. Used by `task image:smoke` and
    /// the docker e2e test to verify the wire from inside the same
    /// container/VM namespace as the server (sidesteps the Docker
    /// Desktop macOS host→container AF_UNIX boundary).
    IpcCall {
        /// UDS path to connect to.
        #[arg(long, value_name = "PATH")]
        ipc_socket: PathBuf,
        /// MCP tool name (e.g. `rsry_status`).
        #[arg(long)]
        tool: String,
        /// JSON arguments. Defaults to `{}`.
        #[arg(long, default_value = "{}")]
        args: String,
    },
    /// Register current repo (or path) in the global registry (~/.rsry/config.toml)
    Enable {
        /// Path to repo root (defaults to current directory)
        #[arg(default_value = ".")]
        path: String,
    },
    /// Unregister a repo from the global registry by name or path
    Disable {
        /// Repo name or path to remove
        name_or_path: String,
    },
    /// Approve a repo for agent auto-dispatch (sets approval = approved).
    /// Only consulted when [dispatch] require_approval = true.
    Approve {
        /// Repo name to approve
        name: String,
    },
    /// Reject a repo for agent auto-dispatch (sets approval = rejected).
    Reject {
        /// Repo name to reject
        name: String,
    },
    /// Re-parent a thread under a different decade. Useful for cleanup when
    /// threads end up in `ungrouped` or `auto-discovered` and should be
    /// grouped under a real decade.
    ThreadReparent {
        /// Thread ID (e.g. agentic-provenance/agent-identity)
        thread_id: String,
        /// New decade ID (e.g. agentic-provenance)
        decade_id: String,
        /// Optional new thread name (keeps existing if omitted)
        #[arg(short, long)]
        name: Option<String>,
    },
    /// Decompose a markdown document (ADR, README, etc.) into beads
    Decompose {
        /// Path to the markdown file
        path: String,
        /// Title for the decade (defaults to first heading)
        #[arg(short, long)]
        title: Option<String>,
        /// Repo path to create beads in
        #[arg(short, long, default_value = ".")]
        repo: String,
        /// Preview without creating beads
        #[arg(long)]
        dry_run: bool,
        /// LLM model for non-ADR docs (haiku, sonnet, or full model ID).
        /// When set and the document is not ADR-shaped, uses the Anthropic
        /// Messages API to extract atoms instead of the heuristic parser.
        /// Requires ANTHROPIC_API_KEY env var.
        #[arg(long)]
        model: Option<String>,
        /// Emit code stubs into this repo path instead of (or in addition to)
        /// creating beads. Generates `.rsry-stubs/<decade>.rs` from
        /// TechnicalSpec and Constraint atoms. Review the stub PR before
        /// implementing to validate the design.
        #[arg(long)]
        stub_output: Option<String>,
    },
    /// Manage beads directly
    Bead {
        #[command(subcommand)]
        action: BeadAction,
        /// Repo path containing .beads/
        #[arg(short, long, default_value = ".")]
        repo: String,
    },
    /// Garbage-collect merged agent branches from origin
    Sweep {
        /// Repo path (defaults to current directory)
        #[arg(short, long, default_value = ".")]
        repo: String,
        /// Preview what would be deleted without making changes
        #[arg(long)]
        dry_run: bool,
    },
    /// Close beads whose PRs have already merged.
    ///
    /// Walks every open bead with a recorded `pr_url` event, runs
    /// `gh pr view --json state,mergeCommit`, and closes the bead if the PR
    /// is MERGED. Useful for catching up after periods when the reconciler
    /// loop wasn't running and `poll_pr_status` missed merges (the
    /// `scan_vcs` path only looks at the last 50 commits).
    ///
    /// Idempotent. Safe to run any time. Doesn't dispatch agents.
    CloseMerged {
        /// Repo name to limit the sweep to (omit to scan all registered repos)
        #[arg(long)]
        repo: Option<String>,
        /// Preview what would close without making changes
        #[arg(long)]
        dry_run: bool,
    },
    /// Export orchestrator backend state to JSON backup
    Backup {
        /// Output directory (default: ~/.rsry/backups/<timestamp>)
        #[arg(short, long)]
        output: Option<String>,
    },
    /// Migrate orchestrator backend between providers (e.g. dolt → sqlite)
    Migrate {
        /// Target provider: "sqlite" or "dolt"
        #[arg(long)]
        to: String,
        /// Target path (default: ~/.rsry/backend.db for sqlite)
        #[arg(long)]
        path: Option<String>,
        /// Skip post-migration verification
        #[arg(long)]
        skip_verify: bool,
    },
    /// Capture design atoms from a session transcript or source file
    Capture {
        /// Read transcript file (use `-` for stdin)
        #[arg(long, conflicts_with = "from_code")]
        from_session: Option<String>,
        /// Read source file: `<repo> <path>` (e.g. `rosary src/bead.rs`)
        #[arg(long, num_args = 2, conflicts_with = "from_session")]
        from_code: Vec<String>,
        /// Symbol to scope code capture (e.g. `BeadSpec`)
        #[arg(long)]
        symbol: Option<String>,
        /// LLM model: haiku (default), sonnet, or full model ID
        #[arg(long, default_value = "haiku")]
        model: String,
        /// Repo path for --commit (default: current directory)
        #[arg(short, long, default_value = ".")]
        repo: String,
        /// Write extracted BeadSpecs as beads (default: dry-run to stdout)
        #[arg(long)]
        commit: bool,
    },
    /// Manage encrypted notes (age-encrypted, scope-organized)
    Notes {
        #[command(subcommand)]
        action: NotesAction,
        /// Repo path containing `notes/`
        #[arg(short, long, default_value = ".")]
        repo: String,
    },
    /// Manage git hooks for bead sync (post-push / post-merge)
    Hooks {
        #[command(subcommand)]
        action: HooksAction,
        /// Repo path (defaults to current directory)
        #[arg(short, long, default_value = ".")]
        repo: String,
    },
}

#[derive(Subcommand)]
enum NotesAction {
    /// Re-encrypt all notes in a scope after editing the recipient list
    Rotate {
        /// Scope name (becomes `notes/<scope>/`)
        #[arg(long)]
        scope: String,
        /// Recipient(s) to add (repeatable)
        #[arg(long = "add-recipient", value_name = "RECIPIENT")]
        add: Vec<String>,
        /// Recipient(s) to remove (repeatable)
        #[arg(long = "remove-recipient", value_name = "RECIPIENT")]
        remove: Vec<String>,
        /// Identity file for decryption (default: $HOME/.config/age/keys.txt)
        #[arg(long)]
        identity: Option<PathBuf>,
    },
}

#[derive(Subcommand)]
enum HooksAction {
    /// Splice post-push / post-merge bead-sync blocks into the repo's hooks
    /// directory. The hooks dir is resolved via `git rev-parse --git-path
    /// hooks` so worktrees, submodules, and `core.hooksPath` overrides all
    /// route correctly. Existing user content outside the rsry markers is
    /// preserved.
    Install,
    /// Show whether each rsry-managed hook is installed (and where) and
    /// whether the Dolt remote is configured for bead sync.
    Status,
}

#[derive(Subcommand)]
enum BeadAction {
    /// Create a new bead
    Create {
        /// Bead title
        title: String,
        /// Description
        #[arg(short, long, default_value = "")]
        description: String,
        /// Priority (0=P0 highest, 3=P3 lowest)
        #[arg(short, long, default_value_t = 2)]
        priority: u8,
        /// Issue type
        #[arg(short = 't', long, default_value = "task")]
        issue_type: String,
        /// Source files this bead touches (comma-separated)
        #[arg(short, long, value_delimiter = ',')]
        files: Vec<String>,
        /// Test files to validate the change (comma-separated)
        #[arg(long, value_delimiter = ',')]
        test_files: Vec<String>,
    },
    /// Close a bead
    Close {
        /// Bead ID
        id: String,
        /// Skip the verifiable-test-command check (for legacy/non-impl beads)
        #[arg(long)]
        force: bool,
    },
    /// List open beads
    List,
    /// Reopen a closed bead (sets status to `open`).
    /// Useful for recovering from accidental closures or revisiting work.
    Reopen {
        /// Bead ID
        id: String,
    },
    /// Manage comments on a bead (add/update/delete/list)
    Comment {
        #[command(subcommand)]
        action: BeadCommentAction,
    },
    /// Search beads by title/description
    Search {
        /// Search query
        query: String,
    },
    /// Export beads as JSON (for import into another rsry instance)
    Export {
        /// Filter by status (open, blocked, all). Default: open
        #[arg(short, long, default_value = "open")]
        status: String,
    },
    /// Import beads from a JSON file or stdin
    Import {
        /// JSON file path (reads stdin if omitted)
        file: Option<String>,
    },
}

/// Subcommands of `rsry bead comment` (rosary-a96b06).
///
/// `Add` is the legacy primitive (was `rsry bead comment <id> <body>` flat);
/// `List`/`Update`/`Delete` were added with the audit-trail columns. Hard
/// delete is CLI-only and gated behind `--hard` to preserve the audit-trail
/// invariant for normal flows.
#[derive(Subcommand)]
enum BeadCommentAction {
    /// Append a new comment to a bead.
    Add {
        /// Bead ID
        id: String,
        /// Comment body
        body: String,
    },
    /// List comments on a bead with their comment_ids (needed for update/delete).
    List {
        /// Bead ID
        id: String,
        /// Include soft-deleted comments in the listing
        #[arg(long)]
        include_deleted: bool,
    },
    /// Update the body of an existing comment.
    Update {
        /// Bead ID (informational; comment_id is the addressable key)
        id: String,
        /// Stable comment id (see `rsry bead comment list`)
        comment_id: String,
        /// New comment body
        #[arg(long)]
        body: String,
        /// Optional reason recorded in the audit trail
        #[arg(long)]
        reason: Option<String>,
    },
    /// Delete a comment. Soft-delete by default (preserves audit trail);
    /// `--hard` removes the row entirely.
    Delete {
        /// Bead ID (informational; comment_id is the addressable key)
        id: String,
        /// Stable comment id (see `rsry bead comment list`)
        comment_id: String,
        /// Optional reason recorded in the audit trail (soft-delete only)
        #[arg(long)]
        reason: Option<String>,
        /// Hard-delete: physically remove the row. Destroys audit trail.
        #[arg(long)]
        hard: bool,
    },
}

/// Generate a bead ID: `{prefix}-{lower 6 hex chars of millis}` (~16M values before collision).
pub fn generate_bead_id(prefix: &str) -> String {
    let millis = SystemTime::now()
        .duration_since(SystemTime::UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis();
    format!("{prefix}-{:06x}", millis & 0xffffff)
}

/// Capture git username from `git config user.name` at bead creation time.
/// Returns None if git is not available or user.name is not set.
/// Imprecise (self-reported) but Good Enough for team attribution.
/// Build an enriched bead description from a `BeadSpec`, appending structured
/// success criteria, cross-references, and provenance as markdown sections.
fn enrich_bead_description(spec: &bdr::decompose::BeadSpec) -> String {
    let mut desc = spec.description.clone();

    if !spec.success_criteria.is_empty() {
        desc.push_str("\n\n## Success Criteria\n\n");
        for sc in &spec.success_criteria {
            match (&sc.command, &sc.threshold) {
                (Some(cmd), _) => {
                    desc.push_str(&format!("- `{cmd}` — {}\n", sc.description));
                }
                (None, Some(threshold)) => {
                    desc.push_str(&format!("- {} (threshold: {threshold})\n", sc.description));
                }
                (None, None) => {
                    desc.push_str(&format!("- {}\n", sc.description));
                }
            }
        }
    }

    if !spec.references.is_empty() {
        desc.push_str("\n\n## References\n\n");
        for r in &spec.references {
            desc.push_str(&format!("- {r}\n"));
        }
    }

    if !spec.derived_from.is_empty() {
        desc.push_str("\n\n## Derived From\n\n");
        for src in &spec.derived_from {
            desc.push_str(&format!("- {}\n", src.label()));
        }
        if let Some(ref trace) = spec.inferred_from {
            desc.push_str(&format!(
                "\n_Classification assisted by `{}`{}_\n",
                trace.model,
                trace
                    .rationale
                    .as_deref()
                    .map(|r| format!(": {r}"))
                    .unwrap_or_default()
            ));
        }
    }

    desc
}

fn git_config_user_name(repo_root: &Path) -> Option<String> {
    std::process::Command::new("git")
        .args(["config", "user.name"])
        .current_dir(repo_root)
        .output()
        .ok()
        .filter(|o| o.status.success())
        .and_then(|o| String::from_utf8(o.stdout).ok())
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
}

/// Resolve the .beads/ directory for a repo, handling git/jj worktrees.
/// In a worktree, .beads/ lives in the main worktree — resolve via git commondir.
pub fn resolve_beads_dir(repo_root: &Path) -> PathBuf {
    if repo_root.join(".beads").exists() {
        return repo_root.join(".beads");
    }
    // Try to find the main worktree's .beads/ via git commondir
    let git_common = std::process::Command::new("git")
        .args(["rev-parse", "--git-common-dir"])
        .current_dir(repo_root)
        .output()
        .ok()
        .and_then(|o| {
            if o.status.success() {
                String::from_utf8(o.stdout).ok()
            } else {
                None
            }
        })
        .map(|s| PathBuf::from(s.trim()));
    if let Some(common) = git_common {
        let main_root = common.parent().unwrap_or(repo_root);
        main_root.join(".beads")
    } else {
        repo_root.join(".beads")
    }
}

fn daemon_pid_path() -> PathBuf {
    dirs_next::home_dir()
        .unwrap_or_else(|| PathBuf::from("."))
        .join(".rsry")
        .join("rsry.pid")
}

fn daemon_log_path() -> PathBuf {
    dirs_next::home_dir()
        .unwrap_or_else(|| PathBuf::from("."))
        .join(".rsry")
        .join("rsry.log")
}

fn read_daemon_pid() -> Option<u32> {
    let path = daemon_pid_path();
    let content = std::fs::read_to_string(&path).ok()?;
    let pid: u32 = content.trim().parse().ok()?;
    // Check if process is alive via kill -0
    let status = std::process::Command::new("kill")
        .args(["-0", &pid.to_string()])
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .status()
        .ok()?;
    if status.success() {
        Some(pid)
    } else {
        let _ = std::fs::remove_file(&path);
        None
    }
}

/// Resolve config path: if user passed "rosary.toml" (default), check global first.
fn resolve_config(config: &str) -> String {
    if config == "rosary.toml" {
        config::resolve_config_path()
    } else {
        config.to_string()
    }
}

/// Parse a comma-separated repo filter into a set of repo names.
fn parse_repo_filter(filter: &Option<String>) -> Option<Vec<String>> {
    filter.as_ref().map(|f| {
        f.split(',')
            .map(|s| s.trim().to_string())
            .filter(|s| !s.is_empty())
            .collect()
    })
}

/// Filter repo configs to only those matching the filter.
fn filter_repos(
    repos: &[config::RepoConfig],
    filter: &Option<Vec<String>>,
) -> Vec<config::RepoConfig> {
    match filter {
        Some(names) => repos
            .iter()
            .filter(|r| names.contains(&r.name))
            .cloned()
            .collect(),
        None => repos.to_vec(),
    }
}

#[tokio::main]
async fn main() -> Result<()> {
    let cli = Cli::parse();

    match cli.command {
        Command::Scan {
            config,
            repo,
            assay,
        } => {
            let cfg = config::load_merged(&resolve_config(&config))?;
            let repo_filter = parse_repo_filter(&repo);
            let repos = filter_repos(&cfg.repo, &repo_filter);

            if assay {
                let all_plugins: Vec<_> = cfg
                    .plugins
                    .iter()
                    .cloned()
                    .chain(config::discover_plugins(None))
                    .collect();
                let registry = plugin::PluginRegistry::new(all_plugins);
                let n = scan_assay::run_assay_scan(&repos, &registry).await?;
                eprintln!("[assay] filed {n} chore bead(s) for stale refs");
            } else {
                let beads = scanner::scan_repos(&repos).await?;
                cli::scan_summary(&beads);
            }
        }
        Command::Plan { ticket } => {
            linear::plan(&ticket).await?;
        }
        Command::Sync {
            dry_run,
            repo,
            github,
        } => {
            let repo_filter = parse_repo_filter(&repo);

            if github {
                let cfg = config::load_merged(&config::resolve_config_path())?;
                let repos = filter_repos(&cfg.repo, &repo_filter);
                let beads = scanner::scan_repos(&repos).await?;
                let token = cfg
                    .github
                    .as_ref()
                    .and_then(|g| g.token.clone())
                    .or_else(|| std::env::var("GITHUB_TOKEN").ok())
                    .context("GITHUB_TOKEN not set and no github.token in config")?;
                let posted = github_mirror::sync_beads_to_github(&beads, &token).await?;
                println!("github: posted {posted} bead-context comment(s)");
                return Ok(());
            }

            // Connect hierarchy store for thread → sub-issue projection
            let sync_cfg = config::load_merged(&config::resolve_config_path())?;
            let hierarchy: Option<Box<dyn store::HierarchyStore>> =
                if let Some(ref backend_cfg) = sync_cfg.backend {
                    match backend_cfg.connect().await {
                        Ok(b) => Some(b as Box<dyn store::HierarchyStore>),
                        Err(e) => {
                            eprintln!("[sync] hierarchy unavailable ({e}), no sub-issue grouping");
                            None
                        }
                    }
                } else {
                    None
                };
            linear::sync(dry_run, repo_filter.as_deref(), hierarchy.as_deref()).await?;
        }
        Command::Status { repo, json } => {
            let cfg = config::load_merged(&config::resolve_config_path())?;
            let repo_filter = parse_repo_filter(&repo);
            let repos = filter_repos(&cfg.repo, &repo_filter);
            let beads = scanner::scan_repos(&repos).await?;
            if json {
                // Use the canonical predicates from `Bead` so this path
                // agrees with the CLI text output AND the `rsry_status`
                // MCP tool. Before this fix, the JSON path used naive
                // status-string equality, which under-counted `blocked`
                // wildly: a bead with `status="open"` but unresolved
                // dependencies is conceptually blocked (the rendering
                // and MCP paths both treated it that way), but the JSON
                // path missed it. Statusline integrations consuming this
                // JSON saw nonsense numbers — the CLI showed "51 blocked"
                // and the JSON showed "1".
                let open = beads.iter().filter(|b| b.status == "open").count();
                let in_progress = beads
                    .iter()
                    .filter(|b| b.status == "in_progress" || b.status == "dispatched")
                    .count();
                let blocked = beads.iter().filter(|b| b.is_blocked()).count();
                let ready = beads.iter().filter(|b| b.is_ready()).count();
                let done = beads
                    .iter()
                    .filter(|b| b.status == "done" || b.status == "closed")
                    .count();

                // Per-repo breakdown — same predicates per repo, same
                // semantics as the global counts above.
                let mut per_repo = std::collections::BTreeMap::new();
                for bead in &beads {
                    let entry = per_repo.entry(bead.repo.clone()).or_insert_with(
                        || serde_json::json!({"open": 0, "in_progress": 0, "blocked": 0}),
                    );
                    if bead.is_blocked() {
                        entry["blocked"] = json!(entry["blocked"].as_u64().unwrap_or(0) + 1);
                    } else if bead.status == "in_progress" || bead.status == "dispatched" {
                        entry["in_progress"] =
                            json!(entry["in_progress"].as_u64().unwrap_or(0) + 1);
                    } else if bead.status == "open" {
                        entry["open"] = json!(entry["open"].as_u64().unwrap_or(0) + 1);
                    }
                }

                println!(
                    "{}",
                    serde_json::json!({
                        "total": beads.len(),
                        "open": open,
                        "ready": ready,
                        "in_progress": in_progress,
                        "blocked": blocked,
                        "done": done,
                        "repos": per_repo,
                    })
                );
            } else {
                cli::print_status_summary(&beads);
                cli::print_ready_beads(&beads, 10);
            }
        }
        Command::Dispatch {
            bead_id,
            repo,
            isolate,
        } => {
            dispatch::run(&bead_id, std::path::Path::new(&repo), isolate).await?;
        }
        Command::Run {
            config,
            concurrency,
            interval,
            once,
            dry_run,
            provider,
            overnight,
            bead,
        } => {
            // --overnight sets defaults, but explicit --concurrency/--interval override
            let concurrency = if overnight && concurrency == 3 {
                1
            } else {
                concurrency
            };
            let interval = if overnight && interval == 30 {
                120
            } else {
                interval
            };
            reconcile::run(
                &resolve_config(&config),
                concurrency,
                interval,
                once,
                dry_run,
                &provider,
                overnight,
                bead.as_deref(),
            )
            .await?;
        }
        Command::Start {
            config,
            concurrency,
            interval,
            provider,
            overnight,
        } => {
            if let Some(pid) = read_daemon_pid() {
                cli::daemon_already_running(pid);
                return Ok(());
            }

            let log_path = daemon_log_path();
            if let Some(parent) = log_path.parent() {
                std::fs::create_dir_all(parent)?;
            }

            let mut args = vec![
                "run".to_string(),
                "--config".to_string(),
                resolve_config(&config),
                "--concurrency".to_string(),
                concurrency.to_string(),
                "--interval".to_string(),
                interval.to_string(),
                "--provider".to_string(),
                provider,
            ];
            if overnight {
                args.push("--overnight".to_string());
            }

            let log_file = std::fs::File::create(&log_path)?;
            // nosemgrep: blocking-subprocess-in-async — .spawn() returns immediately (non-blocking)
            let child = std::process::Command::new(std::env::current_exe()?)
                .args(&args)
                .stdout(log_file.try_clone()?)
                .stderr(log_file)
                .stdin(std::process::Stdio::null())
                .spawn()?;

            let pid = child.id();
            std::fs::write(daemon_pid_path(), pid.to_string())?;
            cli::daemon_started(pid, &log_path.to_string_lossy());
        }
        Command::Stop => {
            if let Some(pid) = read_daemon_pid() {
                unsafe {
                    libc::kill(pid as i32, libc::SIGTERM);
                }
                let _ = std::fs::remove_file(daemon_pid_path());
                cli::daemon_stopped(pid);
            } else {
                println!("No daemon running");
            }
        }
        Command::Logs => {
            let log_path = daemon_log_path();
            if log_path.exists() {
                // nosemgrep: blocking-subprocess-in-async — intentionally blocking: interactive tail -f
                let status = std::process::Command::new("tail")
                    .args(["-f", &log_path.to_string_lossy()])
                    .status()?;
                std::process::exit(status.code().unwrap_or(1));
            } else {
                println!("No log file at {}", log_path.display());
            }
        }
        Command::Serve { transport, port } => {
            serve::run(&transport, port).await?;
        }
        Command::Mcp { ipc_socket } => {
            serve::run_ipc(&ipc_socket).await?;
        }
        Command::IpcCall {
            ipc_socket,
            tool,
            args,
        } => {
            let (text, is_error) = serve::run_ipc_call(&ipc_socket, &tool, args.as_bytes()).await?;
            print!("{text}");
            if !text.ends_with('\n') {
                println!();
            }
            if is_error {
                std::process::exit(1);
            }
        }
        Command::Enable { path } => {
            let entry = config::enable_repo(Path::new(&path))?;
            // Init .beads/ Dolt DB if not present
            if !entry.path.join(".beads").exists() {
                dolt::init_beads_db(&entry.path).await?;
            }
            cli::repo_enabled(&entry.name, &entry.path.to_string_lossy());
        }
        Command::Disable { name_or_path } => match config::disable_repo(&name_or_path)? {
            Some(name) => cli::repo_disabled(&name),
            None => println!("Not found: {name_or_path}"),
        },
        Command::Approve { name } => {
            match config::set_repo_approval(&name, crate::config::DispatchApproval::Approved)? {
                Some(_) => println!("approved {name} for dispatch"),
                None => println!("Not found: {name}"),
            }
        }
        Command::Reject { name } => {
            match config::set_repo_approval(&name, crate::config::DispatchApproval::Rejected)? {
                Some(_) => {
                    println!("rejected {name} — beads from this repo will not auto-dispatch")
                }
                None => println!("Not found: {name}"),
            }
        }
        Command::ThreadReparent {
            thread_id,
            decade_id,
            name,
        } => {
            let backend_cfg = config::load_global()
                .ok()
                .and_then(|c| c.backend)
                .ok_or_else(|| anyhow::anyhow!("[backend] section missing from config"))?;
            let backend = backend_cfg
                .connect()
                .await
                .context("opening orchestrator backend")?;
            store::reparent_thread(&*backend, &thread_id, &decade_id, name.as_deref()).await?;
            println!("reparented {thread_id} → {decade_id}");
        }
        Command::Decompose {
            path,
            title,
            repo,
            dry_run,
            model,
            stub_output,
        } => {
            let markdown =
                std::fs::read_to_string(&path).with_context(|| format!("reading {path}"))?;

            // Route: ADR-shaped docs use the heuristic parser.
            // Non-ADR docs with --model set use LLM extraction.
            let (atoms, meta) = if model.is_some() && !bdr::parse::is_adr_shaped(&markdown) {
                let model_name = model.as_deref().unwrap();
                let atoms = bdr_enrich::extract_atoms_with_llm(&markdown, model_name).await?;
                let meta = bdr::parse::DocMeta {
                    provenance: Some(bdr::provenance::ProvenanceRef::Doc { path: path.clone() }),
                    ..Default::default()
                };
                (atoms, meta)
            } else {
                let parsed = bdr::parse::parse_doc_full(&markdown, &path);
                (parsed.atoms, parsed.meta)
            };

            if atoms.is_empty() {
                println!("No decomposable atoms found in {path}");
                return Ok(());
            }

            let adr_title = title.unwrap_or_else(|| {
                markdown
                    .lines()
                    .find(|l: &&str| l.starts_with("# "))
                    .map(|l: &str| l.trim_start_matches('#').trim().to_string())
                    .unwrap_or_else(|| path.clone())
            });

            let mut decade = bdr::thread::build_decade_with_meta(&path, &adr_title, &atoms, &meta);

            // When LLM extraction was used, stamp inferred_from on every BeadSpec.
            if let Some(ref model_name) = model {
                let trace = bdr::provenance::InferenceTrace {
                    model: bdr_enrich::resolve_model_id(model_name).to_string(),
                    rationale: None,
                };
                for thread in &mut decade.threads {
                    for spec in &mut thread.beads {
                        spec.inferred_from = Some(trace.clone());
                    }
                }
            }

            cli::decompose_decade(
                &decade.title,
                &decade.id,
                &format!("{:?}", decade.status),
                decade.threads.len(),
            );
            for thread in &decade.threads {
                cli::decompose_thread(&thread.name, thread.beads.len());
                for bead_spec in &thread.beads {
                    cli::decompose_bead(
                        &bead_spec.channel.to_string(),
                        &bead_spec.title,
                        &bead_spec.issue_type,
                        bead_spec.priority,
                    );
                }
                if !thread.cross_repo_refs.is_empty() {
                    cli::decompose_refs(&thread.cross_repo_refs);
                }
            }

            if !dry_run {
                let repo_root = scanner::resolve_repo_path(Path::new(&repo));
                let beads_dir = repo_root.join(".beads");
                let client = bead_sqlite::connect_bead_store(&beads_dir).await?;
                let decompose_repo_name = repo_root
                    .file_name()
                    .map(|n| n.to_string_lossy().to_string())
                    .unwrap_or_else(|| repo.clone());
                let created_by = git_config_user_name(&repo_root);

                // Connect to backend store for thread/decade assignment (best-effort).
                let backend: Option<Box<dyn store::BackendStore>> =
                    match config::load_global().ok().and_then(|c| c.backend) {
                        Some(cfg) => cfg.connect().await.ok(),
                        None => None,
                    };

                // Seed decade + threads in the backend so beads land in the lattice.
                if let Some(ref b) = backend {
                    let _ = b
                        .upsert_decade(&store::DecadeRecord {
                            id: decade.id.clone(),
                            title: decade.title.clone(),
                            source_path: path.clone(),
                            status: "active".to_string(),
                        })
                        .await;
                    for thread in &decade.threads {
                        let prefix = config::load_global()
                            .ok()
                            .and_then(|c| c.github)
                            .map(|g| g.agent_branch_prefix)
                            .unwrap_or_else(|| "rosary".to_string());
                        let feature_branch = workspace::thread_branch_name(&prefix, &thread.name);
                        let _ = b
                            .upsert_thread(&store::ThreadRecord {
                                id: thread.id.clone(),
                                name: thread.name.clone(),
                                decade_id: decade.id.clone(),
                                feature_branch: Some(feature_branch),
                            })
                            .await;
                    }
                }

                let mut created = 0;
                let mut skipped = 0;
                for thread in &decade.threads {
                    for spec in &thread.beads {
                        // Dedup: skip if a bead with the exact same title already exists.
                        let existing = client
                            .search_beads(&spec.title, &decompose_repo_name, 10)
                            .await
                            .unwrap_or_default();
                        if existing.iter().any(|b| b.title == spec.title) {
                            eprintln!("  [skip] '{}' — already exists", spec.title);
                            skipped += 1;
                            continue;
                        }

                        // Cross-repo routing: warn when target differs from --repo.
                        if let Some(ref target) = spec.target_repo
                            && target != &decompose_repo_name
                        {
                            eprintln!(
                                "  [route] '{}' → suggested repo: {target} \
                                 (creating in {decompose_repo_name})",
                                spec.title
                            );
                        }

                        // Enrich description with success criteria and references.
                        let desc = enrich_bead_description(spec);

                        let id = generate_bead_id(&decompose_repo_name);
                        let owner = dispatch::default_agent(&spec.issue_type);
                        client
                            .create_bead_full(
                                &id,
                                &spec.title,
                                &desc,
                                spec.priority,
                                &spec.issue_type,
                                owner,
                                &[], // file scopes set by code-reader agent post-dispatch
                                &[],
                                &[], // depends_on: ADR-level refs can't map to bead IDs yet
                                created_by.as_deref(),
                                "",
                                &spec.derived_from,
                            )
                            .await?;

                        // Assign to thread in backend lattice.
                        if let Some(ref b) = backend {
                            let _ = b
                                .add_bead_to_thread(
                                    &thread.id,
                                    &store::WorkRef {
                                        repo: decompose_repo_name.clone(),
                                        bead_id: id.clone(),
                                        scope: String::new(),
                                    },
                                )
                                .await;
                        }

                        created += 1;
                    }
                }
                if skipped > 0 {
                    eprintln!("  [dedup] skipped {skipped} already-existing beads");
                }
                cli::decompose_summary(created, &repo_root.to_string_lossy());
            } else {
                println!();
                println!(
                    "  {}",
                    owo_colors::OwoColorize::dimmed(&"(dry run — no beads created)")
                );
            }

            // Stub output: emit code skeletons for design review.
            if let Some(stub_repo) = stub_output {
                let target = scanner::resolve_repo_path(Path::new(&stub_repo));
                match decompose::write_stubs(&target, &decade.title, &atoms)? {
                    Some(stub_path) => {
                        println!("  stub output → {}", stub_path.display());
                        println!("  hint: commit, push, and open a draft PR for design review");
                    }
                    None => {
                        println!("  (no TechnicalSpec or Constraint atoms — nothing to stub)");
                    }
                }
            }
        }
        Command::Capture {
            from_session,
            from_code,
            symbol,
            model,
            repo,
            commit,
        } => {
            let specs = if let Some(ref transcript) = from_session {
                let opts = capture::SessionCaptureOpts {
                    transcript_path: transcript,
                    model: &model,
                };
                capture::capture_from_session(&opts).await?
            } else if from_code.len() == 2 {
                let repo_root = scanner::resolve_repo_path(Path::new(&repo));
                let opts = capture::CodeCaptureOpts {
                    repo: &from_code[0],
                    path: &from_code[1],
                    symbol: symbol.as_deref(),
                    model: &model,
                    repo_root: &repo_root,
                };
                capture::capture_from_code(&opts).await?
            } else {
                anyhow::bail!("use --from-session <path> or --from-code <repo> <path>");
            };

            if !commit {
                println!("{}", serde_json::to_string_pretty(&specs)?);
            } else {
                let repo_root = scanner::resolve_repo_path(Path::new(&repo));
                let beads_dir = resolve_beads_dir(&repo_root);
                let client = bead_sqlite::connect_bead_store(&beads_dir).await?;
                let repo_name = repo_root
                    .file_name()
                    .map(|n| n.to_string_lossy().to_string())
                    .unwrap_or_else(|| repo.clone());
                let created_by = git_config_user_name(&repo_root);
                let mut created = 0;
                for spec in &specs {
                    let desc = enrich_bead_description(spec);
                    let id = generate_bead_id(&repo_name);
                    let owner = dispatch::default_agent(&spec.issue_type);
                    client
                        .create_bead_full(
                            &id,
                            &spec.title,
                            &desc,
                            spec.priority,
                            &spec.issue_type,
                            owner,
                            &[],
                            &[],
                            &[],
                            created_by.as_deref(),
                            "",
                            &spec.derived_from,
                        )
                        .await?;
                    created += 1;
                }
                cli::decompose_summary(created, &repo_root.to_string_lossy());
            }
        }
        Command::Bead { action, repo } => {
            let repo_root = scanner::resolve_repo_path(Path::new(&repo));
            let beads_dir = resolve_beads_dir(&repo_root);
            let client = bead_sqlite::connect_bead_store(&beads_dir).await?;
            let repo_name = repo_root
                .file_name()
                .map(|n| n.to_string_lossy().to_string())
                .unwrap_or_else(|| repo.clone());

            match action {
                BeadAction::Create {
                    title,
                    description,
                    priority,
                    issue_type,
                    files,
                    test_files,
                } => {
                    if bead::requires_files(&issue_type) && files.is_empty() {
                        anyhow::bail!(
                            "files required for {issue_type} beads — specify which code this bead touches"
                        );
                    }
                    let id = generate_bead_id(&repo_name);
                    let owner = dispatch::default_agent(&issue_type);
                    let created_by = git_config_user_name(&repo_root);
                    client
                        .create_bead_full(
                            &id,
                            &title,
                            &description,
                            priority,
                            &issue_type,
                            owner,
                            &files,
                            &test_files,
                            &[], // CLI doesn't support depends_on yet
                            created_by.as_deref(),
                            "",
                            &[],
                        )
                        .await?;
                    cli::bead_created(&id, &title);
                }
                BeadAction::Close { id, force } => {
                    if !force {
                        let beads = client.list_beads(&repo_name).await?;
                        if let Some(bead) = beads.iter().find(|b| b.id == id || b.id.ends_with(&id))
                            && !bead.has_verifiable_test_command()
                        {
                            anyhow::bail!(
                                "bead {} ({}) has no verifiable test command in its description.\n\
                                 Add e.g. `cargo test -p <crate>` to success criteria, or pass --force to override.",
                                bead.id,
                                bead.issue_type
                            );
                        }
                    }
                    client.close_bead(&id).await?;
                    cli::bead_closed(&id);
                }
                BeadAction::List => {
                    let beads = client.list_beads(&repo_name).await?;
                    cli::bead_list(&beads);
                }
                BeadAction::Reopen { id } => {
                    client.update_status(&id, "open").await?;
                    client.log_event(&id, "reopened", "via rsry-cli").await;
                    println!("reopened {id}");
                }
                BeadAction::Comment { action } => match action {
                    BeadCommentAction::Add { id, body } => {
                        client.add_comment(&id, &body, "rsry-cli").await?;
                        cli::bead_commented(&id);
                    }
                    BeadCommentAction::List {
                        id,
                        include_deleted,
                    } => {
                        let comments = client.list_comments(&id, include_deleted).await?;
                        if comments.is_empty() {
                            println!("(no comments on {id})");
                        } else {
                            for c in comments {
                                let edited = if c.is_edited() { " (edited)" } else { "" };
                                let deleted = if c.is_deleted() { " (deleted)" } else { "" };
                                println!(
                                    "  #{id_:<6} {when} {author}{edited}{deleted}\n      {text}",
                                    id_ = c.id,
                                    when = c.created_at.format("%Y-%m-%d %H:%M:%S"),
                                    author = c.author,
                                    text = c.text.lines().next().unwrap_or(&c.text),
                                );
                                if c.is_edited()
                                    && let Some(orig) = &c.original_text
                                {
                                    println!(
                                        "      original: {}",
                                        orig.lines().next().unwrap_or(orig)
                                    );
                                }
                                if let Some(reason) = &c.edit_reason {
                                    println!("      edit reason: {reason}");
                                }
                                if let Some(reason) = &c.delete_reason {
                                    println!("      delete reason: {reason}");
                                }
                            }
                        }
                    }
                    BeadCommentAction::Update {
                        id: _,
                        comment_id,
                        body,
                        reason,
                    } => {
                        let updated = client
                            .update_comment(&comment_id, &body, reason.as_deref())
                            .await?;
                        println!(
                            "updated comment #{comment_id} on {} (edited at {})",
                            updated.issue_id,
                            updated
                                .edited_at
                                .map(|d| d.format("%Y-%m-%d %H:%M:%S").to_string())
                                .unwrap_or_else(|| "?".to_string())
                        );
                    }
                    BeadCommentAction::Delete {
                        id: _,
                        comment_id,
                        reason,
                        hard,
                    } => {
                        if hard {
                            // Hard-delete is irreversible and destroys audit
                            // trail. Require explicit terminal confirmation.
                            use std::io::Write;
                            print!("hard-delete comment #{comment_id}? type 'yes' to confirm: ");
                            std::io::stdout().flush().ok();
                            let mut buf = String::new();
                            std::io::stdin().read_line(&mut buf)?;
                            if buf.trim() != "yes" {
                                println!("aborted (audit trail preserved)");
                                return Ok(());
                            }
                            client.hard_delete_comment(&comment_id).await?;
                            println!("hard-deleted comment #{comment_id}");
                        } else {
                            client
                                .delete_comment(&comment_id, reason.as_deref())
                                .await?;
                            println!("soft-deleted comment #{comment_id}");
                        }
                    }
                },
                BeadAction::Search { query } => {
                    let beads = client.search_beads(&query, &repo_name, 50).await?;
                    cli::bead_search_results(&beads, &query);
                }
                BeadAction::Export { status } => {
                    let beads = client.list_beads(&repo_name).await?;
                    let filtered: Vec<_> = match status.as_str() {
                        "all" => beads,
                        "blocked" => beads.into_iter().filter(|b| b.is_blocked()).collect(),
                        s => beads.into_iter().filter(|b| b.status == s).collect(),
                    };
                    let export = import::export_beads_json(&filtered);
                    println!("{}", serde_json::to_string_pretty(&export)?);
                }
                BeadAction::Import { file } => {
                    let beads_json = import::read_beads_json(file)?;
                    let r = import::import_beads(&beads_json, &*client, &repo_name).await?;
                    println!(
                        "Imported {}, skipped {} (duplicate titles)",
                        r.imported, r.skipped
                    );
                }
            }
        }
        Command::Sweep { repo, dry_run } => {
            let repo_path = scanner::resolve_repo_path(std::path::Path::new(&repo));
            let result = workspace::sweep_agent_branches(&repo_path, dry_run).await;
            if dry_run {
                println!(
                    "dry-run: {} checked, {} would delete, {} skipped (active), {} skipped (unmerged)",
                    result.checked, result.deleted, result.skipped_active, result.skipped_unmerged
                );
            } else {
                println!(
                    "sweep: {} checked, {} deleted, {} skipped (active), {} skipped (unmerged)",
                    result.checked, result.deleted, result.skipped_active, result.skipped_unmerged
                );
            }
        }
        Command::CloseMerged { repo, dry_run } => {
            let summary = run_close_merged(repo.as_deref(), dry_run).await?;
            let verb = if dry_run { "would close" } else { "closed" };
            println!(
                "close-merged: {} {} (checked={}, no_pr_url={}, not_merged={}, gh_errors={})",
                summary.merged_closed,
                verb,
                summary.checked,
                summary.no_pr_url,
                summary.not_merged,
                summary.gh_errors,
            );
            for id in &summary.bead_ids_closed {
                println!("  {id}");
            }
        }
        Command::Backup { output } => {
            let cfg = config::load_merged(&config::resolve_config_path())?;
            let backend_cfg = cfg
                .backend
                .as_ref()
                .ok_or_else(|| anyhow::anyhow!("[backend] section missing from config"))?;
            let source = backend_cfg.connect_exportable().await?;
            let snapshot = migrate::export_snapshot(&*source, &backend_cfg.provider).await?;
            let rsry_dir = config::rsry_dir();
            let dir = output.unwrap_or_else(|| {
                rsry_dir
                    .join("backups")
                    .join(chrono::Utc::now().format("%Y%m%d-%H%M%S").to_string())
                    .to_string_lossy()
                    .into_owned()
            });
            migrate::save_backup(&snapshot, std::path::Path::new(&dir))?;
            let counts = snapshot.counts();
            eprintln!("Backup saved to {dir}");
            eprintln!(
                "  decades={} threads={} members={} pipelines={} dispatches={} deps={} links={} repos={}",
                counts.decades,
                counts.threads,
                counts.thread_members,
                counts.pipelines,
                counts.dispatches,
                counts.dependencies,
                counts.linear_links,
                counts.user_repos
            );
        }
        Command::Migrate {
            to,
            path,
            skip_verify,
        } => {
            let cfg = config::load_merged(&config::resolve_config_path())?;
            let backend_cfg = cfg
                .backend
                .as_ref()
                .ok_or_else(|| anyhow::anyhow!("[backend] section missing from config"))?;
            let source = backend_cfg.connect_exportable().await?;

            let rsry_dir = config::rsry_dir();
            let target_path = path.unwrap_or_else(|| match to.as_str() {
                "sqlite" => rsry_dir.join("backend.db").to_string_lossy().into_owned(),
                _ => rsry_dir.join("dolt/rosary").to_string_lossy().into_owned(),
            });
            let target_cfg = config::BackendConfig {
                provider: to.clone(),
                path: target_path.clone().into(),
            };
            let target = target_cfg.connect_or_create().await?;

            // Auto-backup before migration
            let backup_dir = rsry_dir
                .join("backups")
                .join(format!(
                    "pre-migrate-{}",
                    chrono::Utc::now().format("%Y%m%d-%H%M%S")
                ))
                .to_string_lossy()
                .into_owned();

            eprintln!("Migrating {} → {} ...", backend_cfg.provider, to);
            let report = migrate::migrate(
                &*source,
                &*target,
                &backend_cfg.provider,
                Some(std::path::Path::new(&backup_dir)),
            )
            .await?;

            eprintln!("Source: {:?}", report.source_counts);
            eprintln!("Target: {:?}", report.target_counts);

            if skip_verify {
                eprintln!("Verification: SKIPPED (--skip-verify)");
                eprintln!();
                eprintln!("To switch, edit ~/.rsry/config.toml:");
                eprintln!("  [backend]");
                eprintln!("  provider = \"{}\"", to);
                eprintln!("  path = \"{}\"", target_path);
            } else if report.verified {
                eprintln!("Verification: PASSED");
                eprintln!();
                eprintln!("To switch, edit ~/.rsry/config.toml:");
                eprintln!("  [backend]");
                eprintln!("  provider = \"{}\"", to);
                eprintln!("  path = \"{}\"", target_path);
            } else {
                eprintln!("Verification: FAILED — counts mismatch!");
                eprintln!("Backup at: {backup_dir}");
                std::process::exit(1);
            }
        }
        Command::Notes { action, repo } => {
            let repo_root = scanner::resolve_repo_path(Path::new(&repo));
            match action {
                NotesAction::Rotate {
                    scope,
                    add,
                    remove,
                    identity,
                } => {
                    let opts = notes::RotateOpts {
                        repo_root: &repo_root,
                        scope: &scope,
                        add_recipients: &add,
                        remove_recipients: &remove,
                        identity: identity.as_deref(),
                    };
                    let result = notes::rotate_scope(&opts).await?;
                    println!(
                        "rotated {} file(s) in notes/{} (recipients: {})",
                        result.files_rotated,
                        scope,
                        result.final_recipients.len()
                    );
                }
            }
        }
        Command::Hooks { action, repo } => {
            let repo_root = scanner::resolve_repo_path(Path::new(&repo));
            match action {
                HooksAction::Install => hooks::install(&repo_root)?,
                HooksAction::Status => hooks::status(&repo_root)?,
            }
        }
    }

    Ok(())
}

// ---------------------------------------------------------------------------
// `rsry hooks` — git hook management for bead sync
// ---------------------------------------------------------------------------

mod hooks {
    //! Git hook management for bead sync (post-push / post-merge).
    //!
    //! Templates are embedded into the binary at compile time via `include_str!`
    //! so installation works in any environment — release images, packaged
    //! binaries, contributor checkouts — without depending on the source tree
    //! being adjacent to the executable.
    //!
    //! Installation is merge-aware: rather than clobbering existing hooks, the
    //! rsry block is spliced between the literal marker lines defined as
    //! [`MARKER_START`] and [`MARKER_END`] below. User content outside those
    //! markers is preserved across re-installs and the operation is
    //! idempotent. `MARKER_END` is intentionally short so the closing line is
    //! easy to grep for; `MARKER_START` carries the do-not-edit hint so
    //! anyone opening the hook file sees the convention without a separate
    //! README round-trip.
    //!
    //! The hooks directory is resolved via `git rev-parse --git-path hooks`
    //! so worktrees, submodules (`.git` is a file pointer to the real
    //! gitdir, not a directory), and `core.hooksPath` overrides all route to
    //! the right place.
    use anyhow::{Context, Result};
    use std::path::{Path, PathBuf};
    use std::process::Command;

    /// Begin marker line for the rsry-managed shell block inside a hook file.
    /// Anything between this line and [`MARKER_END`] is regenerated on each
    /// install. Including the do-not-edit hint on the start marker keeps the
    /// convention visible inside the file itself.
    pub(crate) const MARKER_START: &str = "# >>> rsry-managed (do not edit between these markers; `rsry hooks install` regenerates) >>>";
    /// End marker line — closes the rsry-managed section.
    pub(crate) const MARKER_END: &str = "# <<< rsry-managed <<<";

    /// Hooks rsry manages and their canonical shell-body content.
    ///
    /// Content lives in `docs/git-hooks/*` so it's reviewable alongside the
    /// code; `include_str!` bakes it into the binary so installation works
    /// in released images without `find_template_dir` style filesystem
    /// guessing.
    pub(crate) const HOOKS: &[(&str, &str)] = &[
        ("post-push", include_str!("../docs/git-hooks/post-push")),
        ("post-merge", include_str!("../docs/git-hooks/post-merge")),
    ];

    /// Resolve the actual hooks directory for `repo_root`.
    ///
    /// Uses `git rev-parse --git-path hooks` so worktrees and submodules
    /// (where `.git` is a file pointing at the real gitdir) work correctly —
    /// the previous `repo_root.join(".git").join("hooks")` shortcut was wrong
    /// for both cases.
    pub(crate) fn resolve_hooks_dir(repo_root: &Path) -> Result<PathBuf> {
        let out = Command::new("git")
            .arg("-C")
            .arg(repo_root)
            .args(["rev-parse", "--git-path", "hooks"])
            .output()
            .with_context(|| format!("invoking `git` in {}", repo_root.display()))?;
        if !out.status.success() {
            anyhow::bail!(
                "{} is not a git repo: {}",
                repo_root.display(),
                String::from_utf8_lossy(&out.stderr).trim(),
            );
        }
        let rel = String::from_utf8_lossy(&out.stdout).trim().to_string();
        let candidate = Path::new(&rel);
        Ok(if candidate.is_absolute() {
            candidate.to_path_buf()
        } else {
            repo_root.join(candidate)
        })
    }

    /// Build a fresh hook file from scratch (no existing file at the path).
    /// Wraps the rsry block in `#!/bin/sh` + a brief header + markers.
    pub(crate) fn fresh_hook(block: &str) -> String {
        let mut out = String::new();
        out.push_str("#!/bin/sh\n");
        out.push_str("# Installed by `rsry hooks install`. Edit outside the rsry-managed\n");
        out.push_str("# section below to add your own logic; re-running install will\n");
        out.push_str("# regenerate only the marked block and preserve everything else.\n\n");
        out.push_str(MARKER_START);
        out.push('\n');
        out.push_str(block);
        if !block.ends_with('\n') {
            out.push('\n');
        }
        out.push_str(MARKER_END);
        out.push('\n');
        out
    }

    /// Splice `block` into an existing hook file's contents.
    ///
    /// - If the file already has an rsry marker section, replace just that
    ///   section. Content outside the markers (including the shebang and any
    ///   user-written shell logic) is preserved verbatim.
    /// - If the file has no marker section, append one at the end so the
    ///   user's pre-existing hook continues to run AND the rsry block runs
    ///   after it.
    pub(crate) fn merge_hook(existing: &str, block: &str) -> String {
        if let Some(start) = existing.find(MARKER_START) {
            let after_start = start + MARKER_START.len();
            // Find the matching end marker; if missing (corrupted file),
            // replace through end-of-file rather than leaving stale content.
            let end_inclusive = existing[after_start..]
                .find(MARKER_END)
                .map(|i| after_start + i + MARKER_END.len())
                .unwrap_or(existing.len());
            let mut out = String::with_capacity(existing.len() + block.len());
            out.push_str(&existing[..start]);
            out.push_str(MARKER_START);
            out.push('\n');
            out.push_str(block);
            if !block.ends_with('\n') {
                out.push('\n');
            }
            out.push_str(MARKER_END);
            out.push_str(&existing[end_inclusive..]);
            out
        } else {
            // Append. Leave a blank line between user content and our block
            // so the boundary is visually clear when someone `cat`s the file.
            let mut out = existing.to_string();
            if !out.ends_with('\n') {
                out.push('\n');
            }
            out.push('\n');
            out.push_str(MARKER_START);
            out.push('\n');
            out.push_str(block);
            if !block.ends_with('\n') {
                out.push('\n');
            }
            out.push_str(MARKER_END);
            out.push('\n');
            out
        }
    }

    /// Install rsry hooks into `repo_root`.
    ///
    /// Merge-aware: existing user hooks are preserved. The rsry block is
    /// (re)inserted between markers in each managed hook file. Idempotent —
    /// running install twice produces the same file content the second time.
    pub fn install(repo_root: &Path) -> Result<()> {
        let hooks_dir = resolve_hooks_dir(repo_root)?;
        std::fs::create_dir_all(&hooks_dir)
            .with_context(|| format!("creating {}", hooks_dir.display()))?;

        for (name, block) in HOOKS {
            let dst = hooks_dir.join(name);
            let content = if dst.exists() {
                let existing = std::fs::read_to_string(&dst)
                    .with_context(|| format!("reading existing hook at {}", dst.display()))?;
                merge_hook(&existing, block)
            } else {
                fresh_hook(block)
            };
            std::fs::write(&dst, &content)
                .with_context(|| format!("writing hook at {}", dst.display()))?;
            #[cfg(unix)]
            {
                use std::os::unix::fs::PermissionsExt;
                let mut perms = std::fs::metadata(&dst)?.permissions();
                perms.set_mode(0o755);
                std::fs::set_permissions(&dst, perms)?;
            }
            println!("[hooks] installed {} → {}", name, dst.display());
        }

        // Warn if Dolt remote is not configured — hooks will silently no-op otherwise.
        let repo_name = repo_root
            .file_name()
            .and_then(|n| n.to_str())
            .unwrap_or("repo");
        let dolt_dir = repo_root.join(".beads").join("dolt").join(repo_name);
        if dolt_dir.exists() {
            match check_dolt_remote(&dolt_dir) {
                DoltRemoteStatus::Configured(_) => {}
                DoltRemoteStatus::NotConfigured => {
                    eprintln!(
                        "[hooks] WARNING: no dolt remote configured in {}",
                        dolt_dir.display()
                    );
                    eprintln!(
                        "[hooks] Run: cd {} && dolt remote add origin <url>",
                        dolt_dir.display()
                    );
                }
                DoltRemoteStatus::Errored { exit, stderr } => {
                    eprintln!(
                        "[hooks] WARNING: `dolt remote -v` failed in {} (exit {exit}): {}",
                        dolt_dir.display(),
                        stderr.trim()
                    );
                }
                DoltRemoteStatus::NotInvokable(e) => {
                    eprintln!("[hooks] WARNING: couldn't invoke dolt: {e}");
                }
            }
        }

        Ok(())
    }

    /// Show which rsry hooks are installed and whether Dolt remotes are configured.
    pub fn status(repo_root: &Path) -> Result<()> {
        let hooks_dir = resolve_hooks_dir(repo_root)?;
        println!("repo: {}", repo_root.display());
        println!("hooks dir: {}", hooks_dir.display());
        println!();
        println!("git hooks:");
        for (name, _) in HOOKS {
            let path = hooks_dir.join(name);
            let state = if !path.exists() {
                "✗ not installed"
            } else if std::fs::read_to_string(&path)
                .map(|c| c.contains(MARKER_START))
                .unwrap_or(false)
            {
                "✓ rsry-managed"
            } else {
                "△ exists, no rsry markers (run `rsry hooks install` to merge in)"
            };
            println!("  {state}  {name}");
        }

        let repo_name = repo_root
            .file_name()
            .and_then(|n| n.to_str())
            .unwrap_or("repo");
        let dolt_dir = repo_root.join(".beads").join("dolt").join(repo_name);
        println!();
        println!("dolt remote:");
        if !dolt_dir.exists() {
            println!("  ? no .beads/dolt/{repo_name} found");
            return Ok(());
        }
        match check_dolt_remote(&dolt_dir) {
            DoltRemoteStatus::Configured(stdout) => print!("{stdout}"),
            DoltRemoteStatus::NotConfigured => println!("  ✗ no remote configured"),
            DoltRemoteStatus::Errored { exit, stderr } => {
                println!(
                    "  ! `dolt remote -v` failed (exit {exit}): {}",
                    stderr.trim()
                );
            }
            DoltRemoteStatus::NotInvokable(e) => println!("  ? dolt not available: {e}"),
        }
        Ok(())
    }

    /// Result of probing `dolt remote -v` in a Dolt-backed bead directory.
    ///
    /// Distinguishes "command ran cleanly with no remote" from "command
    /// failed" — the previous code lumped both into "no remote configured"
    /// and hid real errors (e.g. corrupted repo, unsupported Dolt version).
    pub(crate) enum DoltRemoteStatus {
        /// `dolt remote -v` exited 0 with non-empty stdout.
        Configured(String),
        /// `dolt remote -v` exited 0 with empty stdout — truly no remote.
        NotConfigured,
        /// `dolt remote -v` exited non-zero; preserve stderr for the user.
        Errored { exit: i32, stderr: String },
        /// The `dolt` binary couldn't be spawned (missing, no exec perm).
        NotInvokable(String),
    }

    /// Classify a `dolt remote -v` invocation result. Pure function over the
    /// command output so it can be unit-tested without an actual `dolt`
    /// binary.
    pub(crate) fn classify_dolt_remote(
        result: std::io::Result<std::process::Output>,
    ) -> DoltRemoteStatus {
        match result {
            Err(e) => DoltRemoteStatus::NotInvokable(e.to_string()),
            Ok(out) if !out.status.success() => DoltRemoteStatus::Errored {
                exit: out.status.code().unwrap_or(-1),
                stderr: String::from_utf8_lossy(&out.stderr).into_owned(),
            },
            Ok(out) if out.stdout.is_empty() => DoltRemoteStatus::NotConfigured,
            Ok(out) => {
                DoltRemoteStatus::Configured(String::from_utf8_lossy(&out.stdout).into_owned())
            }
        }
    }

    /// Run `dolt remote -v` in the given directory and classify the result.
    fn check_dolt_remote(dolt_dir: &Path) -> DoltRemoteStatus {
        let result = Command::new("dolt")
            .args(["remote", "-v"])
            .current_dir(dolt_dir)
            .output();
        classify_dolt_remote(result)
    }

    #[cfg(test)]
    mod tests {
        use super::*;
        use std::process::Command;

        /// Run `git` with a genuinely-isolated env so the host's gitconfig
        /// can't leak into the test (commit.gpgsign, core.hooksPath, user
        /// identity, etc.). We override:
        ///
        /// - `HOME` → empty tempdir so `$HOME/.gitconfig` is a fresh file
        /// - `GIT_CONFIG_GLOBAL` → /dev/null on Unix so the global file is
        ///   forced empty regardless of HOME
        /// - `GIT_CONFIG_NOSYSTEM=1` → skip `/etc/gitconfig`
        ///
        /// Each call gets its own scratch HOME so tests don't share state.
        fn git(dir: &Path, args: &[&str]) -> std::process::Output {
            // Use the existing dir for HOME; git only writes to ~/.gitconfig
            // when called with `config --global`, which we never do. Pointing
            // HOME at a tempdir under our control is sufficient isolation.
            let home = tempfile::tempdir().expect("HOME tempdir");
            Command::new("git")
                .current_dir(dir)
                .env_clear()
                .env("HOME", home.path())
                .env("GIT_CONFIG_GLOBAL", "/dev/null")
                .env("GIT_CONFIG_NOSYSTEM", "1")
                // Preserve PATH so git itself and its subcommands can be found.
                .env("PATH", std::env::var("PATH").unwrap_or_default())
                .args(args)
                .output()
                .expect("spawn git")
        }

        fn init_repo(dir: &Path) {
            assert!(git(dir, &["init", "-q", "-b", "main"]).status.success());
            // user.email / user.name go into THIS repo's local config, not
            // global — so they don't need the env scaffolding above to take
            // effect, but they also don't hurt.
            assert!(
                git(dir, &["config", "user.email", "test@example.invalid"])
                    .status
                    .success()
            );
            assert!(git(dir, &["config", "user.name", "test"]).status.success());
            assert!(
                git(dir, &["config", "commit.gpgsign", "false"])
                    .status
                    .success()
            );
        }

        fn seed_commit(dir: &Path) {
            std::fs::write(dir.join("seed"), "x").unwrap();
            assert!(git(dir, &["add", "seed"]).status.success());
            assert!(git(dir, &["commit", "-q", "-m", "seed"]).status.success());
        }

        // --- embedded-template invariants ---------------------------------

        #[test]
        fn templates_embedded_and_nonempty() {
            // include_str! is a compile-time op; reading them here proves the
            // build had access to docs/git-hooks/* AND the content survived
            // into the binary. Anything that depends on a real filesystem
            // lookup at runtime (the old find_template_dir path) would fail
            // here when run from a different cwd.
            for (name, content) in HOOKS {
                assert!(!content.trim().is_empty(), "template {name} is empty");
                assert!(
                    content.contains("dolt"),
                    "template {name} should reference dolt commands"
                );
            }
        }

        // --- resolve_hooks_dir --------------------------------------------

        #[test]
        fn resolve_hooks_dir_regular_repo() {
            let dir = tempfile::tempdir().unwrap();
            init_repo(dir.path());
            let resolved = resolve_hooks_dir(dir.path()).unwrap();
            // Canonicalize to avoid symlink-prefix mismatches on macOS
            // (/var vs /private/var).
            assert_eq!(
                resolved.canonicalize().unwrap(),
                dir.path()
                    .canonicalize()
                    .unwrap()
                    .join(".git")
                    .join("hooks")
            );
        }

        #[test]
        fn resolve_hooks_dir_worktree() {
            // Main repo with a seed commit, then `git worktree add` a sibling.
            // Inside the worktree, `.git` is a FILE pointing at the gitdir —
            // the old `repo_root.join(".git").join("hooks")` shortcut would
            // produce a non-existent path here.
            let main = tempfile::tempdir().unwrap();
            init_repo(main.path());
            seed_commit(main.path());

            let wt_parent = tempfile::tempdir().unwrap();
            let wt_path = wt_parent.path().join("wt");
            assert!(
                git(
                    main.path(),
                    &[
                        "worktree",
                        "add",
                        "-q",
                        "-b",
                        "wt-branch",
                        wt_path.to_str().unwrap(),
                    ],
                )
                .status
                .success(),
                "git worktree add must succeed",
            );

            // Sanity: in a worktree, .git is a file (not a directory).
            assert!(wt_path.join(".git").is_file());

            let resolved = resolve_hooks_dir(&wt_path).unwrap();
            // The resolved path must NOT be wt_path/.git/hooks (which doesn't
            // exist as a directory in a worktree). It should point under the
            // main gitdir or a worktree-specific hooks dir, but in any case
            // the canonicalized prefix should be the main repo's gitdir.
            let main_gitdir = main.path().canonicalize().unwrap().join(".git");
            assert!(
                resolved
                    .canonicalize()
                    .or_else(|_| resolved.parent().unwrap().canonicalize())
                    .unwrap()
                    .starts_with(&main_gitdir),
                "worktree hooks dir {} should be under main gitdir {}",
                resolved.display(),
                main_gitdir.display(),
            );
        }

        #[test]
        fn resolve_hooks_dir_non_git_errors() {
            let dir = tempfile::tempdir().unwrap();
            // Empty tempdir, no git init — must fail loudly.
            let err = resolve_hooks_dir(dir.path()).unwrap_err();
            assert!(
                err.to_string().contains("not a git repo"),
                "expected `not a git repo` in error, got: {err}",
            );
        }

        // --- merge_hook (pure-function behavior) --------------------------

        #[test]
        fn merge_hook_replaces_existing_marker_block() {
            let existing = format!(
                "#!/bin/sh\n# user header\necho hi\n\n{}\nold block contents\n{}\necho after\n",
                MARKER_START, MARKER_END
            );
            let merged = merge_hook(&existing, "new block contents\n");
            assert!(merged.contains("user header"));
            assert!(merged.contains("echo hi"));
            assert!(merged.contains("echo after"));
            assert!(merged.contains("new block contents"));
            assert!(
                !merged.contains("old block contents"),
                "old marked content should be replaced"
            );
        }

        #[test]
        fn merge_hook_appends_when_no_existing_markers() {
            let existing = "#!/bin/sh\necho user logic\n";
            let merged = merge_hook(existing, "rsry block\n");
            assert!(merged.contains("echo user logic"));
            assert!(merged.contains(MARKER_START));
            assert!(merged.contains("rsry block"));
            assert!(merged.contains(MARKER_END));
            // User content must precede the marker block when appending.
            assert!(
                merged.find("echo user logic").unwrap() < merged.find(MARKER_START).unwrap(),
                "user content should come before appended rsry block",
            );
        }

        #[test]
        fn merge_hook_idempotent_when_block_unchanged() {
            // Two calls with the same block produce the same final content.
            let starting = format!(
                "#!/bin/sh\necho user\n\n{}\nsame block\n{}\n",
                MARKER_START, MARKER_END
            );
            let first = merge_hook(&starting, "same block\n");
            let second = merge_hook(&first, "same block\n");
            assert_eq!(first, second, "merge should be idempotent");
            assert_eq!(
                second.matches(MARKER_START).count(),
                1,
                "no duplicated marker block"
            );
        }

        #[test]
        fn merge_hook_recovers_when_end_marker_missing() {
            // Defensive: if the file has START but no END (manual edit damage),
            // we replace from START to EOF rather than leaving cruft.
            let existing = format!("#!/bin/sh\n{}\nstale\n(no end marker)\n", MARKER_START);
            let merged = merge_hook(&existing, "fresh\n");
            assert!(!merged.contains("stale"));
            assert!(!merged.contains("(no end marker)"));
            assert!(merged.contains("fresh"));
            assert!(merged.contains(MARKER_END));
        }

        // --- install (full filesystem + git interaction) ------------------

        #[test]
        fn install_into_fresh_repo() {
            let dir = tempfile::tempdir().unwrap();
            init_repo(dir.path());
            install(dir.path()).unwrap();
            for (name, block) in HOOKS {
                let path = dir.path().join(".git").join("hooks").join(name);
                assert!(path.exists(), "{name} should be installed");
                let content = std::fs::read_to_string(&path).unwrap();
                assert!(content.starts_with("#!/bin/sh"));
                assert!(content.contains(MARKER_START));
                assert!(content.contains(MARKER_END));
                assert!(
                    content.contains(block.trim()),
                    "{name} should contain template content",
                );
                #[cfg(unix)]
                {
                    use std::os::unix::fs::PermissionsExt;
                    let mode = std::fs::metadata(&path).unwrap().permissions().mode() & 0o777;
                    assert_eq!(mode, 0o755, "{name} should be executable (0o755)");
                }
            }
        }

        #[test]
        fn install_merges_into_existing_user_hook() {
            let dir = tempfile::tempdir().unwrap();
            init_repo(dir.path());
            let hooks_dir = dir.path().join(".git").join("hooks");
            std::fs::create_dir_all(&hooks_dir).unwrap();
            let post_push = hooks_dir.join("post-push");
            // Simulate a pre-existing user hook with custom logic.
            std::fs::write(
                &post_push,
                "#!/bin/sh\n# team-managed hook\necho 'user custom logic'\n",
            )
            .unwrap();

            install(dir.path()).unwrap();

            let content = std::fs::read_to_string(&post_push).unwrap();
            assert!(
                content.contains("user custom logic"),
                "user logic must be preserved across install: {content}",
            );
            assert!(content.contains(MARKER_START));
            assert!(content.contains(MARKER_END));
            assert!(content.contains("team-managed hook"));
        }

        #[test]
        fn install_idempotent_on_reinstall() {
            let dir = tempfile::tempdir().unwrap();
            init_repo(dir.path());
            install(dir.path()).unwrap();
            let path = dir.path().join(".git").join("hooks").join("post-push");
            let first = std::fs::read_to_string(&path).unwrap();
            // Reinstall — should produce identical bytes (no duplicated block).
            install(dir.path()).unwrap();
            let second = std::fs::read_to_string(&path).unwrap();
            assert_eq!(first, second, "reinstall should be idempotent");
            assert_eq!(
                second.matches(MARKER_START).count(),
                1,
                "no duplicated rsry block on reinstall",
            );
        }

        #[test]
        fn install_preserves_user_content_after_marker_section() {
            let dir = tempfile::tempdir().unwrap();
            init_repo(dir.path());
            // First install lays down the rsry block.
            install(dir.path()).unwrap();
            let path = dir.path().join(".git").join("hooks").join("post-push");
            // User appends their own logic AFTER the rsry marker block.
            let original = std::fs::read_to_string(&path).unwrap();
            std::fs::write(&path, format!("{}\necho 'user trailer'\n", original)).unwrap();

            // Reinstall — must preserve the user trailer.
            install(dir.path()).unwrap();
            let after = std::fs::read_to_string(&path).unwrap();
            assert!(
                after.contains("user trailer"),
                "user trailer dropped: {after}"
            );
            assert!(after.contains(MARKER_START));
        }

        #[test]
        fn install_works_in_git_worktree() {
            // The bug Copilot caught: in a worktree, `.git` is a file pointer
            // and `.git/hooks/` doesn't exist. Old code bailed with "is this a
            // git repo?" — new code routes through `git rev-parse` and writes
            // to the right place.
            let main = tempfile::tempdir().unwrap();
            init_repo(main.path());
            seed_commit(main.path());
            let wt_parent = tempfile::tempdir().unwrap();
            let wt_path = wt_parent.path().join("wt");
            assert!(
                git(
                    main.path(),
                    &[
                        "worktree",
                        "add",
                        "-q",
                        "-b",
                        "wt-install",
                        wt_path.to_str().unwrap(),
                    ],
                )
                .status
                .success()
            );

            install(&wt_path).expect("install must succeed in a worktree");

            // The hook landed somewhere reachable via resolve_hooks_dir.
            let hooks_dir = resolve_hooks_dir(&wt_path).unwrap();
            for (name, _) in HOOKS {
                assert!(
                    hooks_dir.join(name).exists(),
                    "{name} should be installed at {}",
                    hooks_dir.display(),
                );
            }
        }

        #[test]
        fn install_errors_outside_git_repo() {
            let dir = tempfile::tempdir().unwrap();
            // No git init — install must refuse rather than write into a
            // random directory.
            let err = install(dir.path()).unwrap_err();
            assert!(
                err.to_string().contains("not a git repo"),
                "expected error mentioning `not a git repo`, got: {err}",
            );
        }

        // --- classify_dolt_remote (pure-function decision logic) ----------

        /// Forge a `std::process::Output` with the given exit status and
        /// stdout/stderr. Used to drive `classify_dolt_remote` deterministically
        /// without an actual `dolt` binary.
        fn forge_output(success: bool, stdout: &str, stderr: &str) -> std::process::Output {
            use std::os::unix::process::ExitStatusExt;
            let status = std::process::ExitStatus::from_raw(if success { 0 } else { 1 << 8 });
            std::process::Output {
                status,
                stdout: stdout.as_bytes().to_vec(),
                stderr: stderr.as_bytes().to_vec(),
            }
        }

        #[test]
        fn classify_dolt_remote_configured() {
            let out = forge_output(true, "origin\thttps://example.com (fetch)\n", "");
            match classify_dolt_remote(Ok(out)) {
                DoltRemoteStatus::Configured(s) => assert!(s.contains("origin")),
                other => panic!(
                    "expected Configured, got {:?}",
                    std::mem::discriminant(&other)
                ),
            }
        }

        #[test]
        fn classify_dolt_remote_not_configured() {
            let out = forge_output(true, "", "");
            assert!(matches!(
                classify_dolt_remote(Ok(out)),
                DoltRemoteStatus::NotConfigured
            ));
        }

        #[test]
        fn classify_dolt_remote_errored_surfaces_stderr() {
            // The bug Copilot caught: exit-non-zero with empty stdout was
            // misreported as "no remote configured". Now it must surface
            // the failure with stderr preserved.
            let out = forge_output(false, "", "fatal: not a dolt repository\n");
            match classify_dolt_remote(Ok(out)) {
                DoltRemoteStatus::Errored { exit, stderr } => {
                    assert_eq!(exit, 1);
                    assert!(stderr.contains("not a dolt repository"));
                }
                _ => panic!("expected Errored variant for exit-1"),
            }
        }

        #[test]
        fn classify_dolt_remote_spawn_failure() {
            let err = std::io::Error::new(std::io::ErrorKind::NotFound, "no such file");
            match classify_dolt_remote(Err(err)) {
                DoltRemoteStatus::NotInvokable(msg) => assert!(msg.contains("no such file")),
                _ => panic!("expected NotInvokable for spawn failure"),
            }
        }

        // --- documentation / marker consistency ---------------------------

        #[test]
        fn marker_constants_have_expected_shape() {
            // Both markers must start with `# >>>` / `# <<<` so they're
            // greppable AND obviously comments in shell. Anyone who edits
            // the markers later should see this test fail before drift
            // leaks into installed hooks.
            assert!(
                MARKER_START.starts_with("# >>> rsry-managed"),
                "MARKER_START shape changed: {MARKER_START}"
            );
            assert!(
                MARKER_END.starts_with("# <<< rsry-managed"),
                "MARKER_END shape changed: {MARKER_END}"
            );
        }

        #[test]
        fn readme_documents_actual_marker_lines() {
            // Drift-detector: if the marker constants change, the docs/git-hooks
            // README MUST reference the new strings. The README explains where
            // users should look in hook files to find the rsry-managed block.
            let readme = include_str!("../docs/git-hooks/README.md");
            assert!(
                readme.contains(MARKER_START),
                "README must contain MARKER_START literal so users can grep for it"
            );
            assert!(
                readme.contains(MARKER_END),
                "README must contain MARKER_END literal so users can grep for it"
            );
        }
    }
}

// ---------------------------------------------------------------------------
// `rsry close-merged` — catch-up sweep for stalled merged-PR beads
// ---------------------------------------------------------------------------

/// Result of a close-merged sweep across one or all registered repos.
#[derive(Debug, Default, Clone, PartialEq, Eq)]
pub struct CloseMergedSummary {
    /// Open beads inspected
    pub checked: usize,
    /// Beads with no `pr_url` event recorded — skipped
    pub no_pr_url: usize,
    /// Beads whose PR is still open or closed-without-merge — left alone
    pub not_merged: usize,
    /// `gh pr view` failed (auth, network, deleted PR, etc.) — left alone
    pub gh_errors: usize,
    /// Beads we closed (or would close, in dry-run)
    pub merged_closed: usize,
    /// IDs of beads closed (in order processed)
    pub bead_ids_closed: Vec<String>,
}

/// Walks every open bead in the given repo (or all registered repos),
/// looks up the PR URL from the `pr_url` event log, runs `gh pr view` to
/// check merge state, and closes the bead when the PR is MERGED.
///
/// Idempotent. `dry_run = true` reports counts but doesn't write.
pub async fn run_close_merged(
    repo_filter: Option<&str>,
    dry_run: bool,
) -> Result<CloseMergedSummary> {
    let cfg = config::load_merged(&config::resolve_config_path())?;
    run_close_merged_with_config(&cfg, repo_filter, dry_run).await
}

/// Inner form taking an explicit Config — exists so unit tests can
/// pass a hand-built empty `Config` and exercise the no-repos /
/// no-match paths without inheriting whatever's in `~/.rsry/config.toml`
/// (which `load_merged` ALWAYS pulls in, defeating any env-based
/// override).
pub async fn run_close_merged_with_config(
    cfg: &config::Config,
    repo_filter: Option<&str>,
    dry_run: bool,
) -> Result<CloseMergedSummary> {
    let mut summary = CloseMergedSummary::default();

    let repos: Vec<&config::RepoConfig> = cfg
        .repo
        .iter()
        .filter(|r| repo_filter.is_none_or(|name| r.name == name))
        .collect();

    if repos.is_empty() {
        if let Some(name) = repo_filter {
            eprintln!("close-merged: no repo named '{name}' is registered");
        } else {
            eprintln!("close-merged: no repos registered");
        }
        return Ok(summary);
    }

    for repo in repos {
        let resolved = scanner::resolve_repo_path(&repo.path);
        // Use the canonical resolver — handles git/jj worktrees where
        // .beads/ lives in the main worktree, not the worktree root.
        // The naive `resolved.join(".beads")` would silently skip those.
        let beads_dir = resolve_beads_dir(&resolved);
        if !beads_dir.exists() {
            continue;
        }
        // Connect to this repo's bead store via the canonical helper.
        let store = match bead_sqlite::connect_bead_store(&beads_dir).await {
            Ok(s) => s,
            Err(e) => {
                eprintln!("close-merged: skipping {}: {e}", repo.name);
                continue;
            }
        };

        let beads = match store.list_beads(&repo.name).await {
            Ok(b) => b,
            Err(e) => {
                // Surface store errors instead of silently treating them as
                // "no beads" — masking these produced misleading all-zero
                // summaries before.
                eprintln!("close-merged: list_beads({}) failed: {e}", repo.name);
                summary.gh_errors += 1;
                continue;
            }
        };
        // Sweep ALL non-terminal beads — anything that could legitimately
        // have a PR URL set. The original filter on `Open` only missed
        // the exact stuck-needs-merge cases this command was built for
        // (Dispatched, Verifying, PrOpen).
        let candidate_beads: Vec<&bead::Bead> = beads
            .iter()
            .filter(|b| {
                // Done is the only "fully done" terminal in the enum
                // ("closed" status string maps to Done via From<&str>).
                // Rejected is also terminal — skip those too.
                !matches!(b.state(), bead::BeadState::Done | bead::BeadState::Rejected)
            })
            .collect();

        for b in candidate_beads {
            summary.checked += 1;
            // PR URL can be in two places. Prefer the bead's own pr_url
            // column (set at PR creation in workspace_ops), then fall back
            // to the events log. Either is fine — first one wins.
            let pr_url = b.pr_url.clone().filter(|s| !s.trim().is_empty()).or(
                match store.get_latest_event(&b.id, "pr_url").await {
                    Ok(opt) => opt.filter(|s| !s.trim().is_empty()),
                    Err(_) => None,
                },
            );
            let Some(pr_url) = pr_url else {
                summary.no_pr_url += 1;
                continue;
            };
            let pr_url = pr_url.trim().to_string();

            // Ask gh for the merge state. Single call, JSON-shaped.
            let output = tokio::process::Command::new("gh")
                .args(["pr", "view", &pr_url, "--json", "state,mergeCommit"])
                .output()
                .await;
            let Ok(out) = output else {
                summary.gh_errors += 1;
                continue;
            };
            if !out.status.success() {
                summary.gh_errors += 1;
                continue;
            }
            let stdout = String::from_utf8_lossy(&out.stdout);
            let parsed: serde_json::Value = match serde_json::from_str(&stdout) {
                Ok(v) => v,
                Err(_) => {
                    summary.gh_errors += 1;
                    continue;
                }
            };
            let state = parsed["state"].as_str().unwrap_or("");
            if state != "MERGED" {
                summary.not_merged += 1;
                continue;
            }
            let merge_sha = parsed["mergeCommit"]["oid"].as_str().unwrap_or("");

            summary.merged_closed += 1;
            summary.bead_ids_closed.push(b.id.clone());
            if dry_run {
                continue;
            }
            // Record the merge SHA + close the bead. Best-effort logging.
            if !merge_sha.is_empty() {
                store.log_event(&b.id, "merge_sha", merge_sha).await;
            }
            // Format the audit comment so an empty merge_sha doesn't render
            // as `PR merged ()` — that's confusing to read in scrollback.
            let audit_msg = if merge_sha.is_empty() {
                "Auto-closed by rsry close-merged: PR merged".to_string()
            } else {
                format!("Auto-closed by rsry close-merged: PR merged ({merge_sha})")
            };
            store.add_comment(&b.id, &audit_msg, "rosary").await.ok();
            store.close_bead(&b.id).await.ok();
        }
    }

    Ok(summary)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn close_merged_summary_default_is_zero() {
        // Sanity: the summary type's Default is all-zero so reporting a
        // no-op sweep is meaningful (no false positives in the count).
        let s = CloseMergedSummary::default();
        assert_eq!(s.checked, 0);
        assert_eq!(s.merged_closed, 0);
        assert!(s.bead_ids_closed.is_empty());
    }

    #[tokio::test]
    async fn close_merged_no_repos_returns_empty_summary() {
        // Regression guard: when the config has no repos (or none match the
        // filter), run_close_merged_with_config returns Ok with a zero
        // summary — not an error. This keeps the command safe to schedule
        // periodically. Uses an explicit empty Config so the test doesn't
        // pull in the user's real ~/.rsry/config.toml via load_merged.
        let cfg = config::Config::default();
        let summary_no_filter = run_close_merged_with_config(&cfg, None, true)
            .await
            .unwrap();
        let summary_filtered = run_close_merged_with_config(&cfg, Some("nonexistent"), true)
            .await
            .unwrap();

        // Both paths return the all-zero default — no work, no errors.
        assert_eq!(summary_no_filter, CloseMergedSummary::default());
        assert_eq!(summary_filtered, CloseMergedSummary::default());
    }

    #[test]
    fn bead_action_variants_construct() {
        // Verify each BeadAction variant can be constructed with expected fields
        let create = BeadAction::Create {
            title: "Fix the widget".to_string(),
            description: "It is broken".to_string(),
            priority: 1,
            issue_type: "bug".to_string(),
            files: vec!["src/widget.rs".to_string()],
            test_files: vec![],
        };
        assert!(matches!(create, BeadAction::Create { priority: 1, .. }));

        let close = BeadAction::Close {
            id: "rsry-abc".to_string(),
            force: false,
        };
        assert!(matches!(close, BeadAction::Close { .. }));

        let list = BeadAction::List;
        assert!(matches!(list, BeadAction::List));

        let comment = BeadAction::Comment {
            action: BeadCommentAction::Add {
                id: "rsry-abc".to_string(),
                body: "looking into this".to_string(),
            },
        };
        assert!(matches!(
            comment,
            BeadAction::Comment {
                action: BeadCommentAction::Add { .. }
            }
        ));
    }

    #[test]
    fn generate_bead_id_uses_repo_prefix() {
        let id = generate_bead_id("mache");
        assert!(
            id.starts_with("mache-"),
            "id should start with 'mache-': {id}"
        );
        // Suffix must be exactly 6 hex characters
        let suffix = &id["mache-".len()..];
        assert_eq!(suffix.len(), 6, "suffix should be 6 chars: {suffix}");
        assert!(
            suffix.chars().all(|c| c.is_ascii_hexdigit()),
            "suffix should be hex: {suffix}"
        );
    }

    #[test]
    fn generate_bead_id_different_repos() {
        let id1 = generate_bead_id("rosary");
        let id2 = generate_bead_id("mache");
        assert!(id1.starts_with("rosary-"));
        assert!(id2.starts_with("mache-"));
    }
}
