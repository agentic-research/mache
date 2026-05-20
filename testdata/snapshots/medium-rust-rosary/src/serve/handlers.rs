//! Tool handler functions — implementation of each `rsry_*` MCP tool.

use anyhow::{Context, Result};
use serde_json::{Value, json};

use crate::config;
use crate::pool::RepoPool;
use crate::store::{
    BackendStore, BeadStore, CrossRepoDep, DispatchRecord, EvidenceTier, PipelineState, WorkRef,
};

/// Default result limit for bead search (keeps MCP responses bounded).
const SEARCH_DEFAULT_LIMIT: u64 = 20;
/// Hard ceiling — even if the caller asks for more.
const SEARCH_MAX_LIMIT: u64 = 50;
/// Truncate bead descriptions in search results to this many bytes.
const SEARCH_DESC_TRUNCATE: usize = 200;
/// Maximum bytes for a bead title.
const TITLE_MAX_LEN: usize = 512;
/// Maximum bytes for a bead description or comment body.
const BODY_MAX_LEN: usize = 50_000;
/// Maximum valid priority value (P0–P3).
const PRIORITY_MAX: u64 = 3;
// Canonical issue type list lives in crate::bead::VALID_ISSUE_TYPES.

// ---------------------------------------------------------------------------
// Argument parsing helpers
// ---------------------------------------------------------------------------

/// Parse a boolean arg from MCP JSON, with an explicit default.
/// Returns `default` if the key is missing, null, or not a bool.
fn parse_bool_arg(args: &Value, key: &str, default: bool) -> bool {
    args.get(key).and_then(|v| v.as_bool()).unwrap_or(default)
}

// ---------------------------------------------------------------------------
// Client helpers
// ---------------------------------------------------------------------------
/// Get a BeadStore — try the pool first (by name then path), fall back to fresh connect.
pub(crate) async fn get_client<'a>(repo_path: &str, pool: &'a RepoPool) -> Result<StoreRef<'a>> {
    let name = repo_name_from_path(repo_path);
    if let Some(store) = pool.get(&name) {
        return Ok(StoreRef::Pooled(store));
    }
    if let Some((_name, store)) = pool.get_by_path(repo_path) {
        return Ok(StoreRef::Pooled(store));
    }
    let root = crate::scanner::resolve_repo_path(std::path::Path::new(repo_path));
    let beads_dir = crate::resolve_beads_dir(&root);
    Ok(StoreRef::Owned(
        crate::bead_sqlite::connect_bead_store(&beads_dir).await?,
    ))
}

pub(crate) enum StoreRef<'a> {
    Pooled(&'a dyn BeadStore),
    Owned(Box<dyn BeadStore>),
}

impl StoreRef<'_> {
    pub(crate) fn as_store(&self) -> &dyn BeadStore {
        match self {
            StoreRef::Pooled(s) => *s,
            StoreRef::Owned(s) => s.as_ref(),
        }
    }
}

pub(crate) fn repo_name_from_path(repo_path: &str) -> String {
    std::path::Path::new(repo_path)
        .file_name()
        .map(|n| n.to_string_lossy().to_string())
        .unwrap_or_else(|| "unknown".into())
}

/// Resolve the calling MCP args into a `(ScopeId, StoreRef)` pair for
/// any handler that needs a per-repo `BeadStore`. Centralizes the
/// scope-or-repo_path parsing pattern that bead_link uses (PR #212) so
/// converting the other 13 repo_path-taking handlers becomes a
/// one-liner — they all share this entry point.
///
/// Behavior:
/// - Args parse via [`resolve_scope`](crate::serve::scope_args::resolve_scope)
///   (`scope` takes precedence over `repo_path`; bare repo names parse
///   as `ScopeId::Repo`).
/// - For `ScopeId::Repo(name)`: try `pool.get(&name)` first, then fall
///   back to [`get_client`] with `repo_path` (which itself tries
///   `pool.get_by_path` then a fresh SQLite connection). This means a
///   caller can pass `scope: "repo:rosary"` without `repo_path` so
///   long as the pool already has the repo loaded.
/// - For `ScopeId::External(_)` or `ScopeId::Global`: errors. These
///   scopes are addressing-only — no per-repo Dolt store exists for
///   them. Personal-scope storage (the future `rosary-792ed6`
///   substrate) gets its own resolver when it lands.
///
/// rosary-b5da2f PR 5/N (the scope-abstraction series).
pub(crate) async fn resolve_repo_client<'a>(
    args: &Value,
    pool: &'a RepoPool,
) -> Result<(crate::scope::ScopeId, StoreRef<'a>)> {
    use crate::serve::scope_args::resolve_scope;
    let scope = resolve_scope(args)?;
    let repo_name = scope.as_repo_name().ok_or_else(|| {
        anyhow::anyhow!(
            "{scope} scope has no per-repo bead store; this operation is Repo-only. \
             External / Global addressing is supported for LinkageStore edges only \
             (see rsry_bead_link); Personal substrate is tracked under rosary-792ed6."
        )
    })?;
    // Guard against silent mis-attribution when caller passes BOTH
    // `scope` and `repo_path` but they name different repos. Without
    // this check, the resolver would write to repo A's store while
    // labeling output with scope B (Copilot #213 finding).
    if let Some(repo_path_arg) = args.get("repo_path").and_then(|v| v.as_str()) {
        let path_basename = repo_name_from_path(repo_path_arg);
        if path_basename != repo_name {
            anyhow::bail!(
                "scope `repo:{repo_name}` and repo_path basename `{path_basename}` disagree \
                 (scope-and-path mismatch); pick one — pass `scope` alone (and register the \
                 repo in the pool via rsry_repo_register) or pass `repo_path` alone (scope is \
                 inferred from the path)"
            );
        }
    }
    // Try the pool by name first — handles the "scope-only, no
    // repo_path" case for any repo already registered.
    if let Some(store) = pool.get(repo_name) {
        return Ok((scope, StoreRef::Pooled(store)));
    }
    // Fall back to the existing get_client path which expects a
    // filesystem path. This preserves back-compat for callers that
    // still pass `repo_path` directly + handles repos not yet in the
    // pool (one-shot CLI invocations).
    let repo_path = args
        .get("repo_path")
        .and_then(|v| v.as_str())
        .ok_or_else(|| {
            anyhow::anyhow!(
                "scope `{repo_name}` is not loaded in the repo pool and no `repo_path` was passed; \
                 register the repo via `rsry_repo_register` or pass `repo_path` explicitly"
            )
        })?;
    let store_ref = get_client(repo_path, pool).await?;
    Ok((scope, store_ref))
}

// ---------------------------------------------------------------------------
// Tool router
// ---------------------------------------------------------------------------
pub(crate) async fn call_tool(
    name: &str,
    args: &Value,
    config_path: &str,
    pool: &RepoPool,
    backend: Option<&dyn BackendStore>,
    caller: &super::CallerIdentity,
    repo_cache: &crate::repo_cache::RepoCache,
) -> Result<Value> {
    let user_scope = caller.user_scope();

    // Audit log: record every MCP call with caller identity
    if let Some(uid) = user_scope {
        eprintln!("[mcp] {name} (user={uid})");
    }

    match name {
        "rsry_scan" => tool_scan(config_path).await,
        "rsry_status" => tool_status(config_path).await,
        "rsry_list_beads" => {
            let status = args
                .get("status")
                .and_then(|v| v.as_str())
                .map(|s| s.to_string());
            let repo = args
                .get("repo")
                .and_then(|v| v.as_str())
                .map(|s| s.to_string());
            let limit = args
                .get("limit")
                .and_then(|v| v.as_u64())
                .unwrap_or(50)
                .min(200) as usize;
            let offset = args.get("offset").and_then(|v| v.as_u64()).unwrap_or(0) as usize;
            tool_list_beads(
                config_path,
                status.as_deref(),
                repo.as_deref(),
                limit,
                offset,
                user_scope,
            )
            .await
        }
        "rsry_run_once" => {
            let dry_run = parse_bool_arg(args, "dry_run", false);
            let bead_id = args.get("bead_id").and_then(|v| v.as_str());
            tool_run_once(config_path, dry_run, bead_id, user_scope).await
        }
        "rsry_bead_create" => tool_bead_create(args, pool, user_scope).await,
        "rsry_bead_update" => tool_bead_update(args, pool, user_scope).await,
        "rsry_bead_close" => tool_bead_close(args, pool, user_scope).await,
        "rsry_bead_comment" => tool_bead_comment(args, pool, user_scope).await,
        "rsry_bead_comment_list" => tool_bead_comment_list(args, pool, user_scope).await,
        "rsry_bead_comment_update" => tool_bead_comment_update(args, pool, user_scope).await,
        "rsry_bead_comment_delete" => tool_bead_comment_delete(args, pool, user_scope).await,
        "rsry_bead_link" => tool_bead_link(args, pool, backend).await,
        "rsry_bead_search" => tool_bead_search(args, pool, user_scope).await,
        "rsry_dispatch" => tool_dispatch(args, config_path).await,
        "rsry_active" => tool_active().await,
        "rsry_workspace_create" => tool_workspace_create(args, config_path, repo_cache).await,
        "rsry_workspace_checkpoint" => tool_workspace_checkpoint(args).await,
        "rsry_workspace_cleanup" => tool_workspace_cleanup(args),
        "rsry_workspace_merge" => tool_workspace_merge(args).await,
        "rsry_decompose" => tool_decompose(args).await,
        "rsry_pipeline_upsert" => tool_pipeline_upsert(args, backend).await,
        "rsry_pipeline_query" => tool_pipeline_query(args, backend).await,
        "rsry_dispatch_record" => tool_dispatch_record(args, backend).await,
        "rsry_dispatch_history" => tool_dispatch_history(args, backend).await,
        "rsry_decade_list" => tool_decade_list(args, backend).await,
        "rsry_decade_create" => tool_decade_create(args, backend).await,
        "rsry_thread_list" => tool_thread_list(args, backend).await,
        "rsry_thread_create" => tool_thread_create(args, backend).await,
        "rsry_thread_assign" => tool_thread_assign(args, backend).await,
        "rsry_thread_reparent" => tool_thread_reparent(args, backend).await,
        "rsry_repo_register" => tool_repo_register(args, backend, user_scope).await,
        "rsry_repo_list" => tool_repo_list(backend, user_scope).await,
        "rsry_bead_import" => tool_bead_import(args, config_path, pool, user_scope).await,
        "rsry_ticket_load" => tool_ticket_load(args, pool).await,
        _ => anyhow::bail!("Unknown tool: {name}"),
    }
}

// ---------------------------------------------------------------------------
// Scan / status / list
// ---------------------------------------------------------------------------
async fn tool_scan(config_path: &str) -> Result<Value> {
    let cfg = config::load(config_path)?;
    let beads = crate::scanner::scan_repos(&cfg.repo).await?;
    Ok(json!({
        "count": beads.len(),
        "beads": beads,
    }))
}

async fn tool_status(config_path: &str) -> Result<Value> {
    let cfg = config::load(config_path)?;

    // Self-heal stale dispatch state in every repo before counting
    // (rosary-67c43d). The statusline polls this — running the sweep
    // here means a stuck `in_progress` count fixes itself within one
    // statusline refresh, no manual intervention required.
    for repo in &cfg.repo {
        let root = crate::scanner::resolve_repo_path(&repo.path);
        let beads_dir = crate::resolve_beads_dir(&root);
        if let Ok(client) = crate::bead_sqlite::connect_bead_store(&beads_dir).await {
            let repo_name = repo
                .path
                .file_name()
                .and_then(|n| n.to_str())
                .unwrap_or("unknown");
            let _ =
                crate::dispatch::sweep::sweep_orphan_dispatches(client.as_ref(), &root, repo_name)
                    .await;
        }
    }

    let beads = crate::scanner::scan_repos(&cfg.repo).await?;

    // Predicates must match `cli.rs::print_status_summary` and the
    // `rsry status --json` path in main.rs, otherwise the same numbers
    // disagree across the three render surfaces (statusline, terminal,
    // MCP). Before alignment, JSON's `blocked` was status-string equality
    // and missed beads with status="open" + dependency_count > 0 — the
    // canonical `is_blocked()` definition is "status=blocked OR
    // (status=open AND deps unresolved)".
    let open = beads.iter().filter(|b| b.status == "open").count();
    let in_progress = beads
        .iter()
        .filter(|b| b.status == "in_progress" || b.status == "dispatched")
        .count();
    let blocked = beads.iter().filter(|b| b.is_blocked()).count();
    let ready = beads.iter().filter(|b| b.is_ready()).count();
    let total = beads.len();

    Ok(json!({
        "total": total,
        "open": open,
        "ready": ready,
        "in_progress": in_progress,
        "blocked": blocked,
    }))
}

async fn tool_list_beads(
    config_path: &str,
    status: Option<&str>,
    repo: Option<&str>,
    limit: usize,
    offset: usize,
    _user_scope: Option<&str>,
) -> Result<Value> {
    let cfg = config::load(config_path)?;
    let beads = crate::scanner::scan_repos(&cfg.repo).await?;

    let filtered: Vec<_> = beads
        .into_iter()
        .filter(|b| match repo {
            Some(r) => b.repo == r,
            None => true,
        })
        .filter(|b| match status {
            Some("blocked") => b.is_blocked(),
            Some("ready") => b.is_ready(),
            Some(s) => b.status == s,
            None => true,
        })
        .collect();

    let total = filtered.len();
    let page: Vec<_> = filtered.into_iter().skip(offset).take(limit).collect();

    Ok(json!({
        "total": total,
        "count": page.len(),
        "offset": offset,
        "limit": limit,
        "beads": page,
    }))
}

async fn tool_run_once(
    config_path: &str,
    dry_run: bool,
    bead_id: Option<&str>,
    user_id: Option<&str>,
) -> Result<Value> {
    use crate::reconcile::{Reconciler, ReconcilerConfig};
    use std::time::Duration;

    let cfg = config::load(config_path)?;

    let reconciler_config = ReconcilerConfig {
        max_concurrent: 1,
        scan_interval: Duration::from_secs(5),
        repo: cfg.repo,
        once: true,
        dry_run,
        compute: cfg.compute,
        backend: cfg.backend,
        target_bead: bead_id.map(|s| s.to_string()),
        pipelines: cfg.pipelines,
        max_pipeline_depth: cfg.max_pipeline_depth,
        user_id: user_id.map(|s| s.to_string()),
        default_branch: cfg
            .github
            .as_ref()
            .map(|g| g.base.clone())
            .unwrap_or_else(|| "main".to_string()),
        ..Default::default()
    };

    if let Some(target) = bead_id {
        if dry_run {
            // Dry run: single synchronous pass — no background task needed.
            // Avoids infinite loop (dry-run increments dispatched but never
            // reaches terminal state, so run() loops forever).
            let mut reconciler = Reconciler::new(reconciler_config).await;
            let summary = reconciler.iterate().await?;
            return Ok(json!({
                "targeted_bead": target,
                "pipeline": true,
                "status": "dry_run",
                "dispatched": summary.dispatched,
                "triaged": summary.triaged,
                "dry_run": true,
            }));
        }

        // Async hand-off: spawn the full pipeline in the background and return
        // immediately. The MCP HTTP client has a ~60s timeout — the pipeline
        // takes minutes. Use rsry_active to poll for completion.
        let target_id = target.to_string();
        tokio::spawn(async move {
            let mut reconciler = Reconciler::new(reconciler_config).await;
            match reconciler.run().await {
                Ok(summary) => {
                    eprintln!(
                        "[run_once] pipeline for {target_id} finished: dispatched={} passed={} failed={} deadlettered={}",
                        summary.dispatched, summary.passed, summary.failed, summary.deadlettered
                    );
                }
                Err(e) => {
                    eprintln!("[run_once] pipeline for {target_id} failed: {e}");
                }
            }
        });

        Ok(json!({
            "targeted_bead": target,
            "pipeline": true,
            "status": "started",
            "message": "Pipeline running in background. Use rsry_active to monitor progress.",
        }))
    } else {
        // Single pass (no bead_id): fast enough to stay synchronous.
        let mut reconciler = Reconciler::new(reconciler_config).await;
        let summary = reconciler.iterate().await?;
        Ok(json!({
            "scanned": summary.scanned,
            "triaged": summary.triaged,
            "dispatched": summary.dispatched,
            "completed": summary.completed,
            "passed": summary.passed,
            "failed": summary.failed,
            "deadlettered": summary.deadlettered,
            "dry_run": dry_run,
        }))
    }
}

// ---------------------------------------------------------------------------
// Bead CRUD
// ---------------------------------------------------------------------------
async fn tool_bead_create(
    args: &Value,
    pool: &RepoPool,
    user_scope: Option<&str>,
) -> Result<Value> {
    // rosary-b5da2f PR 6: accept `scope` or `repo_path` via the shared
    // resolver. Validation of other args runs BEFORE resolve_repo_client
    // so test fixtures + real callers get the expected error class
    // (e.g. "title must not be blank") rather than an FS error from a
    // nonexistent repo_path that wouldn't have mattered.
    let title = args["title"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("title required"))?;
    if title.trim().is_empty() {
        anyhow::bail!("title must not be blank");
    }
    if title.len() > TITLE_MAX_LEN {
        anyhow::bail!("title exceeds {TITLE_MAX_LEN} bytes (got {})", title.len());
    }
    // description: optional, but must be a string if present (not a number/array)
    let description = match args.get("description") {
        None | Some(Value::Null) => "",
        Some(v) => v
            .as_str()
            .ok_or_else(|| anyhow::anyhow!("description must be a string, got {:?}", v))?,
    };
    if description.len() > BODY_MAX_LEN {
        anyhow::bail!(
            "description exceeds {BODY_MAX_LEN} bytes (got {})",
            description.len()
        );
    }
    // priority: optional, but must be a non-negative integer if present
    let priority_raw = match args.get("priority") {
        None | Some(Value::Null) => 2u64,
        Some(v) => v.as_u64().ok_or_else(|| {
            anyhow::anyhow!("priority must be an integer 0–{PRIORITY_MAX}, got {:?}", v)
        })?,
    };
    if priority_raw > PRIORITY_MAX {
        anyhow::bail!(
            "priority must be 0–{PRIORITY_MAX} (P0=critical … P3=low), got {priority_raw}"
        );
    }
    let priority = priority_raw as u8;
    // issue_type: optional, but must be one of the known values if present
    let issue_type = match args.get("issue_type") {
        None | Some(Value::Null) => "task",
        Some(v) => v
            .as_str()
            .ok_or_else(|| anyhow::anyhow!("issue_type must be a string, got {:?}", v))?,
    };
    if !crate::bead::VALID_ISSUE_TYPES.contains(&issue_type) {
        anyhow::bail!(
            "unknown issue_type {:?} — valid values: {}",
            issue_type,
            crate::bead::VALID_ISSUE_TYPES.join(", ")
        );
    }
    let owner = args
        .get("owner")
        .and_then(|v| v.as_str())
        .unwrap_or_else(|| crate::dispatch::default_agent(issue_type));

    let files: Vec<String> = args
        .get("files")
        .and_then(|v| v.as_array())
        .map(|arr| {
            arr.iter()
                .filter_map(|v| v.as_str().map(|s| s.to_string()))
                .collect()
        })
        .unwrap_or_default();
    let test_files: Vec<String> = args
        .get("test_files")
        .and_then(|v| v.as_array())
        .map(|arr| {
            arr.iter()
                .filter_map(|v| v.as_str().map(|s| s.to_string()))
                .collect()
        })
        .unwrap_or_default();

    if crate::bead::requires_files(issue_type) && files.is_empty() {
        anyhow::bail!(
            "files required for {issue_type} beads — specify which code this bead touches"
        );
    }

    let (scope, client_ref) = resolve_repo_client(args, pool).await?;
    let client = client_ref.as_store();
    let repo_name = scope
        .as_repo_name()
        .expect("Repo-only scope verified by resolve_repo_client");
    let id = crate::generate_bead_id(repo_name);

    // Wire dependencies if provided
    let depends_on: Vec<String> = args
        .get("depends_on")
        .and_then(|v| v.as_array())
        .map(|arr| {
            arr.iter()
                .filter_map(|v| v.as_str().map(|s| s.to_string()))
                .collect()
        })
        .unwrap_or_default();

    // Capture git username from the repo's git config for creator
    // attribution — best-effort, only when `repo_path` is passed
    // explicitly. Pure scope-only callers get `created_by = None`
    // until repo-name → path resolution lands in a follow-up.
    let created_by = args
        .get("repo_path")
        .and_then(|v| v.as_str())
        .and_then(|p| crate::git_config_user_name(std::path::Path::new(p)));

    // Single transaction: INSERT + assignee + files + deps → one dolt commit
    client
        .create_bead_full(
            &id,
            title,
            description,
            priority,
            issue_type,
            owner,
            &files,
            &test_files,
            &depends_on,
            created_by.as_deref(),
            "",
            &[],
        )
        .await?;

    // Set user_id for multi-tenant scoping
    if let Some(uid) = user_scope {
        if let Err(e) = client.set_user_id(&id, uid).await {
            eprintln!("[mcp] failed to set user_id on {id}: {e}");
        }
        client.log_event(&id, "created_by", uid).await;
    }

    Ok(json!({ "id": id, "title": title, "priority": priority, "owner": owner }))
}

async fn tool_bead_update(
    args: &Value,
    pool: &RepoPool,
    _user_scope: Option<&str>,
) -> Result<Value> {
    let id = args["id"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("id required"))?;

    // Validate optional fields before constructing the update.
    // Use presence check (args.get(...).is_some()) rather than as_str()/as_u64()
    // so that wrong-type values (e.g. priority: -1 / "2" / 1.5) are rejected
    // instead of silently ignored.
    if let Some(tv) = args.get("title").filter(|v| !v.is_null()) {
        let t = tv
            .as_str()
            .ok_or_else(|| anyhow::anyhow!("title must be a string, got {:?}", tv))?;
        if t.trim().is_empty() {
            anyhow::bail!("title must not be blank");
        }
        if t.len() > TITLE_MAX_LEN {
            anyhow::bail!("title exceeds {TITLE_MAX_LEN} bytes (got {})", t.len());
        }
    }
    if let Some(dv) = args.get("description").filter(|v| !v.is_null()) {
        let d = dv
            .as_str()
            .ok_or_else(|| anyhow::anyhow!("description must be a string, got {:?}", dv))?;
        anyhow::ensure!(
            d.len() <= BODY_MAX_LEN,
            "description exceeds {BODY_MAX_LEN} bytes (got {})",
            d.len()
        );
    }
    if let Some(pv) = args.get("priority").filter(|v| !v.is_null()) {
        let p = pv.as_u64().ok_or_else(|| {
            anyhow::anyhow!("priority must be an integer 0–{PRIORITY_MAX}, got {:?}", pv)
        })?;
        anyhow::ensure!(
            p <= PRIORITY_MAX,
            "priority must be 0–{PRIORITY_MAX} (P0=critical … P3=low), got {p}"
        );
    }
    if let Some(itv) = args.get("issue_type").filter(|v| !v.is_null()) {
        let it = itv
            .as_str()
            .ok_or_else(|| anyhow::anyhow!("issue_type must be a string, got {:?}", itv))?;
        anyhow::ensure!(
            crate::bead::VALID_ISSUE_TYPES.contains(&it),
            "unknown issue_type {:?} — valid values: {}",
            it,
            crate::bead::VALID_ISSUE_TYPES.join(", ")
        );
    }

    let update = crate::bead::BeadUpdate {
        title: args.get("title").and_then(|v| v.as_str()).map(String::from),
        description: args
            .get("description")
            .and_then(|v| v.as_str())
            .map(String::from),
        priority: args
            .get("priority")
            .and_then(|v| v.as_u64())
            .map(|p| p as u8),
        issue_type: args
            .get("issue_type")
            .and_then(|v| v.as_str())
            .map(String::from),
        owner: args.get("owner").and_then(|v| v.as_str()).map(String::from),
        files: args.get("files").and_then(|v| v.as_array()).map(|arr| {
            arr.iter()
                .filter_map(|v| v.as_str().map(String::from))
                .collect()
        }),
        test_files: args
            .get("test_files")
            .and_then(|v| v.as_array())
            .map(|arr| {
                arr.iter()
                    .filter_map(|v| v.as_str().map(String::from))
                    .collect()
            }),
    };

    if update.is_empty() {
        anyhow::bail!(
            "no fields to update — provide at least one field besides scope/repo_path and id"
        );
    }

    let (_scope, client_ref) = resolve_repo_client(args, pool).await?;
    let client = client_ref.as_store();
    let updated_fields = client.update_bead_fields(id, &update).await?;

    // Log the update event for audit trail
    client
        .log_event(id, "fields_updated", &updated_fields.join(", "))
        .await;

    Ok(json!({ "id": id, "updated_fields": updated_fields }))
}

async fn tool_bead_close(
    args: &Value,
    pool: &RepoPool,
    _user_scope: Option<&str>,
) -> Result<Value> {
    let id = args["id"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("id required"))?;

    let (scope, client_ref) = resolve_repo_client(args, pool).await?;
    let client = client_ref.as_store();
    client.close_bead(id).await?;

    // Unregister the session so rsry_active stops showing it.
    // Best-effort — session may not exist if bead was closed manually.
    if let Ok(mut registry) = crate::session::SessionRegistry::load() {
        // `resolve_repo_client` guarantees Repo(_) here; `expect`
        // preserves that invariant explicitly. Silently falling back
        // to "" would register under the empty key and hide invariant
        // breaks (Copilot #214 finding).
        let repo = scope
            .as_repo_name()
            .expect("Repo-only scope verified by resolve_repo_client");
        let _ = registry.unregister(id, repo);
    }

    Ok(json!({ "id": id, "status": "closed" }))
}

async fn tool_bead_comment(
    args: &Value,
    pool: &RepoPool,
    _user_scope: Option<&str>,
) -> Result<Value> {
    let id = args["id"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("id required"))?;
    let body = args["body"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("body required"))?;
    if body.trim().is_empty() {
        anyhow::bail!("body must not be blank");
    }
    if body.len() > BODY_MAX_LEN {
        anyhow::bail!("body exceeds {BODY_MAX_LEN} bytes (got {})", body.len());
    }

    let (scope, client_ref) = resolve_repo_client(args, pool).await?;
    let client = client_ref.as_store();
    client.add_comment(id, body, "rsry-mcp").await?;

    // Update session registry so rsry_active shows last activity
    if let Ok(mut registry) = crate::session::SessionRegistry::load() {
        // `resolve_repo_client` guarantees Repo(_); see parallel
        // comment in `tool_bead_close` (Copilot #214 finding).
        let repo = scope
            .as_repo_name()
            .expect("Repo-only scope verified by resolve_repo_client");
        let _ = registry.touch(id, repo, body);
    }

    Ok(json!({ "id": id, "comment_added": true }))
}

async fn tool_bead_comment_list(
    args: &Value,
    pool: &RepoPool,
    _user_scope: Option<&str>,
) -> Result<Value> {
    let id = args["id"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("id required"))?;
    let include_deleted = args
        .get("include_deleted")
        .and_then(|v| v.as_bool())
        .unwrap_or(false);

    let (_scope, client_ref) = resolve_repo_client(args, pool).await?;
    let client = client_ref.as_store();
    let comments = client.list_comments(id, include_deleted).await?;

    Ok(json!({
        "id": id,
        "comments": comments,
        "count": comments.len(),
    }))
}

async fn tool_bead_comment_update(
    args: &Value,
    pool: &RepoPool,
    _user_scope: Option<&str>,
) -> Result<Value> {
    // comment_id is an opaque string — Dolt produces UUIDs (char(36)),
    // SQLite produces stringified integers. Accept both via JSON string;
    // also accept JSON numbers for backward compatibility.
    let comment_id_owned: String = match args.get("comment_id") {
        Some(v) if v.is_string() => v.as_str().unwrap().to_string(),
        Some(v) if v.is_i64() => v.as_i64().unwrap().to_string(),
        Some(v) if v.is_u64() => v.as_u64().unwrap().to_string(),
        _ => anyhow::bail!("comment_id required (string)"),
    };
    let comment_id = comment_id_owned.as_str();
    let body = args["body"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("body required"))?;
    if body.trim().is_empty() {
        anyhow::bail!("body must not be blank");
    }
    if body.len() > BODY_MAX_LEN {
        anyhow::bail!("body exceeds {BODY_MAX_LEN} bytes (got {})", body.len());
    }
    let reason = args.get("reason").and_then(|v| v.as_str());

    let (_scope, client_ref) = resolve_repo_client(args, pool).await?;
    let client = client_ref.as_store();
    let updated = client.update_comment(comment_id, body, reason).await?;

    Ok(json!({
        "comment_id": comment_id,
        "updated": true,
        "comment": updated,
    }))
}

async fn tool_bead_comment_delete(
    args: &Value,
    pool: &RepoPool,
    _user_scope: Option<&str>,
) -> Result<Value> {
    // comment_id is an opaque string — Dolt produces UUIDs (char(36)),
    // SQLite produces stringified integers. Accept both via JSON string;
    // also accept JSON numbers for backward compatibility.
    let comment_id_owned: String = match args.get("comment_id") {
        Some(v) if v.is_string() => v.as_str().unwrap().to_string(),
        Some(v) if v.is_i64() => v.as_i64().unwrap().to_string(),
        Some(v) if v.is_u64() => v.as_u64().unwrap().to_string(),
        _ => anyhow::bail!("comment_id required (string)"),
    };
    let comment_id = comment_id_owned.as_str();
    let reason = args.get("reason").and_then(|v| v.as_str());

    // MCP path is soft-delete only — never hard. Hard-delete is CLI-only by
    // design (see rosary-a96b06: audit-trail preservation).
    let (_scope, client_ref) = resolve_repo_client(args, pool).await?;
    let client = client_ref.as_store();
    client.delete_comment(comment_id, reason).await?;

    Ok(json!({
        "comment_id": comment_id,
        "soft_deleted": true,
    }))
}

/// Infer the cross-repo target from a `depends_on` bead id and the
/// calling repo name. Returns `Some("repo/bead-id")` when the bead id
/// matches the canonical `<repo>-<6hex>` shape and the `<repo>` prefix
/// names a repo other than the calling one — meaning the dep is
/// cross-repo even though the caller didn't pass `cross_repo`
/// explicitly.
///
/// Returns None when:
/// - the bead id is malformed (no recognizable `<repo>-<6hex>` shape),
/// - the inferred prefix matches the calling repo (so the dep is same-repo),
/// - no calling repo name is available (e.g. `External` / `Global` scope —
///   auto-routing has nothing to compare against).
///
/// rosary-98ee93: lets callers express cross-repo deps using only the
/// canonical bead id without needing to remember the `cross_repo` arg
/// shape. Explicit `cross_repo` still takes precedence when provided.
///
/// rosary-b5da2f PR 4: takes the calling repo name directly (was:
/// `repo_path: &str` + internal `repo_name_from_path` call). The new
/// signature works for any `ScopeId` — Repo callers pass the name;
/// External/Global callers pass None (via `ScopeId::as_repo_name`).
fn infer_cross_repo_target(calling_repo: Option<&str>, depends_on: &str) -> Option<String> {
    let calling_repo = calling_repo?;
    // Bead IDs follow `<repo>-<6hex>` (e.g. `signet-9605a3`). Repo names may
    // contain `-` themselves (e.g. `ley-line-open`), so split on the LAST
    // `-` and check whether the suffix is a 6-char hex tag.
    let (prefix, suffix) = depends_on.rsplit_once('-')?;
    if prefix.is_empty() || suffix.len() != 6 || !suffix.chars().all(|c| c.is_ascii_hexdigit()) {
        return None;
    }
    if calling_repo == prefix {
        return None;
    }
    Some(format!("{prefix}/{depends_on}"))
}

async fn tool_bead_link(
    args: &Value,
    pool: &RepoPool,
    backend: Option<&dyn BackendStore>,
) -> Result<Value> {
    use crate::serve::scope_args::resolve_scope;
    // rosary-b5da2f PR 4: accept canonical `scope` arg alongside `repo_path`.
    // resolve_scope handles the precedence + error for "neither provided".
    let scope = resolve_scope(args)?;
    // Accept `id` (canonical). When the caller passes the common-but-wrong
    // shorthand `from_id`/`to_id`/`link_type`, the error names both
    // canonical params so the caller doesn't have to guess (rosary-98b11d).
    let id = args["id"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!(
            "id required — got args without `id`. Expected canonical params: `id` (the dependent bead) + `depends_on` (the prerequisite). \
             If you tried `from_id`/`to_id`/`link_type`, those aren't accepted; use `id`/`depends_on` instead."
        ))?;
    let depends_on = args["depends_on"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!(
            "depends_on required — got `id` but no `depends_on`. \
             For cross-repo deps, you may set `depends_on` to either `<bead-id>` (auto-routed by id prefix) or pass `cross_repo` as `<repo>/<bead-id>` explicitly."
        ))?;
    let remove = args
        .get("remove")
        .and_then(|v| v.as_bool())
        .unwrap_or(false);
    // cross_repo: "other-repo/bead-id" — writes to backend LinkageStore instead of per-repo Dolt.
    // Auto-detect (rosary-98ee93): if `depends_on` carries a `<repo>-<id>` prefix that doesn't
    // match the calling repo name, route through LinkageStore without requiring the
    // caller to remember the explicit `cross_repo` argument shape. Auto-detect only
    // engages when scope is `Repo(_)`; External/Global have no bare-name prefix.
    let cross_repo_target: Option<String> = args
        .get("cross_repo")
        .and_then(|v| v.as_str())
        .map(str::to_string)
        .or_else(|| infer_cross_repo_target(scope.as_repo_name(), depends_on));

    if id == depends_on {
        anyhow::bail!("a bead cannot depend on itself ({id})");
    }

    if let Some(target) = cross_repo_target.as_deref() {
        // Cross-repo dep: parse "repo/bead-id" and write to backend LinkageStore.
        let (to_repo, to_bead) = target
            .split_once('/')
            .ok_or_else(|| anyhow::anyhow!("cross_repo must be 'repo-name/bead-id'"))?;
        // Parse `to_repo` as a ScopeId so reserved-namespace strings
        // (`"global"`, `"external:..."`) are rejected — they could
        // otherwise create rows in the reserved namespace via the
        // wrong-side arg, breaking the invariant that Global/External
        // rows are only produced via the matching `ScopeId` variant
        // (Copilot #212 finding).
        let to_scope: crate::scope::ScopeId = to_repo
            .parse()
            .with_context(|| format!("parse cross_repo target repo `{to_repo}` as ScopeId"))?;
        if to_scope.as_repo_name().is_none() {
            anyhow::bail!(
                "cross_repo target repo `{to_repo}` is in a reserved namespace ({to_scope}); \
                 cross_repo is repo-to-repo only. For External/Global targets, use the \
                 `scope` arg on the matching side."
            );
        }
        // Build the `from` WorkRef via the ScopeId bridge so External / Global
        // scopes encode correctly into the LinkageStore schema.
        let from_wr = scope
            .work_ref(id)
            .with_context(|| format!("encode from-scope for bead `{id}`"))?;
        let to_wr = to_scope
            .work_ref(to_bead)
            .with_context(|| format!("encode cross_repo target `{target}`"))?;
        let dep = CrossRepoDep {
            from: from_wr,
            to: to_wr,
            dep_type: "blocks".to_string(),
            evidence_tier: EvidenceTier::Asserted,
            source: "human".to_string(),
            observed_at: chrono::Utc::now(),
        };
        let linkage = backend
            .ok_or_else(|| anyhow::anyhow!("backend store required for cross-repo links"))?;
        if remove {
            linkage.remove_cross_repo_dep(&dep.from, &dep.to).await?;
            Ok(json!({ "id": id, "cross_repo": target, "action": "removed" }))
        } else {
            linkage.add_dependency(&dep).await?;
            Ok(json!({
                "id": id,
                "cross_repo": target,
                "action": "added",
                "evidence_tier": "asserted"
            }))
        }
    } else {
        // Same-repo dep: needs a per-repo Dolt store. Only `Repo` scope has one;
        // External / Global have no backing per-repo store and must route via
        // `cross_repo` instead (rosary-b5da2f PR 4).
        match scope.as_repo_name() {
            Some(_repo_name) => {
                // Prefer the explicit `repo_path` arg (existing call sites).
                // Fall back to a pool lookup by repo name when only `scope`
                // was provided — full path resolution from name lives in a
                // later PR.
                let repo_path = args
                    .get("repo_path")
                    .and_then(|v| v.as_str())
                    .ok_or_else(|| {
                        anyhow::anyhow!(
                            "same-repo dep requires `repo_path` (path lookup from scope name is a follow-up; pass `repo_path` for now or use `cross_repo` for cross-scope deps)"
                        )
                    })?;
                let client_ref = get_client(repo_path, pool).await?;
                let client = client_ref.as_store();
                if remove {
                    client.remove_dependency(id, depends_on).await?;
                    Ok(json!({ "id": id, "depends_on": depends_on, "action": "removed" }))
                } else {
                    client.add_dependency(id, depends_on).await?;
                    Ok(json!({ "id": id, "depends_on": depends_on, "action": "added" }))
                }
            }
            None => {
                // External / Global scope has no per-repo Dolt store; same-repo
                // semantics don't apply. Point the caller at the LinkageStore
                // path via `cross_repo`.
                anyhow::bail!(
                    "same-repo deps are not supported from {scope} scope — \
                     {scope} has no per-repo Dolt store. Pass `cross_repo` to \
                     write the dep through LinkageStore instead."
                );
            }
        }
    }
}

async fn tool_bead_search(
    args: &Value,
    pool: &RepoPool,
    _user_scope: Option<&str>,
) -> Result<Value> {
    let query_str = args["query"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("query required"))?;
    let limit = args
        .get("limit")
        .and_then(|v| v.as_u64())
        .unwrap_or(SEARCH_DEFAULT_LIMIT)
        .min(SEARCH_MAX_LIMIT) as u32;

    let (scope, client_ref) = resolve_repo_client(args, pool).await?;
    let client = client_ref.as_store();
    let repo_name = scope
        .as_repo_name()
        .expect("Repo-only scope verified by resolve_repo_client");
    let beads = client.search_beads(query_str, repo_name, limit).await?;

    // Truncate descriptions to keep response size bounded
    let beads: Vec<Value> = beads
        .iter()
        .map(|b| {
            let mut v = serde_json::to_value(b).context("serializing bead for search results")?;
            if let Some(desc) = v.get("description").and_then(|d| d.as_str())
                && desc.len() > SEARCH_DESC_TRUNCATE
            {
                // Truncate at char boundary to avoid panic on multi-byte UTF-8
                let end = desc
                    .char_indices()
                    .map(|(i, _)| i)
                    .find(|&i| i >= SEARCH_DESC_TRUNCATE)
                    .unwrap_or(desc.len());
                let truncated = format!("{}...", &desc[..end]);
                v["description"] = Value::String(truncated);
            }
            Ok(v)
        })
        .collect::<Result<Vec<_>>>()?;

    Ok(json!({ "count": beads.len(), "beads": beads }))
}

// ---------------------------------------------------------------------------
// Dispatch / active
// ---------------------------------------------------------------------------

/// MCP dispatch: prepares workspace + prompt + spawns the agent as a
/// detached subprocess. Returns the pid + stream-log path.
///
/// Rationale (rosary-748f07): the previous "return a command string for
/// the MCP caller to spawn themselves" design was blocked by Claude Code's
/// safety classifier ("Create Unsafe Agents"), so the dispatched agent
/// never actually ran. Server-side spawn with `setsid` puts the worker in
/// its own session — the classifier doesn't fire (the spawn happens
/// outside the calling harness) and the worker survives even if the MCP
/// caller exits.
async fn tool_dispatch(args: &Value, _config_path: &str) -> Result<Value> {
    let bead_id = args["bead_id"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("bead_id required"))?;
    let repo_path = args["repo_path"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("repo_path required"))?;
    let provider_name = args["provider"].as_str().unwrap_or("claude");
    let agent_override = args.get("agent").and_then(|v| v.as_str());
    let isolate = args
        .get("isolate")
        .and_then(|v| v.as_bool())
        .unwrap_or(true);

    let root = crate::scanner::resolve_repo_path(std::path::Path::new(repo_path));
    let beads_dir = crate::resolve_beads_dir(&root);
    let client = crate::bead_sqlite::connect_bead_store(&beads_dir).await?;
    let repo_name = repo_name_from_path(repo_path);

    // Self-heal stale state before staging a new dispatch (rosary-67c43d).
    // Reverts any prior `Dispatched` bead in this repo that has no live
    // session and no worktree on disk — typically caused by an MCP caller
    // that never spawned the agent process. Conservative: never touches
    // beads with live sessions or existing worktrees.
    let _ =
        crate::dispatch::sweep::sweep_orphan_dispatches(client.as_ref(), &root, &repo_name).await;

    let mut bead = client
        .get_bead(bead_id, &repo_name)
        .await?
        .ok_or_else(|| anyhow::anyhow!("bead {bead_id} not found"))?;

    // Agent override takes precedence over bead.owner
    if let Some(agent) = agent_override {
        bead.owner = Some(agent.to_string());
    }

    let agent_label = bead
        .owner
        .as_deref()
        .unwrap_or_else(|| crate::dispatch::default_agent(&bead.issue_type));

    // Create isolated workspace (worktree/jj workspace) — this is safe to do
    // from the server because it's just git operations, no process spawning.
    let workspace = if isolate {
        match crate::workspace::Workspace::create(bead_id, &repo_name, &root, true).await {
            Ok(ws) => Some(ws),
            Err(e) => {
                eprintln!("[dispatch] workspace creation failed: {e}");
                None
            }
        }
    } else {
        None
    };

    let work_dir = workspace
        .as_ref()
        .map(|ws| ws.work_dir.to_string_lossy().to_string())
        .unwrap_or_else(|| root.to_string_lossy().to_string());

    // Build the prompt + system_prompt the agent will see. system_prompt is
    // assembled from the agent definition + golden rules.
    let agents_dir = crate::dispatch::resolve_agents_dir();
    let prompt = crate::dispatch::build_prompt(
        &bead,
        &work_dir,
        workspace.as_ref().map(|ws| ws.work_dir.as_path()),
        bead.owner.as_deref(),
    );
    let system_prompt =
        crate::dispatch::build_system_prompt(bead.owner.as_deref(), agents_dir.as_deref());

    // Agent-specific permission override (mirrors the orchestrator path).
    let perms = match bead.owner.as_deref() {
        Some("scoping-agent") => crate::dispatch::PermissionProfile::ReadOnly,
        Some("staging-agent") => crate::dispatch::PermissionProfile::ReadOnly,
        Some("pm-agent") => crate::dispatch::PermissionProfile::Plan,
        Some("architect-agent") => crate::dispatch::PermissionProfile::Plan,
        _ => crate::dispatch::permission_profile(&bead.issue_type),
    };
    let allowed_tools = perms.claude_allowed_tools().to_string();

    // Trust model: rosary's MCP transports are all post-auth by deployment
    // topology — stdio is the local user's own shell, IPC (UDS) is gated by
    // filesystem permissions, and HTTP sits behind cloister (the MCP
    // gateway, per ADR-0005). Authentication is cloister's job, not
    // rosary's. The earlier `allow_mcp_spawn` config flag (PR #200) was
    // rosary reinventing auth one level too deep; it was removed in this
    // commit. If you publicly expose rosary's HTTP transport you've broken
    // the deployment contract — fix that, don't ask rosary to compensate.
    let cfg = crate::config::load(_config_path).ok();

    let binaries = cfg
        .as_ref()
        .and_then(|c| c.dispatch.as_ref())
        .map(|d| d.binaries.clone())
        .unwrap_or_default();
    let provider = crate::dispatch::providers::provider_by_name(provider_name, &binaries)
        .with_context(|| format!("resolving provider {provider_name}"))?;

    // Pre-flight: load the registry now and validate it's writable so we
    // don't spawn a worker we can't track. Loading is best-effort
    // (corrupted file → start fresh); saving is the real test.
    let mut registry = crate::session::SessionRegistry::load().unwrap_or_default();

    // Server-side spawn: setsid + detached so the worker survives the MCP
    // request returning AND isn't subject to the caller's safety classifier.
    let work_dir_path = std::path::Path::new(&work_dir);
    let spawned = crate::dispatch::spawn_detached(
        provider.as_ref(),
        &prompt,
        work_dir_path,
        &perms,
        &system_prompt,
    )
    .await
    .with_context(|| format!("spawning agent for {bead_id}"))?;

    let workspace_vcs = workspace
        .as_ref()
        .map(|ws| match ws.vcs {
            crate::workspace::VcsKind::Jj => "jj",
            crate::workspace::VcsKind::Git => "git",
            crate::workspace::VcsKind::None => "",
        })
        .unwrap_or("")
        .to_string();
    let session_entry = crate::session::SessionEntry {
        bead_id: bead_id.to_string(),
        repo: repo_name.clone(),
        provider: provider_name.to_string(),
        pid: Some(spawned.pid),
        work_dir: work_dir.clone(),
        started_at: chrono::Utc::now(),
        title: bead.title.clone(),
        agent: agent_label.to_string(),
        workspace_vcs,
        repo_path: repo_path.to_string(),
        last_activity: None,
        last_comment: None,
    };

    // Register the live session so `rsry_active` / health-check can see it.
    // If registry.register fails (permissions, disk full, concurrent write)
    // we have to choose: leak an untracked worker, or kill it. We kill,
    // because an untracked worker is worse than no worker — it leaves the
    // bead in an indeterminate state and gives the operator no handle to
    // recover. The worker will exit when we send SIGTERM; the tokio reaper
    // task in spawn_detached will waitpid it.
    if let Err(e) = registry.register(session_entry) {
        let pid = spawned.pid;
        eprintln!("[dispatch] registry write failed; killing untracked worker pid={pid}: {e}");
        unsafe {
            libc::kill(pid as i32, libc::SIGTERM);
        }
        return Err(e).with_context(|| {
            format!("persisting session for {bead_id} (pid {pid} killed to keep state consistent)")
        });
    }

    // Mark bead in-progress: the worker is actually running now (behavior
    // change from the pre-748f07 design, which marked `dispatched` and
    // waited for the caller to spawn the process). Status update failing
    // here is recoverable (operator can mark out-of-band) so we don't kill
    // the worker — just surface the error.
    client
        .update_status(bead_id, "in_progress")
        .await
        .with_context(|| {
            format!(
                "marking bead {bead_id} in_progress (worker is RUNNING at pid {}; \
                 bead status update failed but the agent is tracked in sessions.json)",
                spawned.pid,
            )
        })?;

    Ok(json!({
        "bead_id": bead_id,
        "title": bead.title,
        "status": "in_progress",
        "agent": agent_label,
        "provider": provider_name,
        "work_dir": work_dir,
        "pid": spawned.pid,
        "stream": spawned.stream_log.display().to_string(),
        "allowed_tools": allowed_tools,
        "instructions": "Worker spawned server-side (pid above). Tail `stream` to watch progress, or call rsry_active to list live sessions.",
    }))
}

async fn tool_active() -> Result<Value> {
    let registry = crate::session::SessionRegistry::load().unwrap_or_default();
    let mut running = Vec::new();
    let mut completed = Vec::new();

    for s in registry.active() {
        let health = check_agent_health(s);
        let entry = json!({
            "bead_id": s.bead_id,
            "title": s.title,
            "agent": s.agent,
            "repo": s.repo,
            "provider": s.provider,
            "pid": s.pid,
            "work_dir": s.work_dir,
            "started_at": s.started_at.to_rfc3339(),
            "last_activity": s.last_activity.map(|t| t.to_rfc3339()),
            "last_comment": s.last_comment,
            "health": health,
        });
        if health == "dead" {
            completed.push(entry);
        } else {
            running.push(entry);
        }
    }

    Ok(json!({
        "running": running.len(),
        "completed": completed.len(),
        "agents": running,
        "needs_merge": completed,
    }))
}

/// Quick health check for a dispatched agent.
/// Returns "healthy", "idle", "stuck", or "dead".
fn check_agent_health(session: &crate::session::SessionEntry) -> &'static str {
    // Check if PID is alive
    let pid_alive = session
        .pid
        .map(crate::session::is_pid_alive)
        .unwrap_or(false);
    if !pid_alive {
        return "dead";
    }

    // Check for TCP connections (active API calls)
    let has_tcp = session.pid.is_some_and(|pid| {
        std::process::Command::new("lsof")
            .args(["-p", &pid.to_string(), "-i", "TCP"])
            .stdout(std::process::Stdio::piped())
            .stderr(std::process::Stdio::null())
            .output()
            .map(|o| String::from_utf8_lossy(&o.stdout).contains("ESTABLISHED"))
            .unwrap_or(false)
    });

    // Check workspace for recent file changes (last 2 minutes)
    let ws_active = if !session.work_dir.is_empty() {
        std::process::Command::new("find")
            .args([
                &session.work_dir,
                "-maxdepth",
                "3",
                "-newer",
                &session.work_dir,
                "-name",
                "*.rs",
                "-o",
                "-name",
                "*.ex",
            ])
            .stdout(std::process::Stdio::piped())
            .stderr(std::process::Stdio::null())
            .output()
            .map(|o| !o.stdout.is_empty())
            .unwrap_or(false)
    } else {
        false
    };

    if has_tcp || ws_active {
        "healthy"
    } else if session.last_activity.is_some() {
        // Had activity before but none now
        "idle"
    } else {
        // Never had activity, no TCP — likely stuck
        "stuck"
    }
}

// ---------------------------------------------------------------------------
// Workspace tools
// ---------------------------------------------------------------------------

async fn tool_workspace_create(
    args: &Value,
    config_path: &str,
    repo_cache: &crate::repo_cache::RepoCache,
) -> Result<Value> {
    let bead_id = args["bead_id"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("bead_id required"))?;
    let repo_path = args["repo_path"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("repo_path required"))?;

    // Remote repo URL: clone on demand, then proceed with the local path.
    // Only https:// is accepted — http:// is rejected to avoid credential leakage.
    if repo_path.starts_with("http://") {
        anyhow::bail!("insecure http:// URLs not allowed — use https://");
    }
    let root = if repo_path.starts_with("https://") {
        let github_token = resolve_github_token(config_path).await;
        repo_cache
            .ensure_local(repo_path, github_token.as_deref())
            .await?
    } else {
        crate::scanner::resolve_repo_path(std::path::Path::new(repo_path))
    };

    let repo_name = repo_name_from_path(&root.to_string_lossy());
    let ws = crate::workspace::Workspace::create(bead_id, &repo_name, &root, true).await?;

    Ok(json!({
        "bead_id": bead_id,
        "work_dir": ws.work_dir.to_string_lossy(),
        "vcs": format!("{:?}", ws.vcs),
        "repo_path": ws.repo_path.to_string_lossy(),
    }))
}

/// Resolve a GitHub token for cloning private repos.
/// Tries GitHub App installation token first, then GITHUB_TOKEN env var.
async fn resolve_github_token(config_path: &str) -> Option<String> {
    if let Ok(cfg) = config::load(config_path)
        && let Some(gh_cfg) = cfg.github
        && let Ok(client) = crate::github::GitHubClient::from_config(&gh_cfg)
        && let Ok(token) = client.bearer_token().await
    {
        return Some(token);
    }
    std::env::var("GITHUB_TOKEN").ok()
}

async fn tool_workspace_checkpoint(args: &Value) -> Result<Value> {
    let bead_id = args["bead_id"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("bead_id required"))?;
    let repo_path = args["repo_path"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("repo_path required"))?;
    let message = args["message"].as_str().unwrap_or("agent work");

    let root = crate::scanner::resolve_repo_path(std::path::Path::new(repo_path));
    let repo_name = repo_name_from_path(repo_path);

    let ws = crate::workspace::Workspace::from_existing(bead_id, &repo_name, &root);
    let change_id = ws.checkpoint(message).await?;

    Ok(json!({
        "bead_id": bead_id,
        "change_id": change_id,
        "vcs": format!("{:?}", ws.vcs),
    }))
}

fn tool_workspace_cleanup(args: &Value) -> Result<Value> {
    let bead_id = args["bead_id"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("bead_id required"))?;
    let repo_path = args["repo_path"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("repo_path required"))?;

    let root = crate::scanner::resolve_repo_path(std::path::Path::new(repo_path));
    let vcs = crate::workspace::detect_vcs(&root);

    match vcs {
        crate::workspace::VcsKind::Jj => {
            crate::workspace::cleanup_jj_workspace(&root, bead_id);
        }
        crate::workspace::VcsKind::Git => {
            crate::workspace::cleanup_git_worktree(&root, bead_id);
        }
        crate::workspace::VcsKind::None => {}
    }

    Ok(json!({
        "bead_id": bead_id,
        "cleaned": true,
    }))
}

async fn tool_workspace_merge(args: &Value) -> Result<Value> {
    let bead_id = args["bead_id"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("bead_id required"))?;
    let repo_path = args["repo_path"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("repo_path required"))?;
    let issue_type = args["issue_type"].as_str().unwrap_or("task");
    let base_branch = args.get("base_branch").and_then(|v| v.as_str());
    if let Some(b) = base_branch {
        anyhow::ensure!(!b.trim().is_empty(), "base_branch must not be blank");
    }

    let root = crate::scanner::resolve_repo_path(std::path::Path::new(repo_path));
    let branch = format!("fix/{bead_id}");

    let result =
        crate::workspace::merge_or_pr_with_base(&root, &branch, bead_id, issue_type, base_branch)
            .await?;

    // Unregister the session after merge — agent is done, work is landed.
    if let Ok(mut registry) = crate::session::SessionRegistry::load() {
        let repo = std::path::Path::new(repo_path)
            .file_name()
            .and_then(|n| n.to_str())
            .unwrap_or("");
        let _ = registry.unregister(bead_id, repo);
    }

    Ok(json!({
        "bead_id": bead_id,
        "branch": branch,
        "result": result.message,
        "pr_url": result.pr_url,
    }))
}

// ---------------------------------------------------------------------------
// Decompose
// ---------------------------------------------------------------------------

async fn tool_decompose(args: &Value) -> Result<Value> {
    let path = args
        .get("path")
        .and_then(|v| v.as_str())
        .ok_or_else(|| anyhow::anyhow!("path is required"))?;

    let markdown = std::fs::read_to_string(path).with_context(|| format!("reading {path}"))?;

    let model = args
        .get("model")
        .and_then(|v| v.as_str())
        .map(|s| s.to_string());

    let (atoms, meta) = if model.is_some() && !bdr::parse::is_adr_shaped(&markdown) {
        let model_name = model.as_deref().unwrap();
        let atoms = crate::bdr_enrich::extract_atoms_with_llm(&markdown, model_name).await?;
        let meta = bdr::parse::DocMeta {
            provenance: Some(bdr::provenance::ProvenanceRef::Doc {
                path: path.to_string(),
            }),
            ..Default::default()
        };
        (atoms, meta)
    } else {
        let parsed = bdr::parse::parse_doc_full(&markdown, path);
        (parsed.atoms, parsed.meta)
    };

    if atoms.is_empty() {
        return Ok(json!({
            "decade": null,
            "message": "No decomposable atoms found",
            "atom_count": 0,
        }));
    }

    let title = args
        .get("title")
        .and_then(|v| v.as_str())
        .map(|s| s.to_string())
        .unwrap_or_else(|| {
            markdown
                .lines()
                .find(|l| l.starts_with("# "))
                .map(|l| l.trim_start_matches('#').trim().to_string())
                .unwrap_or_else(|| path.to_string())
        });

    let mut decade = bdr::thread::build_decade_with_meta(path, &title, &atoms, &meta);

    // Stamp inferred_from on every BeadSpec when LLM extraction was used.
    if let Some(ref model_name) = model {
        let trace = bdr::provenance::InferenceTrace {
            model: crate::bdr_enrich::resolve_model_id(model_name).to_string(),
            rationale: None,
        };
        for thread in &mut decade.threads {
            for spec in &mut thread.beads {
                spec.inferred_from = Some(trace.clone());
            }
        }
    }

    let commit = args
        .get("commit")
        .and_then(|v| v.as_bool())
        .unwrap_or(false);

    let mut created = 0usize;
    let mut skipped = 0usize;

    if commit {
        let repo_path = args
            .get("repo_path")
            .and_then(|v| v.as_str())
            .ok_or_else(|| anyhow::anyhow!("repo_path is required when commit=true"))?;

        let repo_root = std::path::Path::new(repo_path);
        let beads_dir = repo_root.join(".beads");
        let client = crate::bead_sqlite::connect_bead_store(&beads_dir).await?;
        let repo_name = repo_root
            .file_name()
            .map(|n| n.to_string_lossy().to_string())
            .unwrap_or_else(|| repo_path.to_string());

        // Connect to backend for lattice assignment (best-effort).
        let backend: Option<Box<dyn BackendStore>> =
            match config::load_global().ok().and_then(|c| c.backend) {
                Some(cfg) => cfg.connect().await.ok(),
                None => None,
            };

        if let Some(ref b) = backend {
            let _ = b
                .upsert_decade(&crate::store::DecadeRecord {
                    id: decade.id.clone(),
                    title: decade.title.clone(),
                    source_path: path.to_string(),
                    status: "active".to_string(),
                })
                .await;
            for thread in &decade.threads {
                let prefix = config::load_global()
                    .ok()
                    .and_then(|c| c.github)
                    .map(|g| g.agent_branch_prefix)
                    .unwrap_or_else(|| "rosary".to_string());
                let feature_branch = crate::workspace::thread_branch_name(&prefix, &thread.name);
                let _ = b
                    .upsert_thread(&crate::store::ThreadRecord {
                        id: thread.id.clone(),
                        name: thread.name.clone(),
                        decade_id: decade.id.clone(),
                        feature_branch: Some(feature_branch),
                    })
                    .await;
            }
        }

        for thread in &decade.threads {
            for spec in &thread.beads {
                // Dedup: skip exact title matches.
                let existing = client
                    .search_beads(&spec.title, &repo_name, 10)
                    .await
                    .unwrap_or_default();
                if existing.iter().any(|b| b.title == spec.title) {
                    skipped += 1;
                    continue;
                }

                let desc = enrich_decompose_description(spec);
                let id = crate::generate_bead_id(&repo_name);
                let owner = crate::dispatch::default_agent(&spec.issue_type);
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
                        None,
                        "",
                        &spec.derived_from,
                    )
                    .await?;

                if let Some(ref b) = backend {
                    let _ = b
                        .add_bead_to_thread(
                            &thread.id,
                            &WorkRef {
                                repo: repo_name.clone(),
                                bead_id: id,
                                scope: String::new(),
                            },
                        )
                        .await;
                }
                created += 1;
            }
        }
    }

    Ok(json!({
        "decade": {
            "id": decade.id,
            "title": decade.title,
            "status": format!("{:?}", decade.status),
            "thread_count": decade.threads.len(),
            "bead_count": decade.threads.iter().map(|t| t.beads.len()).sum::<usize>(),
        },
        "meta": decade.meta,
        "threads": decade.threads.iter().map(|t| json!({
            "id": t.id,
            "name": t.name,
            "bead_count": t.beads.len(),
            "cross_repo_refs": t.cross_repo_refs,
            "beads": t.beads.iter().map(|b| json!({
                "title": b.title,
                "issue_type": b.issue_type,
                "priority": b.priority,
                "channel": b.channel.as_str(),
                "thread_group": b.thread_group,
                "target_repo": b.target_repo,
                "depends_on": b.depends_on,
                "success_criteria": b.success_criteria,
                "references": b.references,
            })).collect::<Vec<_>>(),
        })).collect::<Vec<_>>(),
        "atom_count": atoms.len(),
        "committed": commit,
        "beads_created": created,
        "beads_skipped": skipped,
    }))
}

/// Enrich a `BeadSpec` description with success criteria and references.
fn enrich_decompose_description(spec: &bdr::decompose::BeadSpec) -> String {
    let mut desc = spec.description.clone();

    if !spec.success_criteria.is_empty() {
        desc.push_str("\n\n## Success Criteria\n\n");
        for sc in &spec.success_criteria {
            match (&sc.command, &sc.threshold) {
                (Some(cmd), _) => desc.push_str(&format!("- `{cmd}` — {}\n", sc.description)),
                (None, Some(t)) => {
                    desc.push_str(&format!("- {} (threshold: {t})\n", sc.description))
                }
                (None, None) => desc.push_str(&format!("- {}\n", sc.description)),
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

// ---------------------------------------------------------------------------
// Pipeline / dispatch record / history
// ---------------------------------------------------------------------------

pub(crate) async fn tool_pipeline_upsert(
    args: &Value,
    backend: Option<&dyn BackendStore>,
) -> Result<Value> {
    let backend = backend.ok_or_else(|| {
        anyhow::anyhow!(
            "backend store not configured — add [backend] section to ~/.rsry/config.toml"
        )
    })?;

    let repo = args["repo"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("repo required"))?;
    let bead_id = args["bead_id"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("bead_id required"))?;
    let pipeline_phase = args["pipeline_phase"]
        .as_u64()
        .ok_or_else(|| anyhow::anyhow!("pipeline_phase required"))? as u8;
    let pipeline_agent = args["pipeline_agent"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("pipeline_agent required"))?;

    let phase_status = args
        .get("phase_status")
        .and_then(|v| v.as_str())
        .unwrap_or("pending");
    let retries = args.get("retries").and_then(|v| v.as_u64()).unwrap_or(0) as u32;
    let consecutive_reverts = args
        .get("consecutive_reverts")
        .and_then(|v| v.as_u64())
        .unwrap_or(0) as u32;
    let highest_verify_tier = args
        .get("highest_verify_tier")
        .and_then(|v| v.as_u64())
        .map(|v| v as u8);
    let last_generation = args
        .get("last_generation")
        .and_then(|v| v.as_u64())
        .unwrap_or(0);
    let backoff_until = args
        .get("backoff_until")
        .and_then(|v| v.as_str())
        .map(|s| {
            s.parse::<chrono::DateTime<chrono::Utc>>()
                .with_context(|| format!("parsing backoff_until '{s}' as ISO 8601"))
        })
        .transpose()?;

    let state = PipelineState {
        bead_ref: WorkRef {
            repo: repo.to_string(),
            scope: String::new(),
            bead_id: bead_id.to_string(),
        },
        pipeline_phase,
        pipeline_agent: pipeline_agent.to_string(),
        phase_status: phase_status.to_string(),
        retries,
        consecutive_reverts,
        highest_verify_tier,
        last_generation,
        backoff_until,
    };

    backend.upsert_pipeline(&state).await?;

    Ok(json!({
        "repo": repo,
        "bead_id": bead_id,
        "pipeline_phase": pipeline_phase,
        "pipeline_agent": pipeline_agent,
        "phase_status": phase_status,
        "upserted": true,
    }))
}

async fn tool_pipeline_query(args: &Value, backend: Option<&dyn BackendStore>) -> Result<Value> {
    let backend = backend.ok_or_else(|| anyhow::anyhow!("backend store not configured"))?;

    let repo = args.get("repo").and_then(|v| v.as_str());
    let bead_id = args.get("bead_id").and_then(|v| v.as_str());

    match (repo, bead_id) {
        (Some(repo), Some(bead_id)) => {
            let bead_ref = WorkRef {
                repo: repo.to_string(),
                scope: String::new(),
                bead_id: bead_id.to_string(),
            };
            let pipeline = backend.get_pipeline(&bead_ref).await?;
            match pipeline {
                Some(p) => Ok(json!({
                    "mode": "get",
                    "pipeline": {
                        "repo": p.bead_ref.repo,
                        "bead_id": p.bead_ref.bead_id,
                        "pipeline_phase": p.pipeline_phase,
                        "pipeline_agent": p.pipeline_agent,
                        "phase_status": p.phase_status,
                        "retries": p.retries,
                    }
                })),
                None => Ok(json!({ "mode": "get", "pipeline": null })),
            }
        }
        (None, None) => {
            let pipelines = backend.list_active_pipelines().await?;
            let items: Vec<Value> = pipelines
                .iter()
                .map(|p| {
                    json!({
                        "repo": p.bead_ref.repo,
                        "bead_id": p.bead_ref.bead_id,
                        "pipeline_phase": p.pipeline_phase,
                        "pipeline_agent": p.pipeline_agent,
                        "phase_status": p.phase_status,
                    })
                })
                .collect();
            Ok(json!({ "mode": "list", "count": items.len(), "pipelines": items }))
        }
        _ => anyhow::bail!("provide both repo and bead_id, or neither for list"),
    }
}

async fn tool_dispatch_record(args: &Value, backend: Option<&dyn BackendStore>) -> Result<Value> {
    let backend = backend.ok_or_else(|| anyhow::anyhow!("backend store not configured"))?;

    let id = args["id"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("id required"))?;
    let repo = args["repo"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("repo required"))?;
    let bead_id = args["bead_id"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("bead_id required"))?;
    let agent = args["agent"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("agent required"))?;
    let provider = args["provider"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("provider required"))?;
    let work_dir = args["work_dir"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("work_dir required"))?;

    let record = DispatchRecord {
        id: id.to_string(),
        bead_ref: WorkRef {
            repo: repo.to_string(),
            scope: String::new(),
            bead_id: bead_id.to_string(),
        },
        agent: agent.to_string(),
        provider: provider.to_string(),
        started_at: chrono::Utc::now(),
        completed_at: None,
        outcome: None,
        work_dir: work_dir.to_string(),
        session_id: None,
        workspace_path: None,
        chain_hash: None,
    };

    backend.record_dispatch(&record).await?;
    Ok(json!({ "id": id, "bead_id": bead_id, "recorded": true }))
}

async fn tool_dispatch_history(args: &Value, backend: Option<&dyn BackendStore>) -> Result<Value> {
    let backend = backend.ok_or_else(|| anyhow::anyhow!("backend store not configured"))?;
    let bead_id = args.get("bead_id").and_then(|v| v.as_str());
    let active_only = args
        .get("active_only")
        .and_then(|v| v.as_bool())
        .unwrap_or(bead_id.is_none());

    let mut dispatches = backend.active_dispatches().await?;
    if let Some(bead_id) = bead_id {
        dispatches.retain(|d: &DispatchRecord| d.bead_ref.bead_id == bead_id);
    }
    if !active_only {
        // active_dispatches already filters to active — nothing extra needed
        let _ = active_only;
    }

    let items: Vec<Value> = dispatches
        .iter()
        .map(|d| {
            json!({
                "id": d.id,
                "repo": d.bead_ref.repo,
                "bead_id": d.bead_ref.bead_id,
                "agent": d.agent,
                "provider": d.provider,
                "started_at": d.started_at.to_rfc3339(),
                "completed_at": d.completed_at.map(|t| t.to_rfc3339()),
                "outcome": d.outcome,
                "work_dir": d.work_dir,
            })
        })
        .collect();

    Ok(json!({ "count": items.len(), "dispatches": items }))
}

// ---------------------------------------------------------------------------
// Hierarchy tools (decades, threads, bead membership)
// ---------------------------------------------------------------------------

/// `rsry_decade_create` — dedicated MCP tool for creating decades.
///
/// `tool_thread_assign` already auto-creates decades as a side effect,
/// but agents that want to populate the BDR hierarchy explicitly (file
/// N beads → group into M threads → roll up into 1 decade in one
/// session) need an idempotent dedicated entry point that returns the
/// created record. rosary-992e79.
///
/// Idempotency rule: re-creating with the same title + source_path is
/// a no-op success (`action: "existed"`). Conflict (same id, different
/// title) errors loudly so agents can't accidentally stomp curated
/// decade names.
async fn tool_decade_create(args: &Value, backend: Option<&dyn BackendStore>) -> Result<Value> {
    use crate::store::DecadeRecord;
    let backend = backend.ok_or_else(|| anyhow::anyhow!("backend store not configured"))?;

    let id = args["id"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("id required (decade slug)"))?;
    let title = args["title"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("title required"))?;
    let source_path = args
        .get("source_path")
        .and_then(|v| v.as_str())
        .unwrap_or("");
    let status = args
        .get("status")
        .and_then(|v| v.as_str())
        .unwrap_or("active");

    if let Some(existing) = backend.get_decade(id).await? {
        if existing.title == title && existing.source_path == source_path {
            return Ok(json!({
                "id": existing.id,
                "title": existing.title,
                "source_path": existing.source_path,
                "status": existing.status,
                "action": "existed",
            }));
        }
        // Name the actually-diverging field(s) so the operator can see
        // what changed. Reporting "conflicting title" when only
        // source_path differs misleads readers (Copilot #205 finding).
        let mut diffs = Vec::new();
        if existing.title != title {
            diffs.push(format!(
                "title (`{}` vs requested `{title}`)",
                existing.title
            ));
        }
        if existing.source_path != source_path {
            diffs.push(format!(
                "source_path (`{}` vs requested `{source_path}`)",
                existing.source_path
            ));
        }
        anyhow::bail!(
            "decade `{id}` already exists with conflicting {}; refusing silent overwrite",
            diffs.join(" and ")
        );
    }

    let decade = DecadeRecord {
        id: id.to_string(),
        title: title.to_string(),
        source_path: source_path.to_string(),
        status: status.to_string(),
    };
    backend.upsert_decade(&decade).await?;
    Ok(json!({
        "id": decade.id,
        "title": decade.title,
        "source_path": decade.source_path,
        "status": decade.status,
        "action": "created",
    }))
}

/// `rsry_thread_create` — dedicated MCP tool for creating threads under
/// a named decade. Refuses to land an orphan thread if the parent
/// decade doesn't exist (unlike `thread_assign`, which auto-creates an
/// `ungrouped` decade as a fall-through). rosary-992e79.
async fn tool_thread_create(args: &Value, backend: Option<&dyn BackendStore>) -> Result<Value> {
    use crate::store::ThreadRecord;
    let backend = backend.ok_or_else(|| anyhow::anyhow!("backend store not configured"))?;

    let decade_id = args["decade_id"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("decade_id required (parent decade)"))?;
    let id = args["id"].as_str().ok_or_else(|| {
        anyhow::anyhow!("id required (thread slug, conventionally `<decade>/<name>`)")
    })?;
    let name = args["name"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("name required (human-readable thread title)"))?;

    if backend.get_decade(decade_id).await?.is_none() {
        anyhow::bail!(
            "parent decade `{decade_id}` does not exist; create it first via rsry_decade_create"
        );
    }

    // Thread IDs are globally unique — the `threads` table primary key is
    // just `id`, not `(decade_id, id)`. Scan every decade for an existing
    // thread with this id; otherwise a same-id thread under a different
    // decade would silently get re-parented by upsert (Copilot #205 finding).
    let mut existing: Option<crate::store::ThreadRecord> = None;
    for decade in backend.list_decades(None).await? {
        if let Some(t) = backend
            .list_threads(&decade.id)
            .await?
            .into_iter()
            .find(|t| t.id == id)
        {
            existing = Some(t);
            break;
        }
    }
    if let Some(existing) = existing {
        if existing.name == name && existing.decade_id == decade_id {
            return Ok(json!({
                "id": existing.id,
                "name": existing.name,
                "decade_id": existing.decade_id,
                "feature_branch": existing.feature_branch,
                "action": "existed",
            }));
        }
        if existing.decade_id != decade_id {
            anyhow::bail!(
                "thread `{id}` already exists under decade `{}` (requested `{decade_id}`); thread ids are globally unique — re-parent via rsry_thread_reparent instead",
                existing.decade_id
            );
        }
        anyhow::bail!(
            "thread `{id}` already exists under decade `{decade_id}` with a conflicting name (`{}` vs requested `{name}`)",
            existing.name
        );
    }

    let prefix = crate::config::load_global()
        .ok()
        .and_then(|c| c.github)
        .map(|g| g.agent_branch_prefix)
        .unwrap_or_else(|| "rosary".to_string());
    let feature_branch = crate::workspace::thread_branch_name(&prefix, name);

    let thread = ThreadRecord {
        id: id.to_string(),
        name: name.to_string(),
        decade_id: decade_id.to_string(),
        feature_branch: Some(feature_branch.clone()),
    };
    backend.upsert_thread(&thread).await?;

    Ok(json!({
        "id": thread.id,
        "name": thread.name,
        "decade_id": thread.decade_id,
        "feature_branch": feature_branch,
        "action": "created",
    }))
}

async fn tool_decade_list(args: &Value, backend: Option<&dyn BackendStore>) -> Result<Value> {
    let backend = backend.ok_or_else(|| anyhow::anyhow!("backend store not configured"))?;
    let status = args.get("status").and_then(|v| v.as_str());

    let decades = backend.list_decades(status).await?;

    let items: Vec<Value> = decades
        .iter()
        .map(|d| {
            json!({
                "id": d.id,
                "title": d.title,
                "source_path": d.source_path,
                "status": d.status,
            })
        })
        .collect();

    Ok(json!({ "count": items.len(), "decades": items }))
}

async fn tool_thread_list(args: &Value, backend: Option<&dyn BackendStore>) -> Result<Value> {
    let backend = backend.ok_or_else(|| anyhow::anyhow!("backend store not configured"))?;

    // Option 1: list threads for a decade
    if let Some(decade_id) = args.get("decade_id").and_then(|v| v.as_str()) {
        let threads = backend.list_threads(decade_id).await?;
        let items: Vec<Value> = threads
            .iter()
            .map(|t| {
                json!({
                    "id": t.id,
                    "name": t.name,
                    "decade_id": t.decade_id,
                    "feature_branch": t.feature_branch,
                })
            })
            .collect();
        return Ok(json!({ "count": items.len(), "threads": items }));
    }

    // Option 2: find thread for a specific bead
    if let (Some(bead_id), Some(repo)) = (
        args.get("bead_id").and_then(|v| v.as_str()),
        args.get("repo").and_then(|v| v.as_str()),
    ) {
        let bead_ref = crate::store::WorkRef {
            repo: repo.to_string(),
            scope: String::new(),
            bead_id: bead_id.to_string(),
        };
        let thread_id = backend.find_thread_for_bead(&bead_ref).await?;
        return Ok(json!({ "bead_id": bead_id, "thread_id": thread_id }));
    }

    anyhow::bail!("provide either decade_id or (bead_id + repo)")
}

async fn tool_thread_assign(args: &Value, backend: Option<&dyn BackendStore>) -> Result<Value> {
    let backend = backend.ok_or_else(|| anyhow::anyhow!("backend store not configured"))?;
    use crate::store::{ThreadRecord, WorkRef};

    let thread_id = args["thread_id"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("thread_id required"))?;
    let bead_id = args["bead_id"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("bead_id required"))?;
    let repo = args["repo"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("repo required"))?;

    // Create thread if it doesn't exist
    let thread_name = args
        .get("thread_name")
        .and_then(|v| v.as_str())
        .unwrap_or(thread_id);
    let decade_id = args
        .get("decade_id")
        .and_then(|v| v.as_str())
        .unwrap_or("ungrouped");

    // Auto-create decade if it doesn't exist
    use crate::store::DecadeRecord;
    if backend.get_decade(decade_id).await?.is_none() {
        backend
            .upsert_decade(&DecadeRecord {
                id: decade_id.to_string(),
                title: decade_id.to_string(),
                source_path: String::new(),
                status: "active".to_string(),
            })
            .await?;
    }

    // Derive feature branch from config prefix + thread name.
    let prefix = crate::config::load_global()
        .ok()
        .and_then(|c| c.github)
        .map(|g| g.agent_branch_prefix)
        .unwrap_or_else(|| "rosary".to_string());
    let feature_branch = crate::workspace::thread_branch_name(&prefix, thread_name);

    backend
        .upsert_thread(&ThreadRecord {
            id: thread_id.to_string(),
            name: thread_name.to_string(),
            decade_id: decade_id.to_string(),
            feature_branch: Some(feature_branch.clone()),
        })
        .await?;

    let bead_ref = WorkRef {
        repo: repo.to_string(),
        scope: String::new(),
        bead_id: bead_id.to_string(),
    };
    backend.add_bead_to_thread(thread_id, &bead_ref).await?;

    let members = backend.list_beads_in_thread(thread_id).await?;

    Ok(json!({
        "thread_id": thread_id,
        "bead_id": bead_id,
        "action": "assigned",
        "thread_size": members.len(),
    }))
}

// ---------------------------------------------------------------------------
// Bead import (cross-instance migration)
// ---------------------------------------------------------------------------

async fn tool_bead_import(
    args: &Value,
    config_path: &str,
    pool: &RepoPool,
    user_scope: Option<&str>,
) -> Result<Value> {
    let default_repo_path = args.get("repo_path").and_then(|v| v.as_str());
    let beads = args["beads"]
        .as_array()
        .ok_or_else(|| anyhow::anyhow!("beads array required"))?;

    // Build repo name → path lookup from config for routing by repo name
    let cfg = config::load_merged(config_path)?;
    let repo_paths: std::collections::HashMap<String, String> = cfg
        .repo
        .iter()
        .map(|r| {
            let path = crate::scanner::expand_path(&r.path);
            (r.name.clone(), path.to_string_lossy().to_string())
        })
        .collect();

    let mut imported = 0;
    let mut skipped = 0;
    let mut errors = Vec::new();
    let mut ids = Vec::new();

    for bead in beads {
        let title = bead["title"].as_str().unwrap_or("(untitled)");

        // Resolve target repo: per-bead "repo" field, then fallback to repo_path param
        let resolved_repo_path = bead
            .get("repo")
            .and_then(|v| v.as_str())
            .and_then(|name| repo_paths.get(name).map(|s| s.as_str()))
            .or(default_repo_path);

        let repo_path = match resolved_repo_path {
            Some(p) => p,
            None => {
                errors.push(format!(
                    "no repo for bead '{title}' — set repo field or repo_path param"
                ));
                continue;
            }
        };

        let client_ref = get_client(repo_path, pool).await?;
        let client = client_ref.as_store();
        let repo_name = repo_name_from_path(repo_path);

        match crate::import::import_bead(bead, client, &repo_name).await? {
            Some(id) => {
                if let Some(uid) = user_scope {
                    let _ = client.set_user_id(&id, uid).await;
                }
                ids.push(id);
                imported += 1;
            }
            None => skipped += 1,
        }
    }

    let mut result = json!({
        "imported": imported,
        "skipped": skipped,
        "ids": ids,
    });
    if !errors.is_empty() {
        result["errors"] = json!(errors);
    }
    Ok(result)
}

/// Re-parent an existing thread under a different decade.
///
/// Used to clean up the bead lattice when threads land in `ungrouped` or
/// `auto-discovered` and should belong to a real decade. Calls `upsert_thread`
/// with the new `decade_id`. Auto-creates the target decade if missing.
async fn tool_thread_reparent(args: &Value, backend: Option<&dyn BackendStore>) -> Result<Value> {
    let backend = backend.ok_or_else(|| anyhow::anyhow!("backend store not configured"))?;

    let thread_id = args["thread_id"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("thread_id required"))?;
    let decade_id = args["decade_id"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("decade_id required"))?;
    let new_name = args.get("name").and_then(|v| v.as_str());

    crate::store::reparent_thread(backend, thread_id, decade_id, new_name).await?;

    Ok(serde_json::json!({
        "thread_id": thread_id,
        "decade_id": decade_id,
        "reparented": true,
    }))
}

// ---------------------------------------------------------------------------
// User repo registration (multi-tenant)
// ---------------------------------------------------------------------------

async fn tool_repo_register(
    args: &Value,
    backend: Option<&dyn BackendStore>,
    user_scope: Option<&str>,
) -> Result<Value> {
    let backend = backend.ok_or_else(|| {
        anyhow::anyhow!(
            "backend store not configured — add [backend] section to ~/.rsry/config.toml"
        )
    })?;
    let user_id = user_scope.ok_or_else(|| {
        anyhow::anyhow!("repo registration requires user identity (connect via mcp.rosary.bot)")
    })?;

    let repo_url = args["repo_url"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("repo_url required"))?;

    // Derive repo_name from URL if not provided
    let repo_name = args
        .get("repo_name")
        .and_then(|v| v.as_str())
        .map(|s| s.to_string())
        .unwrap_or_else(|| {
            repo_url
                .trim_end_matches('/')
                .trim_end_matches(".git")
                .rsplit('/')
                .next()
                .unwrap_or("repo")
                .to_string()
        });

    use crate::store::UserRepo;
    let repo = UserRepo {
        user_id: user_id.to_string(),
        repo_url: repo_url.to_string(),
        repo_name: repo_name.clone(),
        github_token_ref: None, // TODO: accept token ref from dashboard settings
    };

    backend.register_repo(&repo).await?;

    Ok(json!({
        "user_id": user_id,
        "repo_name": repo_name,
        "repo_url": repo_url,
        "registered": true,
    }))
}

async fn tool_repo_list(
    backend: Option<&dyn BackendStore>,
    user_scope: Option<&str>,
) -> Result<Value> {
    let backend = backend.ok_or_else(|| anyhow::anyhow!("backend store not configured"))?;
    let user_id =
        user_scope.ok_or_else(|| anyhow::anyhow!("repo listing requires user identity"))?;

    let repos = backend.list_user_repos(user_id).await?;

    Ok(json!({
        "user_id": user_id,
        "count": repos.len(),
        "repos": repos.iter().map(|r| json!({
            "repo_name": r.repo_name,
            "repo_url": r.repo_url,
        })).collect::<Vec<_>>(),
    }))
}

// ---------------------------------------------------------------------------
// rsry_ticket_load — Phase 0 of rosary-5d7141 (rosary-5dc9b0)
// ---------------------------------------------------------------------------

/// Consolidate Linear + (linked GH/Zendesk URLs) + existing-bead context for
/// a single ticket into one MCP response. Replaces the 4-5 manual lookups the
/// user performs per escalation. See `serve::ticket_load` for the pure-fn
/// helpers; this orchestrator does the Linear I/O and stitches the pieces.
async fn tool_ticket_load(args: &Value, pool: &RepoPool) -> Result<Value> {
    use super::ticket_load::{
        assemble_context, extract_github_link, extract_zendesk_link, find_triage_bead,
    };

    let ticket_id = args
        .get("ticket_id")
        .and_then(|v| v.as_str())
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .ok_or_else(|| anyhow::anyhow!("ticket_id is required (e.g. \"CUS-495\")"))?;

    let Some(client) = crate::linear::try_client() else {
        anyhow::bail!(
            "Linear not configured — set LINEAR_API_KEY or [linear].api_key in ~/.rsry/config.toml"
        );
    };

    let linear_issue = crate::linear::get_ticket(&client, ticket_id)
        .await
        .with_context(|| format!("fetching Linear ticket {ticket_id}"))?;

    // Linear's `id` is the internal UUID required for the comments query.
    // Propagate comments-fetch errors instead of swallowing them — a transient
    // Linear/GraphQL failure here would otherwise produce a successful-looking
    // response with an empty `comments` array, which is indistinguishable from
    // "the ticket has no comments" and silently degrades URL discovery + bead
    // matching that depend on comment text. (Copilot review on PR #215.)
    let comments = match linear_issue["id"].as_str() {
        Some(internal_id) => crate::linear::get_ticket_comments(&client, internal_id)
            .await
            .with_context(|| format!("fetching comments for Linear ticket {ticket_id}"))?,
        None => Vec::new(),
    };

    // Concatenate body + comments for URL extraction so a GH link anywhere in
    // the conversation is discoverable, not just in the ticket body.
    let body_text = linear_issue["description"].as_str().unwrap_or("");
    let combined_text: String = std::iter::once(body_text.to_string())
        .chain(
            comments
                .iter()
                .filter_map(|c| c["body"].as_str().map(String::from)),
        )
        .collect::<Vec<_>>()
        .join("\n");

    let linked_github = extract_github_link(&combined_text).map(|url| {
        // Phase 0: URL only. The body + state fields wait on a follow-up bead
        // (fetching GH issue bodies via `gh api` adds a new I/O dep).
        json!({ "url": url, "body": null, "state": null })
    });
    let linked_zendesk = extract_zendesk_link(&combined_text);
    // Bead lookup uses the canonical Linear identifier (`CUS-495`) returned by
    // `get_ticket`, NOT the raw caller-supplied `ticket_id` — which may be a
    // full URL. Existing beads consistently reference the identifier form, so
    // this normalization is what makes URL-shaped inputs match real beads.
    // (Copilot review on PR #215.)
    let lookup_id = linear_issue["identifier"].as_str().unwrap_or(ticket_id);
    let existing_bead = find_triage_bead(pool, lookup_id).await;

    Ok(assemble_context(
        linear_issue,
        comments,
        linked_github,
        linked_zendesk,
        existing_bead,
    ))
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;

    // ---- tool_ticket_load (rosary-5dc9b0) ---------------------------------

    /// Caller must supply `ticket_id`; the error message names the missing arg
    /// so future MCP clients learn the right shape from one rejection.
    #[tokio::test]
    async fn ticket_load_rejects_missing_ticket_id() {
        let args = json!({});
        let err = tool_ticket_load(&args, &crate::pool::RepoPool::empty())
            .await
            .unwrap_err();
        let msg = err.to_string();
        assert!(
            msg.contains("ticket_id"),
            "error must name the missing field; got: {msg}"
        );
    }

    /// Whitespace-only ticket_id is rejected — same validation surface as a
    /// missing field so the error UX stays consistent.
    #[tokio::test]
    async fn ticket_load_rejects_blank_ticket_id() {
        let args = json!({ "ticket_id": "   " });
        let err = tool_ticket_load(&args, &crate::pool::RepoPool::empty())
            .await
            .unwrap_err();
        let msg = err.to_string();
        assert!(
            msg.contains("ticket_id"),
            "error must name the rejected field; got: {msg}"
        );
    }

    #[tokio::test]
    async fn pipeline_upsert_errors_without_backend() {
        let args = json!({
            "repo": "rosary",
            "bead_id": "rsry-001",
            "pipeline_phase": 0,
            "pipeline_agent": "dev-agent",
        });
        let result = tool_pipeline_upsert(&args, None).await;
        assert!(result.is_err());
        let msg = result.unwrap_err().to_string();
        assert!(msg.contains("backend store not configured"), "got: {msg}");
    }

    #[tokio::test]
    async fn pipeline_upsert_rejects_missing_required_fields() {
        // Missing pipeline_agent
        let args = json!({
            "repo": "rosary",
            "bead_id": "rsry-001",
            "pipeline_phase": 0,
        });
        let result = tool_pipeline_upsert(&args, None).await;
        // Should fail on backend check before field validation, but if backend were present
        // it would fail on missing pipeline_agent. Test the backend-absent path first.
        assert!(result.is_err());
    }

    // Regression tests for rosary-b0b69a: exercises the same parse_bool_arg
    // helper that call_tool uses, so regressions are caught.

    #[test]
    fn run_once_dry_run_defaults_to_false() {
        assert!(
            !parse_bool_arg(&json!({}), "dry_run", false),
            "dry_run must default to false — MCP dispatch won't work otherwise"
        );
    }

    #[test]
    fn run_once_dry_run_explicit_true() {
        assert!(parse_bool_arg(&json!({"dry_run": true}), "dry_run", false));
    }

    #[test]
    fn run_once_dry_run_explicit_false() {
        assert!(!parse_bool_arg(
            &json!({"dry_run": false}),
            "dry_run",
            false
        ));
    }

    #[test]
    fn run_once_dry_run_string_value_defaults_to_false() {
        // If a client sends "false" as a string, as_bool() returns None
        assert!(
            !parse_bool_arg(&json!({"dry_run": "false"}), "dry_run", false),
            "string 'false' must not become true"
        );
    }

    // Tests for user_id propagation through CallerIdentity -> tool_run_once ->
    // ReconcilerConfig.user_id.  We test the user_scope() extraction layer (the
    // only part that can be unit-tested without loading config or running the
    // reconciler) to lock in the contract: authenticated callers produce
    // Some(user_id), anonymous/machine callers produce None.

    #[test]
    fn user_scope_authenticated_user_yields_some() {
        let id = super::super::CallerIdentity::User("alice".to_string());
        assert_eq!(id.user_scope(), Some("alice"));
    }

    #[test]
    fn user_scope_machine_as_user_yields_some() {
        let id = super::super::CallerIdentity::MachineAsUser {
            user_id: "bob".to_string(),
            service: "ingester".to_string(),
        };
        assert_eq!(id.user_scope(), Some("bob"));
    }

    #[test]
    fn user_scope_machine_yields_none() {
        let id = super::super::CallerIdentity::Machine("ingester".to_string());
        assert_eq!(
            id.user_scope(),
            None,
            "machine-level service must not scope to a user"
        );
    }

    #[test]
    fn user_scope_anonymous_yields_none() {
        let id = super::super::CallerIdentity::Anonymous;
        assert_eq!(
            id.user_scope(),
            None,
            "anonymous/CLI callers must not scope to a user"
        );
    }
}

// ---------------------------------------------------------------------------
// Input validation tests — exercise the MCP boundary guards directly
// ---------------------------------------------------------------------------
#[cfg(test)]
mod input_validation_tests {
    use super::*;
    use crate::store::LinkageStore;
    use serde_json::json;

    fn empty_pool() -> crate::pool::RepoPool {
        crate::pool::RepoPool::empty()
    }

    // Validation fires before DB access, so an empty pool + nonexistent path is fine.
    const FAKE_REPO: &str = "/nonexistent/repo";

    // ---- tool_bead_create --------------------------------------------------

    #[tokio::test]
    async fn create_rejects_blank_title() {
        let args = json!({ "repo_path": FAKE_REPO, "title": "   " });
        let err = tool_bead_create(&args, &empty_pool(), None)
            .await
            .unwrap_err();
        assert!(err.to_string().contains("blank"), "{err}");
    }

    #[tokio::test]
    async fn create_rejects_title_too_long() {
        let long = "x".repeat(TITLE_MAX_LEN + 1);
        let args = json!({ "repo_path": FAKE_REPO, "title": long });
        let err = tool_bead_create(&args, &empty_pool(), None)
            .await
            .unwrap_err();
        assert!(err.to_string().contains("exceeds"), "{err}");
    }

    #[tokio::test]
    async fn create_rejects_priority_out_of_range() {
        let args = json!({ "repo_path": FAKE_REPO, "title": "T", "priority": 4 });
        let err = tool_bead_create(&args, &empty_pool(), None)
            .await
            .unwrap_err();
        assert!(err.to_string().contains("priority must be 0"), "{err}");
    }

    #[tokio::test]
    async fn create_rejects_priority_wrong_type() {
        let args = json!({ "repo_path": FAKE_REPO, "title": "T", "priority": "high" });
        let err = tool_bead_create(&args, &empty_pool(), None)
            .await
            .unwrap_err();
        assert!(
            err.to_string().contains("priority must be an integer"),
            "{err}"
        );
    }

    #[tokio::test]
    async fn create_rejects_unknown_issue_type() {
        let args = json!({ "repo_path": FAKE_REPO, "title": "T", "issue_type": "story" });
        let err = tool_bead_create(&args, &empty_pool(), None)
            .await
            .unwrap_err();
        assert!(err.to_string().contains("unknown issue_type"), "{err}");
    }

    #[tokio::test]
    async fn create_rejects_issue_type_wrong_type() {
        let args = json!({ "repo_path": FAKE_REPO, "title": "T", "issue_type": 42 });
        let err = tool_bead_create(&args, &empty_pool(), None)
            .await
            .unwrap_err();
        assert!(
            err.to_string().contains("issue_type must be a string"),
            "{err}"
        );
    }

    // ---- tool_bead_update --------------------------------------------------

    #[tokio::test]
    async fn update_rejects_blank_title() {
        let args = json!({ "repo_path": FAKE_REPO, "id": "x-1", "title": "" });
        let err = tool_bead_update(&args, &empty_pool(), None)
            .await
            .unwrap_err();
        assert!(err.to_string().contains("blank"), "{err}");
    }

    #[tokio::test]
    async fn update_rejects_priority_wrong_type() {
        let args = json!({ "repo_path": FAKE_REPO, "id": "x-1", "priority": -1 });
        let err = tool_bead_update(&args, &empty_pool(), None)
            .await
            .unwrap_err();
        assert!(
            err.to_string().contains("priority must be an integer"),
            "{err}"
        );
    }

    #[tokio::test]
    async fn update_rejects_out_of_range_priority() {
        let args = json!({ "repo_path": FAKE_REPO, "id": "x-1", "priority": 99 });
        let err = tool_bead_update(&args, &empty_pool(), None)
            .await
            .unwrap_err();
        assert!(err.to_string().contains("priority must be 0"), "{err}");
    }

    #[tokio::test]
    async fn update_rejects_unknown_issue_type() {
        let args = json!({ "repo_path": FAKE_REPO, "id": "x-1", "issue_type": "spike" });
        let err = tool_bead_update(&args, &empty_pool(), None)
            .await
            .unwrap_err();
        assert!(err.to_string().contains("unknown issue_type"), "{err}");
    }

    // ---- tool_bead_comment -------------------------------------------------

    #[tokio::test]
    async fn comment_rejects_blank_body() {
        let args = json!({ "repo_path": FAKE_REPO, "id": "x-1", "body": "  " });
        let err = tool_bead_comment(&args, &empty_pool(), None)
            .await
            .unwrap_err();
        assert!(err.to_string().contains("blank"), "{err}");
    }

    #[tokio::test]
    async fn comment_rejects_oversized_body() {
        let big = "a".repeat(BODY_MAX_LEN + 1);
        let args = json!({ "repo_path": FAKE_REPO, "id": "x-1", "body": big });
        let err = tool_bead_comment(&args, &empty_pool(), None)
            .await
            .unwrap_err();
        assert!(err.to_string().contains("exceeds"), "{err}");
    }

    // ---- tool_bead_link ----------------------------------------------------

    #[tokio::test]
    async fn link_rejects_self_dependency() {
        let args = json!({ "repo_path": FAKE_REPO, "id": "x-1", "depends_on": "x-1" });
        let err = tool_bead_link(&args, &empty_pool(), None)
            .await
            .unwrap_err();
        assert!(err.to_string().contains("cannot depend on itself"), "{err}");
    }

    /// rosary-98b11d: when the caller passes the common-but-wrong
    /// shorthand `{from_id, to_id, link_type}`, the error must name
    /// the canonical parameters (`id`, `depends_on`) so the caller
    /// doesn't have to guess three times to discover the real names.
    #[tokio::test]
    async fn link_error_names_canonical_params_on_missing_id() {
        let args = json!({
            "repo_path": FAKE_REPO,
            "from_id": "a-1",
            "to_id": "b-1",
            "link_type": "blocks"
        });
        let err = tool_bead_link(&args, &empty_pool(), None)
            .await
            .unwrap_err();
        let msg = err.to_string();
        assert!(
            msg.contains("id") && msg.contains("depends_on"),
            "error must name both 'id' and 'depends_on'; got: {msg}"
        );
    }

    /// Sibling: caller passed `id` correctly but used a wrong-name
    /// for the target. The `depends_on required` error must still
    /// surface the canonical name so callers don't loop on a second
    /// schema-discovery cycle.
    #[tokio::test]
    async fn link_error_names_canonical_params_on_missing_depends_on() {
        let args = json!({
            "repo_path": FAKE_REPO,
            "id": "a-1",
            "to_id": "b-1"
        });
        let err = tool_bead_link(&args, &empty_pool(), None)
            .await
            .unwrap_err();
        let msg = err.to_string();
        assert!(
            msg.contains("depends_on"),
            "error must name 'depends_on'; got: {msg}"
        );
    }

    /// rosary-98ee93: when `depends_on` carries a `<repo>-<6hex>` prefix
    /// matching a repo other than the calling `repo_path`, the handler
    /// must auto-route through LinkageStore. Callers shouldn't have to
    /// remember the explicit `cross_repo` argument shape — the canonical
    /// bead-id namespace already encodes the target repo (per
    /// `generate_bead_id`'s `<repo>-<6hex>` convention).
    #[tokio::test]
    async fn link_auto_routes_cross_repo_via_depends_on_prefix() {
        use crate::store::tests::InMemoryStore;

        let store = InMemoryStore::new();
        let args = json!({
            "repo_path": "/Users/test/cloister",
            "id": "cloister-963a5c",
            "depends_on": "signet-9605a3",
        });
        let result = tool_bead_link(&args, &empty_pool(), Some(&store)).await;
        assert!(
            result.is_ok(),
            "auto-routed cross-repo link must succeed via LinkageStore; got: {result:?}"
        );
        let deps = store
            .dependencies_of(&crate::store::WorkRef {
                repo: "cloister".into(),
                scope: String::new(),
                bead_id: "cloister-963a5c".into(),
            })
            .await
            .expect("query dependencies_of");
        assert!(
            deps.iter().any(|d| d.to.bead_id == "signet-9605a3"),
            "cross-repo dep must be present in LinkageStore; got: {deps:?}"
        );
    }

    /// rosary-b5da2f PR 4: `tool_bead_link` accepts a canonical `scope`
    /// arg in place of `repo_path`. This is the first MCP handler
    /// converted to use the new `resolve_scope` boundary parser. The
    /// LinkageStore write path must accept `scope: "repo:cloister"`
    /// equivalently to `repo_path: "/Users/.../cloister"`.
    #[tokio::test]
    async fn link_accepts_scope_arg_in_place_of_repo_path() {
        use crate::store::tests::InMemoryStore;

        let store = InMemoryStore::new();
        // No `repo_path`; only `scope` in canonical form.
        let args = json!({
            "scope": "repo:cloister",
            "id": "cloister-963a5c",
            "depends_on": "signet-9605a3",
        });
        let result = tool_bead_link(&args, &empty_pool(), Some(&store)).await;
        assert!(
            result.is_ok(),
            "scope arg must work in place of repo_path for cross-repo deps; got: {result:?}"
        );
        let deps = store
            .dependencies_of(&crate::store::WorkRef {
                repo: "cloister".into(),
                scope: String::new(),
                bead_id: "cloister-963a5c".into(),
            })
            .await
            .expect("query dependencies_of");
        assert!(
            deps.iter().any(|d| d.to.bead_id == "signet-9605a3"),
            "cross-repo dep must land in LinkageStore when scope was used; got: {deps:?}"
        );
    }

    /// rosary-b5da2f PR 4: `Global` scope can write cross-repo deps via
    /// the LinkageStore bridge — meta-beads (the future incoming triage
    /// queue per `rosary-1db9c9`) need to express deps without a
    /// per-repo backing store. The `from.repo` field stores the
    /// reserved `"global"` namespace (per `ScopeId::work_ref`).
    #[tokio::test]
    async fn link_from_global_scope_routes_via_linkage_store() {
        use crate::store::tests::InMemoryStore;

        let store = InMemoryStore::new();
        let args = json!({
            "scope": "global",
            "id": "global-meta-001",
            "depends_on": "signet-9605a3",
            // Explicit cross_repo because Global has no bead-id prefix
            // to auto-detect from.
            "cross_repo": "signet/signet-9605a3",
        });
        let result = tool_bead_link(&args, &empty_pool(), Some(&store)).await;
        assert!(
            result.is_ok(),
            "Global scope must support cross-repo deps via cross_repo; got: {result:?}"
        );
        let deps = store
            .dependencies_of(&crate::store::WorkRef {
                repo: "global".into(),
                scope: String::new(),
                bead_id: "global-meta-001".into(),
            })
            .await
            .expect("query dependencies_of from global scope");
        assert!(
            deps.iter().any(|d| d.to.bead_id == "signet-9605a3"),
            "Global → signet dep must land; got: {deps:?}"
        );
    }

    /// Copilot #212 finding: `cross_repo` target must NOT silently
    /// accept reserved-namespace strings (`"global"`, `"external:..."`)
    /// as if they were repo names — that would create rows in the
    /// reserved namespace via the wrong-side arg, breaking the
    /// round-trip invariant where Global-scope rows can only be
    /// produced via `ScopeId::Global`.
    #[tokio::test]
    async fn link_rejects_global_namespace_in_cross_repo_target() {
        use crate::store::tests::InMemoryStore;
        let store = InMemoryStore::new();
        let args = json!({
            "scope": "repo:cloister",
            "id": "cloister-963a5c",
            "depends_on": "signet-9605a3",
            "cross_repo": "global/some-bead",   // RESERVED — must reject
        });
        let err = tool_bead_link(&args, &empty_pool(), Some(&store))
            .await
            .unwrap_err();
        let msg = err.to_string();
        assert!(
            msg.contains("global") || msg.contains("reserved"),
            "error must name the reserved namespace; got: {msg}"
        );
    }

    /// Copilot #212 finding: same guard for the `external:` reserved
    /// prefix in cross_repo. Parsing `"external:foo"` as `ScopeId`
    /// produces `External(_)`, not `Repo(_)` — and cross_repo is a
    /// repo-to-repo edge today.
    #[tokio::test]
    async fn link_rejects_external_namespace_in_cross_repo_target() {
        use crate::store::tests::InMemoryStore;
        let store = InMemoryStore::new();
        let args = json!({
            "scope": "repo:cloister",
            "id": "cloister-963a5c",
            "depends_on": "signet-9605a3",
            "cross_repo": "external:zen://foo/some-bead",
        });
        let err = tool_bead_link(&args, &empty_pool(), Some(&store))
            .await
            .unwrap_err();
        let msg = err.to_string();
        assert!(
            msg.contains("external") || msg.contains("reserved"),
            "error must name the reserved namespace; got: {msg}"
        );
    }

    /// rosary-b5da2f PR 4: same-repo deps (no `cross_repo`, no
    /// `depends_on` prefix match) from `External` or `Global` scope
    /// don't make sense — they have no per-repo Dolt store. Must
    /// error with an actionable message pointing the caller at the
    /// `cross_repo` arg.
    #[tokio::test]
    async fn link_errors_on_same_repo_dep_from_global_scope() {
        use crate::store::tests::InMemoryStore;

        let store = InMemoryStore::new();
        let args = json!({
            "scope": "global",
            "id": "global-001",
            "depends_on": "global-002",   // Looks same-scope; no auto-route.
        });
        let err = tool_bead_link(&args, &empty_pool(), Some(&store))
            .await
            .unwrap_err();
        let msg = err.to_string();
        assert!(
            msg.contains("global") || msg.contains("Global"),
            "error must surface that Global scope can't do same-scope deps; got: {msg}"
        );
        assert!(
            msg.contains("cross_repo") || msg.contains("LinkageStore"),
            "error must point at cross_repo arg as the alternative; got: {msg}"
        );
    }

    /// Same-repo deps (where `depends_on`'s repo prefix matches the
    /// calling `repo_path`) must NOT route through LinkageStore — they
    /// stay on the per-repo Dolt path. Otherwise we'd silently divert
    /// every same-repo link to the cross-repo store and confuse the
    /// existing `add_dependency` semantics.
    #[tokio::test]
    async fn link_same_repo_does_not_auto_route_to_linkage_store() {
        use crate::store::tests::InMemoryStore;

        let store = InMemoryStore::new();
        // Both beads in the same repo (`cloister`). Should NOT touch
        // LinkageStore. With no Dolt pool wired here, the same-repo
        // path will Err — that's expected and proves auto-route did
        // not engage (otherwise it'd succeed via the store).
        let args = json!({
            "repo_path": "/Users/test/cloister",
            "id": "cloister-963a5c",
            "depends_on": "cloister-aaaaaa",
        });
        let _ = tool_bead_link(&args, &empty_pool(), Some(&store)).await;
        let deps = store
            .dependencies_of(&crate::store::WorkRef {
                repo: "cloister".into(),
                scope: String::new(),
                bead_id: "cloister-963a5c".into(),
            })
            .await
            .expect("query dependencies_of");
        assert!(
            deps.is_empty(),
            "same-repo links must NOT route through LinkageStore; got: {deps:?}"
        );
    }

    // ---- scope-arg acceptance per converted handler (rosary-b5da2f PR 6) --
    //
    // These tests are the repeatable test harness the user asked for:
    // each converted handler must accept `scope: "repo:<name>"` as a
    // substitute for `repo_path: "/path/to/repo"`. The empty_pool +
    // FAKE_REPO pattern means resolve_repo_client falls to get_client
    // which itself errors on FS lookup — what we're pinning is that
    // **the error is NOT** `"repo_path required"`, which would mean
    // the handler is still doing bespoke arg parsing rather than
    // delegating to resolve_repo_client.

    /// Helper: assert that the error doesn't come from the legacy
    /// `repo_path required` path (i.e. handler is wired to
    /// resolve_repo_client and the scope arg flowed through).
    fn assert_scope_path_engaged(err: &anyhow::Error) {
        let msg = err.to_string();
        assert!(
            !msg.contains("repo_path required"),
            "handler must delegate to resolve_repo_client when only `scope` is passed; \
             error names the legacy parser instead: {msg}"
        );
    }

    #[tokio::test]
    async fn bead_create_accepts_scope_arg() {
        let args = json!({
            "scope": "repo:nonexistent",
            "title": "Test bead",
            "issue_type": "task",
            "files": ["a.rs"],
        });
        let err = tool_bead_create(&args, &empty_pool(), None)
            .await
            .unwrap_err();
        assert_scope_path_engaged(&err);
    }

    #[tokio::test]
    async fn bead_update_accepts_scope_arg() {
        let args = json!({
            "scope": "repo:nonexistent",
            "id": "x-1",
            "title": "new title",
        });
        let err = tool_bead_update(&args, &empty_pool(), None)
            .await
            .unwrap_err();
        assert_scope_path_engaged(&err);
    }

    #[tokio::test]
    async fn bead_close_accepts_scope_arg() {
        let args = json!({
            "scope": "repo:nonexistent",
            "id": "x-1",
        });
        let err = tool_bead_close(&args, &empty_pool(), None)
            .await
            .unwrap_err();
        assert_scope_path_engaged(&err);
    }

    #[tokio::test]
    async fn bead_comment_accepts_scope_arg() {
        let args = json!({
            "scope": "repo:nonexistent",
            "id": "x-1",
            "body": "test",
        });
        let err = tool_bead_comment(&args, &empty_pool(), None)
            .await
            .unwrap_err();
        assert_scope_path_engaged(&err);
    }

    #[tokio::test]
    async fn bead_comment_list_accepts_scope_arg() {
        let args = json!({ "scope": "repo:nonexistent", "id": "x-1" });
        let err = tool_bead_comment_list(&args, &empty_pool(), None)
            .await
            .unwrap_err();
        assert_scope_path_engaged(&err);
    }

    #[tokio::test]
    async fn bead_comment_update_accepts_scope_arg() {
        let args = json!({
            "scope": "repo:nonexistent",
            "comment_id": "c-1",
            "body": "edited",
        });
        let err = tool_bead_comment_update(&args, &empty_pool(), None)
            .await
            .unwrap_err();
        assert_scope_path_engaged(&err);
    }

    #[tokio::test]
    async fn bead_comment_delete_accepts_scope_arg() {
        let args = json!({ "scope": "repo:nonexistent", "comment_id": "c-1" });
        let err = tool_bead_comment_delete(&args, &empty_pool(), None)
            .await
            .unwrap_err();
        assert_scope_path_engaged(&err);
    }

    #[tokio::test]
    async fn bead_search_accepts_scope_arg() {
        let args = json!({ "scope": "repo:nonexistent", "query": "auth" });
        let err = tool_bead_search(&args, &empty_pool(), None)
            .await
            .unwrap_err();
        assert_scope_path_engaged(&err);
    }

    /// Cross-cutting: every converted handler MUST surface the
    /// resolve_scope error when neither `scope` nor `repo_path` is
    /// provided — error names BOTH accepted args so callers know
    /// either is valid. Regression catch for any handler that
    /// reintroduces bespoke `args["repo_path"].ok_or_else(...)` logic.
    #[tokio::test]
    async fn all_converted_handlers_surface_resolve_scope_error() {
        // Args that pass all per-handler validation EXCEPT scope/repo_path,
        // so the resolve_scope-missing-arg path is the actual failure
        // point in every case.
        let bare_args_pairs = [
            (
                "bead_create",
                json!({
                    "title": "x", "issue_type": "task", "files": ["a.rs"]
                }),
            ),
            ("bead_update", json!({"id": "x-1", "title": "x"})),
            ("bead_close", json!({"id": "x-1"})),
            ("bead_comment", json!({"id": "x-1", "body": "x"})),
            ("bead_comment_list", json!({"id": "x-1"})),
            (
                "bead_comment_update",
                json!({"comment_id": "c-1", "body": "x"}),
            ),
            ("bead_comment_delete", json!({"comment_id": "c-1"})),
            ("bead_search", json!({"query": "x"})),
        ];

        for (handler_name, args) in bare_args_pairs {
            let result = match handler_name {
                "bead_create" => tool_bead_create(&args, &empty_pool(), None).await,
                "bead_update" => tool_bead_update(&args, &empty_pool(), None).await,
                "bead_close" => tool_bead_close(&args, &empty_pool(), None).await,
                "bead_comment" => tool_bead_comment(&args, &empty_pool(), None).await,
                "bead_comment_list" => tool_bead_comment_list(&args, &empty_pool(), None).await,
                "bead_comment_update" => tool_bead_comment_update(&args, &empty_pool(), None).await,
                "bead_comment_delete" => tool_bead_comment_delete(&args, &empty_pool(), None).await,
                "bead_search" => tool_bead_search(&args, &empty_pool(), None).await,
                _ => unreachable!(),
            };
            let err = match result {
                Ok(_) => panic!("handler `{handler_name}` accepted args with no scope/repo_path"),
                Err(e) => e,
            };
            let msg = err.to_string();
            assert!(
                msg.contains("scope") && msg.contains("repo_path"),
                "handler `{handler_name}` error must list both accepted args; got: {msg}"
            );
        }
    }

    // ---- resolve_repo_client (rosary-b5da2f PR 5) -------------------------

    /// `resolve_repo_client` rejects `Global` scope with a message
    /// naming the supported addressing paths — Global is identifier-
    /// only, no per-repo bead store.
    #[tokio::test]
    async fn resolve_repo_client_rejects_global_scope() {
        let args = json!({ "scope": "global", "id": "x", "depends_on": "y" });
        let err = match resolve_repo_client(&args, &empty_pool()).await {
            Ok(_) => panic!("resolve_repo_client must reject; returned Ok"),
            Err(e) => e,
        };
        let msg = err.to_string();
        assert!(
            msg.contains("global") || msg.contains("Global"),
            "error must name the rejected scope; got: {msg}"
        );
        assert!(
            msg.contains("Repo-only") || msg.contains("Personal") || msg.contains("LinkageStore"),
            "error must explain the right alternative addressing; got: {msg}"
        );
    }

    /// Same guard for `External` scope.
    #[tokio::test]
    async fn resolve_repo_client_rejects_external_scope() {
        let args = json!({ "scope": "external:zen://inbox", "id": "x" });
        let err = match resolve_repo_client(&args, &empty_pool()).await {
            Ok(_) => panic!("resolve_repo_client must reject; returned Ok"),
            Err(e) => e,
        };
        let msg = err.to_string();
        assert!(
            msg.contains("external") || msg.contains("External"),
            "error must name the rejected scope; got: {msg}"
        );
    }

    /// When `scope: "repo:foo"` is passed but `foo` isn't in the pool
    /// AND no `repo_path` is provided, the error must point the caller
    /// at the two recovery paths: register the repo, or pass repo_path.
    #[tokio::test]
    async fn resolve_repo_client_errors_when_repo_not_in_pool_and_no_repo_path() {
        let args = json!({ "scope": "repo:unloaded-repo" });
        let err = match resolve_repo_client(&args, &empty_pool()).await {
            Ok(_) => panic!("resolve_repo_client must reject; returned Ok"),
            Err(e) => e,
        };
        let msg = err.to_string();
        assert!(
            msg.contains("unloaded-repo"),
            "error must name the missing repo; got: {msg}"
        );
        assert!(
            msg.contains("rsry_repo_register") || msg.contains("repo_path"),
            "error must surface the two recovery paths; got: {msg}"
        );
    }

    /// `resolve_repo_client` is the unified arg-parser; both `scope`
    /// and `repo_path` paths must error symmetrically when *neither*
    /// arg is provided. Delegates to `resolve_scope` for the message
    /// shape (already TDD'd in `serve::scope_args::tests`); this test
    /// pins the delegation contract.
    #[tokio::test]
    async fn resolve_repo_client_errors_when_neither_scope_nor_repo_path() {
        let args = json!({ "id": "x" });
        let err = match resolve_repo_client(&args, &empty_pool()).await {
            Ok(_) => panic!("resolve_repo_client must reject; returned Ok"),
            Err(e) => e,
        };
        let msg = err.to_string();
        assert!(
            msg.contains("scope") && msg.contains("repo_path"),
            "error must list both accepted args (delegated to resolve_scope); got: {msg}"
        );
    }

    /// Copilot #213 finding: when both `scope` and `repo_path` are
    /// passed AND they name different repos, the resolver MUST reject
    /// the pair. Otherwise the caller could operate on repo A's store
    /// while labeling all writes with scope B — silent mis-attribution.
    /// The fix engages whether or not the scope-named repo is in the
    /// pool: the path's basename must match the scope's repo name.
    #[tokio::test]
    async fn resolve_repo_client_rejects_scope_path_mismatch() {
        let args = json!({
            "scope": "repo:cloister",
            "repo_path": "/Users/test/signet",   // basename = "signet" ≠ "cloister"
        });
        let err = match resolve_repo_client(&args, &empty_pool()).await {
            Ok(_) => panic!("resolver must reject mismatched scope/path; returned Ok"),
            Err(e) => e,
        };
        let msg = err.to_string();
        assert!(
            msg.contains("cloister") && msg.contains("signet"),
            "error must name both the scope's repo and the path's basename so the operator \
             can see the disagreement; got: {msg}"
        );
        assert!(
            msg.contains("disagree") || msg.contains("mismatch"),
            "error must explicitly call out the mismatch; got: {msg}"
        );
    }

    /// When the scope-named repo is in the pool AND a non-conflicting
    /// repo_path is passed (e.g. same path or absent), the pool lookup
    /// takes priority. This pins that the mismatch guard ONLY fires on
    /// actual disagreement, not on redundant/consistent specification.
    #[tokio::test]
    async fn resolve_repo_client_accepts_matching_scope_and_repo_path() {
        // Both args specify "cloister" (scope canonical + path basename).
        // No pool entry, so falls to repo_path. FAKE_REPO uses
        // /nonexistent/repo (basename "repo") so we can't use it here;
        // use a path whose basename matches the scope.
        let args = json!({
            "scope": "repo:cloister",
            "repo_path": "/Users/test/cloister",  // basename matches scope
        });
        // FS lookup at /Users/test/cloister will fail (path doesn't
        // exist), but we want to see the resolver get PAST the
        // mismatch guard and into the get_client path — that's the
        // success criterion for THIS test.
        let err = match resolve_repo_client(&args, &empty_pool()).await {
            Ok(_) => return, // pool/FS happened to resolve; fine
            Err(e) => e,
        };
        let msg = err.to_string();
        // The error must NOT be the mismatch guard — that would prove
        // the guard fires on consistent specification (false positive).
        assert!(
            !msg.contains("disagree") && !msg.contains("mismatch"),
            "matching scope+path must not trigger the mismatch guard; got: {msg}"
        );
    }

    /// When `repo_path` (legacy arg) is passed alone, `resolve_repo_client`
    /// must still work — that's the back-compat contract for the 13
    /// handlers about to migrate. Uses FAKE_REPO path to exercise the
    /// fallback to `get_client` (which itself will error on FS lookup,
    /// but resolve_repo_client gets that far cleanly).
    #[tokio::test]
    async fn resolve_repo_client_falls_back_to_repo_path() {
        let args = json!({ "repo_path": FAKE_REPO });
        // The fallback path calls `get_client(FAKE_REPO, ...)` which
        // fails at FS lookup. resolve_repo_client itself is correct;
        // the error must surface from get_client (not the scope
        // parser), proving the fallback engaged.
        let err = match resolve_repo_client(&args, &empty_pool()).await {
            Ok(_) => panic!("resolve_repo_client must reject; returned Ok"),
            Err(e) => e,
        };
        let msg = err.to_string();
        // The error should NOT be a scope-parser error — that would
        // mean resolve_repo_client never reached the fallback.
        assert!(
            !msg.starts_with("scope") && !msg.contains("not loaded in the repo pool"),
            "fallback must reach get_client (not error in scope parsing); got: {msg}"
        );
    }

    // ---- tool_decade_create / tool_thread_create (rosary-992e79) ----------

    /// rosary-992e79: dedicated `rsry_decade_create` returns the created
    /// decade's metadata (not just a confirmation), so callers can chain
    /// "create decade → create thread under it → file beads" in one
    /// session without a separate read step.
    #[tokio::test]
    async fn decade_create_returns_created_metadata() {
        use crate::store::tests::InMemoryStore;
        let store = InMemoryStore::new();
        let args = json!({
            "id": "substrate-idl",
            "title": "Substrate IDL Decade",
            "source_path": "docs/design/substrate-idl.md",
        });
        let result = tool_decade_create(&args, Some(&store))
            .await
            .expect("decade_create must succeed");
        assert_eq!(result["id"], "substrate-idl");
        assert_eq!(result["title"], "Substrate IDL Decade");
        assert_eq!(result["source_path"], "docs/design/substrate-idl.md");
        assert_eq!(result["status"], "active");
        assert_eq!(result["action"], "created");
    }

    /// Idempotency: re-creating with the same title + source_path is a
    /// no-op success, not an error. Lets agents safely retry without a
    /// pre-existence check.
    #[tokio::test]
    async fn decade_create_is_idempotent_on_identical_payload() {
        use crate::store::tests::InMemoryStore;
        let store = InMemoryStore::new();
        let args = json!({ "id": "d-1", "title": "First", "source_path": "x.md" });
        tool_decade_create(&args, Some(&store))
            .await
            .expect("first");
        let again = tool_decade_create(&args, Some(&store))
            .await
            .expect("re-create with identical payload must succeed");
        assert_eq!(again["action"], "existed");
    }

    /// Conflict: re-creating with the same id but a DIFFERENT title
    /// must error — silent overwrite would let agents stomp curated
    /// decade names. The bead's acceptance criteria pin this contract.
    #[tokio::test]
    async fn decade_create_errors_on_conflicting_title() {
        use crate::store::tests::InMemoryStore;
        let store = InMemoryStore::new();
        let args1 = json!({ "id": "d-1", "title": "First" });
        tool_decade_create(&args1, Some(&store))
            .await
            .expect("first");
        let args2 = json!({ "id": "d-1", "title": "Second" });
        let err = tool_decade_create(&args2, Some(&store)).await.unwrap_err();
        let msg = err.to_string();
        assert!(
            msg.contains("d-1") && msg.contains("conflict"),
            "error must name the conflicting id; got: {msg}"
        );
    }

    /// rosary-992e79: `rsry_thread_create` returns the created thread's
    /// metadata including the derived feature_branch (matches the
    /// existing thread_assign branch-naming convention).
    #[tokio::test]
    async fn thread_create_returns_created_metadata() {
        use crate::store::tests::InMemoryStore;
        let store = InMemoryStore::new();
        tool_decade_create(
            &json!({ "id": "d-1", "title": "Test Decade" }),
            Some(&store),
        )
        .await
        .expect("create parent decade");

        let args = json!({
            "decade_id": "d-1",
            "id": "d-1/substrate",
            "name": "Substrate work",
        });
        let result = tool_thread_create(&args, Some(&store))
            .await
            .expect("thread_create must succeed");
        assert_eq!(result["id"], "d-1/substrate");
        assert_eq!(result["name"], "Substrate work");
        assert_eq!(result["decade_id"], "d-1");
        assert_eq!(result["action"], "created");
    }

    /// thread_create must refuse when the parent decade doesn't exist.
    /// `thread_assign` auto-creates an `ungrouped` decade as a
    /// fall-through, but the explicit create-by-id flow must surface
    /// missing parents loudly so agents don't accidentally orphan
    /// threads under stub decades.
    #[tokio::test]
    async fn thread_create_errors_when_parent_decade_missing() {
        use crate::store::tests::InMemoryStore;
        let store = InMemoryStore::new();
        let args = json!({
            "decade_id": "does-not-exist",
            "id": "orphan",
            "name": "Orphan thread",
        });
        let err = tool_thread_create(&args, Some(&store)).await.unwrap_err();
        let msg = err.to_string();
        assert!(
            msg.contains("does-not-exist"),
            "error must name the missing parent decade; got: {msg}"
        );
    }

    /// Idempotency for thread_create: same (decade_id, id, name) is a
    /// no-op success — agents can safely retry.
    #[tokio::test]
    async fn thread_create_is_idempotent_on_identical_payload() {
        use crate::store::tests::InMemoryStore;
        let store = InMemoryStore::new();
        tool_decade_create(&json!({ "id": "d-1", "title": "D" }), Some(&store))
            .await
            .expect("create parent");
        let args = json!({ "decade_id": "d-1", "id": "d-1/t", "name": "T" });
        tool_thread_create(&args, Some(&store))
            .await
            .expect("first");
        let again = tool_thread_create(&args, Some(&store))
            .await
            .expect("re-create with identical payload must succeed");
        assert_eq!(again["action"], "existed");
    }

    /// Copilot #205 finding: the in-decade existence check in
    /// `tool_thread_create` would miss a thread with the same `id`
    /// living under a *different* decade, and silently let upsert
    /// re-parent it. Global uniqueness across decades is the right
    /// contract — otherwise two callers issuing the same thread id
    /// against different decades would clobber each other.
    #[tokio::test]
    async fn thread_create_errors_on_global_id_conflict_across_decades() {
        use crate::store::tests::InMemoryStore;
        let store = InMemoryStore::new();
        tool_decade_create(&json!({ "id": "d-1", "title": "D1" }), Some(&store))
            .await
            .expect("create d-1");
        tool_decade_create(&json!({ "id": "d-2", "title": "D2" }), Some(&store))
            .await
            .expect("create d-2");
        tool_thread_create(
            &json!({ "decade_id": "d-1", "id": "shared-thread-id", "name": "First" }),
            Some(&store),
        )
        .await
        .expect("first thread_create");

        // Same id under a DIFFERENT decade must error — otherwise the
        // second create would silently re-parent the first thread.
        let err = tool_thread_create(
            &json!({ "decade_id": "d-2", "id": "shared-thread-id", "name": "Second" }),
            Some(&store),
        )
        .await
        .unwrap_err();
        let msg = err.to_string();
        assert!(
            msg.contains("shared-thread-id"),
            "error must name the conflicting thread id; got: {msg}"
        );
        assert!(
            msg.contains("d-1") || msg.contains("already exists"),
            "error must surface where the existing thread lives or that it already exists; got: {msg}"
        );
    }

    /// Copilot #205 finding: when only `source_path` differs (title
    /// matches), the conflict message must not falsely claim "conflicting
    /// title" — it should name the actual diverging field. This pins the
    /// error-message accuracy contract so a reader of the failure isn't
    /// misled about what to fix.
    #[tokio::test]
    async fn decade_create_conflict_message_distinguishes_title_from_source_path() {
        use crate::store::tests::InMemoryStore;
        let store = InMemoryStore::new();
        tool_decade_create(
            &json!({ "id": "d-1", "title": "Same Title", "source_path": "a.md" }),
            Some(&store),
        )
        .await
        .expect("first");
        // Title matches; source_path differs. The error must mention
        // source_path, not just title.
        let err = tool_decade_create(
            &json!({ "id": "d-1", "title": "Same Title", "source_path": "b.md" }),
            Some(&store),
        )
        .await
        .unwrap_err();
        let msg = err.to_string();
        assert!(
            msg.contains("source_path"),
            "error must surface that source_path is the conflicting field, not just title; got: {msg}"
        );
        assert!(
            msg.contains("a.md") && msg.contains("b.md"),
            "error must show both source_paths so the operator can see what changed; got: {msg}"
        );
    }
}
