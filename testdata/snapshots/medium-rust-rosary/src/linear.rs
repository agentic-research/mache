use anyhow::{Context, Result};
use serde_json::{Value, json};
use std::collections::HashMap;

use crate::bead::BeadState;

const LINEAR_API_URL: &str = "https://api.linear.app/graphql";

/// Build a reqwest client with the Linear API key in the Authorization header.
fn build_client(api_key: &str) -> Result<reqwest::Client> {
    use reqwest::header::{AUTHORIZATION, CONTENT_TYPE, HeaderMap, HeaderValue};

    let mut headers = HeaderMap::new();
    headers.insert(AUTHORIZATION, HeaderValue::from_str(api_key)?);
    headers.insert(CONTENT_TYPE, HeaderValue::from_static("application/json"));

    Ok(reqwest::Client::builder()
        .default_headers(headers)
        .build()?)
}

/// Execute a GraphQL query against the Linear API and return the JSON response.
async fn graphql(client: &reqwest::Client, query: &str, variables: Value) -> Result<Value> {
    let body = json!({
        "query": query,
        "variables": variables,
    });

    let resp = client
        .post(LINEAR_API_URL)
        .json(&body)
        .send()
        .await
        .context("failed to reach Linear API")?;

    let status = resp.status();
    let text = resp
        .text()
        .await
        .context("failed to read Linear response body")?;

    if !status.is_success() {
        anyhow::bail!("Linear API returned {status}: {text}");
    }

    let json: Value = serde_json::from_str(&text).context("Linear response is not valid JSON")?;

    if let Some(errors) = json.get("errors") {
        anyhow::bail!("Linear GraphQL errors: {errors}");
    }

    Ok(json)
}

/// Read LINEAR_API_KEY from the environment. Returns None (with a helpful message) if unset.
fn get_api_key() -> Option<String> {
    // 1. Env var (highest priority)
    if let Ok(key) = std::env::var("LINEAR_API_KEY")
        && !key.is_empty()
    {
        return Some(key);
    }
    // 2. Config file
    if let Ok(cfg) = crate::config::load_merged("rosary.toml")
        && let Some(linear) = &cfg.linear
        && let Some(ref key) = linear.api_key
        && !key.is_empty()
    {
        return Some(key.clone());
    }
    eprintln!(
        "LINEAR_API_KEY not set. Add to ~/.rsry/config.toml under [linear] or export LINEAR_API_KEY=lin_api_..."
    );
    None
}

/// Parse a Linear issue identifier from either a bare ID ("ART-123") or a URL.
///
/// Supported URL formats:
/// - `https://linear.app/team/issue/ART-123/slug`
/// - `https://linear.app/team/issue/ART-123`
fn parse_issue_identifier(ticket: &str) -> String {
    // If it looks like a URL, extract the identifier segment
    if ticket.starts_with("http://") || ticket.starts_with("https://") {
        // URL pattern: .../issue/ART-123/...
        if let Some(idx) = ticket.find("/issue/") {
            let after = &ticket[idx + 7..]; // skip "/issue/"
            // Take up to the next '/' or end of string
            return after.split('/').next().unwrap_or(after).to_string();
        }
    }
    ticket.to_string()
}

/// Format a priority number into a human-readable label.
fn priority_label(p: i64) -> &'static str {
    match p {
        0 => "No priority",
        1 => "Urgent",
        2 => "High",
        3 => "Medium",
        4 => "Low",
        _ => "Unknown",
    }
}

/// Decompose a Linear ticket into repo-scoped beads (top-down planning).
///
/// Currently fetches and displays the issue. Future: analyze description for repo
/// references and create beads in each referenced repo (rosary writes directly
/// to per-repo Dolt — no `bd` CLI involved).
pub async fn plan(ticket: &str) -> Result<()> {
    let api_key = match get_api_key() {
        Some(k) => k,
        None => return Ok(()),
    };

    let client = build_client(&api_key)?;
    let issue = get_ticket(&client, ticket).await?;

    let title = issue["title"].as_str().unwrap_or("(untitled)");
    let ident = issue["identifier"].as_str().unwrap_or("???");
    let state = issue
        .pointer("/state/name")
        .and_then(|v| v.as_str())
        .unwrap_or("unknown");
    let priority = issue["priority"].as_i64().unwrap_or(0);
    let description = issue["description"].as_str().unwrap_or("(no description)");

    let labels: Vec<&str> = issue
        .pointer("/labels/nodes")
        .and_then(|v| v.as_array())
        .map(|arr| arr.iter().filter_map(|l| l["name"].as_str()).collect())
        .unwrap_or_default();

    println!("--- Linear Issue ---");
    println!("Identifier: {ident}");
    println!("Title:      {title}");
    println!("State:      {state}");
    println!("Priority:   {} ({})", priority, priority_label(priority));
    if !labels.is_empty() {
        println!("Labels:     {}", labels.join(", "));
    }
    println!();
    println!("Description:");
    println!("{description}");
    println!("--------------------");

    // Future: parse description for repo references, decompose into beads
    println!();
    println!("(bead decomposition not yet implemented — coming soon)");

    Ok(())
}

/// Build a Linear-authed reqwest client from env / config, if a key is set.
/// Returns None if the key is absent — callers convert this to a friendly
/// "Linear not configured" error message instead of crashing.
pub(crate) fn try_client() -> Option<reqwest::Client> {
    let api_key = get_api_key()?;
    build_client(&api_key).ok()
}

/// Fetch a single Linear issue by identifier (`ART-123` form or full URL).
/// Returns the issue node JSON. Shared by `plan()` and the `rsry_ticket_load`
/// MCP tool (rosary-5dc9b0).
pub(crate) async fn get_ticket(client: &reqwest::Client, ticket: &str) -> Result<Value> {
    let identifier = parse_issue_identifier(ticket);

    // Issue node includes the internal `id` so callers can chain a comments
    // lookup (`get_ticket_comments`) without a second identifier round-trip.
    let query = r#"
        query IssueByIdentifier($filter: IssueFilter!) {
            issues(filter: $filter, first: 1) {
                nodes {
                    id
                    identifier
                    title
                    description
                    priority
                    state { name }
                    assignee { name email }
                    labels { nodes { name } }
                }
            }
        }
    "#;

    // Split identifier (e.g., "ART-123") into team key and number for the
    // strict filter; otherwise fall back to identifier-eq lookup.
    let parts: Vec<&str> = identifier.splitn(2, '-').collect();
    let filter = if parts.len() == 2 {
        if let Ok(num) = parts[1].parse::<i64>() {
            json!({
                "team": { "key": { "eq": parts[0] } },
                "number": { "eq": num }
            })
        } else {
            json!({ "identifier": { "eq": identifier } })
        }
    } else {
        json!({ "identifier": { "eq": identifier } })
    };

    let resp = graphql(client, query, json!({ "filter": filter })).await?;

    let issue = resp
        .pointer("/data/issues/nodes/0")
        .context("issue not found — check that the identifier is correct")?
        .clone();
    Ok(issue)
}

/// Fetch all comments on a Linear issue by its internal id (from `get_ticket`).
/// Returns an empty list on success-with-no-comments; an error on auth /
/// transport failures. Up to 50 comments — Phase 1 paginates if needed.
pub(crate) async fn get_ticket_comments(
    client: &reqwest::Client,
    issue_id: &str,
) -> Result<Vec<Value>> {
    let query = r#"
        query IssueComments($id: String!) {
            issue(id: $id) {
                comments(first: 50) {
                    nodes {
                        id
                        body
                        createdAt
                        user { name email }
                    }
                }
            }
        }
    "#;

    let resp = graphql(client, query, json!({ "id": issue_id })).await?;

    let nodes = resp
        .pointer("/data/issue/comments/nodes")
        .and_then(|v| v.as_array())
        .cloned()
        .unwrap_or_default();
    Ok(nodes)
}

/// Look up a team's internal ID by its key (e.g., "ART").
async fn resolve_team_id(client: &reqwest::Client, team_key: &str) -> Result<String> {
    let query = r#"
        query Teams {
            teams {
                nodes {
                    id
                    key
                    name
                }
            }
        }
    "#;

    let resp = graphql(client, query, json!({})).await?;

    let teams = resp
        .pointer("/data/teams/nodes")
        .and_then(|v| v.as_array())
        .context("could not fetch teams from Linear")?;

    for team in teams {
        if team["key"].as_str() == Some(team_key)
            && let Some(id) = team["id"].as_str()
        {
            return Ok(id.to_string());
        }
    }

    anyhow::bail!(
        "team '{team_key}' not found. Available teams: {}",
        teams
            .iter()
            .filter_map(|t| t["key"].as_str())
            .collect::<Vec<_>>()
            .join(", ")
    )
}

/// Extract phase identifier from bead text (title or description).
///
/// Recognizes patterns like:
/// - `phase:1`, `phase:2` (label-style, case-insensitive)
/// - `Phase 1`, `Phase 2` (prose-style, case-insensitive)
///
/// The phase value must start with a digit (e.g., "1", "2", "3a").
/// Label-style (`phase:N`) takes priority over prose-style (`Phase N`).
///
/// Returns the phase key (e.g., "1", "2") or None if no phase found.
fn extract_phase(text: &str) -> Option<String> {
    let lower = text.to_lowercase();

    // Try "phase:N" pattern (label-style) — highest priority
    if let Some(idx) = lower.find("phase:") {
        let after = &lower[idx + 6..];
        if after.starts_with(|c: char| c.is_ascii_digit()) {
            let phase_key: String = after.chars().take_while(|c| c.is_alphanumeric()).collect();
            return Some(phase_key);
        }
    }

    // Try "phase N" pattern (prose-style)
    if let Some(idx) = lower.find("phase ") {
        let after = &lower[idx + 6..];
        if after.starts_with(|c: char| c.is_ascii_digit()) {
            let phase_key: String = after.chars().take_while(|c| c.is_alphanumeric()).collect();
            return Some(phase_key);
        }
    }

    None
}

/// Look up a Linear project ID by name.
///
/// Uses exact-match filter to find the project. Returns None if not found.
async fn resolve_project_id(
    client: &reqwest::Client,
    project_name: &str,
) -> Result<Option<String>> {
    let query = r#"
        query FindProject($filter: ProjectFilter!) {
            projects(filter: $filter) {
                nodes {
                    id
                    name
                }
            }
        }
    "#;

    let variables = json!({
        "filter": {
            "name": { "eq": project_name }
        }
    });

    let resp = graphql(client, query, variables).await?;

    let project_id = resp
        .pointer("/data/projects/nodes/0/id")
        .and_then(|v| v.as_str())
        .map(|s| s.to_string());

    Ok(project_id)
}

/// Resolve phase config to a Linear project ID for a given bead.
///
/// Checks bead title and description for phase patterns, then looks up
/// the corresponding project in the phase mapping. Caches resolved project
/// IDs to avoid redundant API calls.
async fn resolve_phase_project_id(
    client: &reqwest::Client,
    bead: &crate::bead::Bead,
    phase_map: &HashMap<String, String>,
    project_cache: &mut HashMap<String, Option<String>>,
) -> Result<Option<String>> {
    if phase_map.is_empty() {
        return Ok(None);
    }

    // Check title first, then description
    let phase_key = extract_phase(&bead.title).or_else(|| extract_phase(&bead.description));

    let Some(key) = phase_key else {
        return Ok(None);
    };

    let Some(project_name) = phase_map.get(&key) else {
        return Ok(None);
    };

    // Check cache first
    if let Some(cached) = project_cache.get(project_name) {
        return Ok(cached.clone());
    }

    // Resolve via API
    let project_id = resolve_project_id(client, project_name).await?;
    if project_id.is_none() {
        eprintln!("  warning: Linear project '{project_name}' (phase {key}) not found");
    }
    project_cache.insert(project_name.clone(), project_id.clone());
    Ok(project_id)
}

/// Connect to a repo's bead store via its .beads/ directory.
async fn connect_repo_beads(
    repo: &crate::config::RepoConfig,
) -> Result<Box<dyn crate::store::BeadStore>> {
    let path = crate::scanner::expand_path(&repo.path);
    let beads_dir = path.join(".beads");
    crate::bead_sqlite::connect_bead_store(&beads_dir).await
}

/// Bidirectional sync: beads <-> Linear.
///
/// 1. Link: match existing Linear issues to beads by title, store external_ref
/// 2. Push: create Linear issues for unlinked beads, store external_ref
/// 3. Close: update Linear issues for closed beads
///
/// If `repo_filter` is provided, only beads from those repos are synced.
pub async fn sync(
    dry_run: bool,
    repo_filter: Option<&[String]>,
    hierarchy: Option<&dyn crate::store::HierarchyStore>,
) -> Result<()> {
    let api_key = match get_api_key() {
        Some(k) => k,
        None => return Ok(()),
    };

    // Read team key: env var > config > default
    let team_key = std::env::var("LINEAR_TEAM").unwrap_or_else(|_| {
        crate::config::load_merged("rosary.toml")
            .ok()
            .and_then(|cfg| cfg.linear.map(|l| l.team))
            .unwrap_or_else(|| "ART".to_string())
    });

    let client = build_client(&api_key)?;
    let team_id = resolve_team_id(&client, &team_key).await?;

    let query = r#"
        query TeamIssues($teamId: String!, $filter: IssueFilter) {
            team(id: $teamId) {
                name
                key
                issues(first: 250, filter: $filter) {
                    nodes {
                        identifier
                        title
                        description
                        priority
                        state { name }
                        url
                    }
                }
            }
        }
    "#;

    let variables = json!({
        "teamId": team_id,
        "filter": {
            "state": { "type": { "in": ["started", "unstarted", "backlog"] } }
        }
    });

    let resp = graphql(&client, query, variables).await?;

    let team_name = resp
        .pointer("/data/team/name")
        .and_then(|v| v.as_str())
        .unwrap_or(&team_key);

    let issues = resp
        .pointer("/data/team/issues/nodes")
        .and_then(|v| v.as_array())
        .context("could not fetch issues from Linear")?;

    crate::cli::sync_header(team_name);

    // --- Build per-repo Dolt client map ---
    let cfg = crate::config::load_merged("rosary.toml")?;
    let phase_map = cfg
        .linear
        .as_ref()
        .map(|l| l.phases.clone())
        .unwrap_or_default();
    let mut project_cache: HashMap<String, Option<String>> = HashMap::new();

    // Filter repos if --repo flag was provided
    let repos: Vec<&crate::config::RepoConfig> = match repo_filter {
        Some(names) => cfg
            .repo
            .iter()
            .filter(|r| names.contains(&r.name))
            .collect(),
        None => cfg.repo.iter().collect(),
    };

    let beads =
        crate::scanner::scan_repos(&repos.iter().map(|r| (*r).clone()).collect::<Vec<_>>()).await?;

    let mut dolt_clients: std::collections::HashMap<String, Box<dyn crate::store::BeadStore>> =
        std::collections::HashMap::new();
    for repo in &repos {
        match connect_repo_beads(repo).await {
            Ok(dc) => {
                dolt_clients.insert(repo.name.clone(), dc);
            }
            Err(e) => {
                crate::cli::sync_error(&repo.name, &format!("Dolt connect: {e}"));
            }
        }
    }

    // Build Linear issue lookup: identifier → (title, description, url, state)
    let linear_issues: Vec<(&str, &str, &str, &str, &str)> = issues
        .iter()
        .filter_map(|i| {
            let ident = i["identifier"].as_str()?;
            let title = i["title"].as_str()?;
            let desc = i["description"].as_str().unwrap_or("");
            let url = i["url"].as_str().unwrap_or("");
            let state = i
                .pointer("/state/name")
                .and_then(|v| v.as_str())
                .unwrap_or("Todo");
            Some((ident, title, desc, url, state))
        })
        .collect();

    let dry_prefix = crate::cli::sync_dry_run_prefix();

    // --- LINK: match existing Linear issues to unlinked beads ---
    // Match by: (1) bead tag in description, (2) title match
    let mut linked = 0;
    let mut linked_ids: std::collections::HashSet<String> = std::collections::HashSet::new();
    for bead in &beads {
        if bead.external_ref.is_some() {
            continue;
        }
        let bead_tag = format!("<!-- bead:{} ", bead.id);
        let prefixed_title = format!("[{}] {}", bead.repo, bead.title);

        let matched = linear_issues.iter().find(|(_, title, desc, _, _)| {
            desc.contains(&bead_tag) || *title == prefixed_title || *title == bead.title
        });

        if let Some((ident, _, _, _url, _)) = matched {
            if dry_run {
                println!("  {dry_prefix} link {} -> {ident}", bead.id);
                linked += 1;
                linked_ids.insert(bead.id.clone());
            } else if let Some(dc) = dolt_clients.get(&bead.repo) {
                if let Err(e) = dc.set_external_ref(&bead.id, ident).await {
                    crate::cli::sync_error(&bead.id, &format!("link: {e}"));
                } else {
                    crate::cli::sync_linked(&bead.id, ident);
                    linked += 1;
                    linked_ids.insert(bead.id.clone());
                }
            }
        }
    }

    // --- PUSH: create Linear issues for unlinked beads, store external_ref ---
    // Track thread → Linear parent issue ID for sub-issue creation.
    // Maps thread_id → Linear issue UUID. First bead synced in a thread
    // stores its UUID here; siblings use it as parentId (sub-issues).
    let mut thread_parent_ids: HashMap<String, String> = HashMap::new();
    let mut created = 0;
    for bead in &beads {
        if bead.external_ref.is_some() || linked_ids.contains(&bead.id) {
            continue;
        }
        if bead.status == "closed" {
            continue;
        }
        if bead.priority > 2 {
            continue;
        }
        let prefixed_title = format!("[{}] {}", bead.repo, bead.title);
        if linear_issues
            .iter()
            .any(|(_, t, _, _, _)| *t == prefixed_title || *t == bead.title)
        {
            continue;
        }

        let label = format!("[{}] ", bead.repo);
        let full_title = format!("{label}{}", bead.title);

        let mut perspective_labels: Vec<String> = Vec::new();
        if let Some(ref owner) = bead.owner
            && let Some(perspective) = owner.strip_suffix("-agent")
        {
            perspective_labels.push(format!("perspective:{perspective}"));
        }

        let project_id =
            resolve_phase_project_id(&client, bead, &phase_map, &mut project_cache).await?;

        // Resolve thread → Linear parent issue UUID for sub-issue creation.
        // thread_parent_ids maps thread_id → Linear issue UUID.
        let parent_linear_uuid: Option<String> = if let Some(hier) = hierarchy {
            let bead_ref = crate::store::WorkRef {
                repo: bead.repo.clone(),
                scope: String::new(),
                bead_id: bead.id.clone(),
            };
            if let Ok(Some(thread_id)) = hier.find_thread_for_bead(&bead_ref).await {
                thread_parent_ids.get(&thread_id).cloned()
                // If no parent exists yet, first bead in the thread creates
                // the parent issue below and stores its UUID for siblings.
            } else {
                None
            }
        } else {
            None
        };

        if dry_run {
            let sub = if parent_linear_uuid.is_some() {
                " (sub-issue)"
            } else {
                ""
            };
            println!("  {dry_prefix} create: {full_title}{sub}");
            created += 1;
        } else {
            match create_linear_issue(
                &client,
                &team_id,
                &full_title,
                &bead.description,
                bead.priority,
                &bead.id,
                &bead.repo,
                &perspective_labels,
                project_id.as_deref(),
                parent_linear_uuid.as_deref(),
            )
            .await
            {
                Ok((ident, uuid)) => {
                    crate::cli::sync_created(&ident, &full_title);
                    created += 1;
                    if let Some(dc) = dolt_clients.get(&bead.repo)
                        && let Err(e) = dc.set_external_ref(&bead.id, &ident).await
                    {
                        crate::cli::sync_error(&bead.id, &format!("store ref: {e}"));
                    }
                    // Store UUID so siblings in the same thread get this as parentId
                    if let Some(hier) = hierarchy {
                        let bead_ref = crate::store::WorkRef {
                            repo: bead.repo.clone(),
                            scope: String::new(),
                            bead_id: bead.id.clone(),
                        };
                        if let Ok(Some(thread_id)) = hier.find_thread_for_bead(&bead_ref).await
                            && !uuid.is_empty()
                        {
                            thread_parent_ids.entry(thread_id).or_insert(uuid);
                        }
                    }
                }
                Err(e) => {
                    crate::cli::sync_error(&bead.id, &e.to_string());
                }
            }
        }
    }

    // --- CLOSE: update Linear issues for closed beads ---
    let mut closed = 0;
    for repo in &repos {
        let Some(dc) = dolt_clients.get(&repo.name) else {
            continue;
        };
        let closed_beads = match dc.list_closed_linked_beads(&repo.name).await {
            Ok(b) => b,
            Err(e) => {
                crate::cli::sync_error(&repo.name, &format!("query closed: {e}"));
                continue;
            }
        };
        for bead in &closed_beads {
            let ext_ref = bead.external_ref.as_deref().unwrap_or_default();
            if linear_issues
                .iter()
                .any(|(ident, _, _, _, _)| *ident == ext_ref)
            {
                if dry_run {
                    println!("  {dry_prefix} close {ext_ref} ({})", bead.id);
                    closed += 1;
                } else {
                    match update_linear_issue_status(&client, &team_id, ext_ref, "closed").await {
                        Ok(()) => {
                            crate::cli::sync_closed(ext_ref, &bead.id, &bead.title);
                            closed += 1;
                        }
                        Err(e) => {
                            crate::cli::sync_error(ext_ref, &e.to_string());
                        }
                    }
                }
            }
        }
    }

    crate::cli::sync_summary(linked, created, closed);

    Ok(())
}

/// Find an existing label by name in a team, or create one.
/// Returns the Linear label ID.
async fn find_or_create_label(
    client: &reqwest::Client,
    team_id: &str,
    name: &str,
) -> Result<String> {
    // Query existing labels for the team
    let query_str = r#"
        query TeamLabels($teamId: String!) {
            team(id: $teamId) {
                labels { nodes { id name } }
            }
        }
    "#;
    let resp = graphql(client, query_str, json!({ "teamId": team_id })).await?;
    let nodes = resp
        .pointer("/data/team/labels/nodes")
        .and_then(|v| v.as_array());

    if let Some(nodes) = nodes {
        for node in nodes {
            if node["name"].as_str() == Some(name)
                && let Some(id) = node["id"].as_str()
            {
                return Ok(id.to_string());
            }
        }
    }

    // Label doesn't exist — create it
    let mutation = r#"
        mutation CreateLabel($input: IssueLabelCreateInput!) {
            issueLabelCreate(input: $input) {
                success
                issueLabel { id }
            }
        }
    "#;
    let variables = json!({
        "input": {
            "name": name,
            "teamId": team_id,
        }
    });
    let resp = graphql(client, mutation, variables).await?;
    let success = resp
        .pointer("/data/issueLabelCreate/success")
        .and_then(|v| v.as_bool())
        .unwrap_or(false);
    if !success {
        anyhow::bail!("issueLabelCreate failed for '{name}'");
    }
    let label_id = resp
        .pointer("/data/issueLabelCreate/issueLabel/id")
        .and_then(|v| v.as_str())
        .context("missing label ID in issueLabelCreate response")?;
    Ok(label_id.to_string())
}

/// Create a new issue in Linear with bead ID tagged in description.
/// Returns (identifier, uuid) — e.g., ("AGE-5", "abc-def-123").
/// The UUID is needed for parentId when creating sub-issues.
#[allow(clippy::too_many_arguments)]
async fn create_linear_issue(
    client: &reqwest::Client,
    team_id: &str,
    title: &str,
    description: &str,
    priority: u8,
    bead_id: &str,
    repo_name: &str,
    perspective_labels: &[String],
    project_id: Option<&str>,
    parent_id: Option<&str>,
) -> Result<(String, String)> {
    // Tag the description with bead ID for bidirectional linkage
    let tagged_description = format!("{description}\n\n<!-- bead:{bead_id} repo:{repo_name} -->",);
    let mutation = r#"
        mutation CreateIssue($input: IssueCreateInput!) {
            issueCreate(input: $input) {
                success
                issue {
                    id
                    identifier
                    title
                }
            }
        }
    "#;

    // Map bead priority (0=P0 highest) to Linear priority (1=urgent, 4=low)
    let linear_priority = match priority {
        0 => 1, // P0 → Urgent
        1 => 2, // P1 → High
        2 => 3, // P2 → Medium
        _ => 4, // P3+ → Low
    };

    // Resolve perspective labels to Linear label IDs
    let mut label_ids: Vec<String> = Vec::new();
    for label_name in perspective_labels {
        match find_or_create_label(client, team_id, label_name).await {
            Ok(id) => label_ids.push(id),
            Err(e) => {
                eprintln!("  warning: could not resolve label '{label_name}': {e}");
            }
        }
    }

    let mut input = json!({
        "teamId": team_id,
        "title": title,
        "description": tagged_description,
        "priority": linear_priority,
    });

    if !label_ids.is_empty() {
        input["labelIds"] = json!(label_ids);
    }

    // Attach to Linear project if phase mapping resolved
    if let Some(pid) = project_id {
        input["projectId"] = json!(pid);
    }

    // Create as sub-issue if parent specified (thread → parent issue)
    if let Some(pid) = parent_id {
        input["parentId"] = json!(pid);
    }

    let variables = json!({ "input": input });

    let resp = graphql(client, mutation, variables).await?;

    let success = resp
        .pointer("/data/issueCreate/success")
        .and_then(|v| v.as_bool())
        .unwrap_or(false);

    if !success {
        anyhow::bail!("Linear issueCreate returned success=false");
    }

    let identifier = resp
        .pointer("/data/issueCreate/issue/identifier")
        .and_then(|v| v.as_str())
        .unwrap_or("???")
        .to_string();

    let uuid = resp
        .pointer("/data/issueCreate/issue/id")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .to_string();

    Ok((identifier, uuid))
}

/// Update a Linear issue's workflow state.
/// Maps rosary status ("closed", "in_progress", "open") to Linear state types.
async fn update_linear_issue_status(
    client: &reqwest::Client,
    team_id: &str,
    external_id: &str,
    status: &str,
) -> Result<()> {
    // Find the issue's internal ID by identifier
    let find_query = r#"
        query FindIssue($filter: IssueFilter!) {
            issues(filter: $filter, first: 1) {
                nodes { id }
            }
        }
    "#;

    let parts: Vec<&str> = external_id.splitn(2, '-').collect();
    let filter = if parts.len() == 2 {
        if let Ok(num) = parts[1].parse::<i64>() {
            json!({
                "team": { "key": { "eq": parts[0] } },
                "number": { "eq": num }
            })
        } else {
            json!({ "identifier": { "eq": external_id } })
        }
    } else {
        json!({ "identifier": { "eq": external_id } })
    };

    let resp = graphql(client, find_query, json!({ "filter": filter })).await?;
    let issue_id = resp
        .pointer("/data/issues/nodes/0/id")
        .and_then(|v| v.as_str())
        .context("issue not found in Linear")?;

    // Find the target workflow state
    let states_query = r#"
        query TeamStates($teamId: String!) {
            team(id: $teamId) {
                states { nodes { id name type } }
            }
        }
    "#;
    let states_resp = graphql(client, states_query, json!({ "teamId": team_id })).await?;
    let states = states_resp
        .pointer("/data/team/states/nodes")
        .and_then(|v| v.as_array())
        .context("fetching workflow states")?;

    // Map rosary status to Linear state via type (stable) + name hint (refinement)
    let bead_state = BeadState::from(status);
    let (target_type, preferred_name) = bead_state.to_linear_type();

    // Try preferred name within type first, fall back to any state with matching type
    let target_state = states
        .iter()
        .find(|s| {
            s["type"].as_str() == Some(target_type) && s["name"].as_str() == Some(preferred_name)
        })
        .or_else(|| {
            states
                .iter()
                .find(|s| s["type"].as_str() == Some(target_type))
        })
        .and_then(|s| s["id"].as_str())
        .context(format!(
            "no Linear state with type '{target_type}' for bead status '{status}'"
        ))?;

    // Update the issue
    let mutation = r#"
        mutation UpdateIssue($id: String!, $input: IssueUpdateInput!) {
            issueUpdate(id: $id, input: $input) { success }
        }
    "#;
    let resp = graphql(
        client,
        mutation,
        json!({ "id": issue_id, "input": { "stateId": target_state } }),
    )
    .await?;

    let success = resp
        .pointer("/data/issueUpdate/success")
        .and_then(|v| v.as_bool())
        .unwrap_or(false);
    if !success {
        anyhow::bail!("issueUpdate failed for {external_id}");
    }

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_bare_identifier() {
        assert_eq!(parse_issue_identifier("ART-123"), "ART-123");
    }

    #[test]
    fn parse_url_with_slug() {
        let url = "https://linear.app/art/issue/ART-123/some-slug-here";
        assert_eq!(parse_issue_identifier(url), "ART-123");
    }

    #[test]
    fn parse_url_without_slug() {
        let url = "https://linear.app/art/issue/ART-456";
        assert_eq!(parse_issue_identifier(url), "ART-456");
    }

    #[test]
    fn parse_non_url_passthrough() {
        assert_eq!(parse_issue_identifier("FOO-1"), "FOO-1");
    }

    #[test]
    fn priority_labels() {
        assert_eq!(priority_label(0), "No priority");
        assert_eq!(priority_label(1), "Urgent");
        assert_eq!(priority_label(2), "High");
        assert_eq!(priority_label(3), "Medium");
        assert_eq!(priority_label(4), "Low");
        assert_eq!(priority_label(99), "Unknown");
    }

    #[test]
    fn bead_to_linear_priority_mapping() {
        // Bead P0 (critical) → Linear 1 (Urgent)
        // Bead P1 (high) → Linear 2 (High)
        // Bead P2 (medium) → Linear 3 (Medium)
        // Bead P3+ → Linear 4 (Low)
        let map = |p: u8| -> i32 {
            match p {
                0 => 1,
                1 => 2,
                2 => 3,
                _ => 4,
            }
        };
        assert_eq!(map(0), 1);
        assert_eq!(map(1), 2);
        assert_eq!(map(2), 3);
        assert_eq!(map(3), 4);
        assert_eq!(map(4), 4);
    }

    #[test]
    fn build_client_requires_valid_header() {
        // API key must be valid ASCII for HTTP header
        let result = build_client("lin_api_valid_key_123");
        assert!(result.is_ok());
    }

    #[test]
    fn build_client_rejects_invalid_header() {
        // Non-ASCII in header value should fail
        let result = build_client("invalid\x00key");
        assert!(result.is_err());
    }

    #[test]
    fn extract_phase_label_style() {
        assert_eq!(extract_phase("phase:1 foundation"), Some("1".to_string()));
        assert_eq!(extract_phase("phase:2"), Some("2".to_string()));
        assert_eq!(extract_phase("phase:3a something"), Some("3a".to_string()));
        assert_eq!(extract_phase("tags: phase:6 final"), Some("6".to_string()));
    }

    #[test]
    fn extract_phase_prose_style() {
        assert_eq!(extract_phase("Phase 1 foundation"), Some("1".to_string()));
        assert_eq!(extract_phase("Phase 2"), Some("2".to_string()));
        assert_eq!(
            extract_phase("Implements Phase 3 requirements"),
            Some("3".to_string())
        );
    }

    #[test]
    fn extract_phase_case_insensitive() {
        assert_eq!(extract_phase("PHASE:1"), Some("1".to_string()));
        assert_eq!(extract_phase("Phase:2"), Some("2".to_string()));
        assert_eq!(extract_phase("PHASE 3"), Some("3".to_string()));
    }

    #[test]
    fn extract_phase_none_when_absent() {
        assert_eq!(extract_phase("no phase here"), None);
        assert_eq!(extract_phase("just a regular bead"), None);
        assert_eq!(extract_phase(""), None);
    }

    #[test]
    fn extract_phase_label_takes_priority_over_prose() {
        // "phase:" label pattern is checked before "phase N" prose pattern
        assert_eq!(
            extract_phase("Phase 2 but also phase:1"),
            Some("1".to_string())
        );
        // When only prose style exists, it works
        assert_eq!(extract_phase("Phase 2 foundation"), Some("2".to_string()));
    }

    /// Integration test — only runs when LINEAR_API_KEY is set.
    #[tokio::test]
    async fn sync_live_linear() {
        let api_key = match std::env::var("LINEAR_API_KEY") {
            Ok(k) if !k.is_empty() => k,
            _ => {
                eprintln!("skipping: LINEAR_API_KEY not set");
                return;
            }
        };

        let client = build_client(&api_key).unwrap();
        let team_key = std::env::var("LINEAR_TEAM").unwrap_or_else(|_| "AGE".to_string());

        // Should be able to resolve team
        let team_id = resolve_team_id(&client, &team_key).await;
        assert!(team_id.is_ok(), "failed to resolve team {team_key}");
    }
}
