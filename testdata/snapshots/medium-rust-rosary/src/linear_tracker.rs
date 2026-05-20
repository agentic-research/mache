//! Linear implementation of the IssueTracker trait.

use anyhow::{Context, Result};
use serde_json::{Value, json};

use crate::bead::{BeadState, BeadUpdate};
use crate::sync::{ExternalIssue, IssueTracker};

const LINEAR_API_URL: &str = "https://api.linear.app/graphql";

/// A cached Linear workflow state: id, name, type.
#[derive(Debug, Clone)]
struct CachedState {
    id: String,
    name: String,
    state_type: String,
}

pub struct LinearTracker {
    client: reqwest::Client,
    team_id: String,
    team_key: String,
    /// Cached workflow states, fetched once at init.
    states: Vec<CachedState>,
    /// Optional config overrides: bead_status → linear_state_name.
    state_overrides: std::collections::HashMap<String, String>,
}

impl LinearTracker {
    pub async fn new(api_key: &str, team_key: &str) -> Result<Self> {
        Self::with_overrides(api_key, team_key, std::collections::HashMap::new()).await
    }

    /// Create with explicit state mapping overrides from config.
    /// Keys are bead status strings, values are Linear state names.
    pub async fn with_overrides(
        api_key: &str,
        team_key: &str,
        state_overrides: std::collections::HashMap<String, String>,
    ) -> Result<Self> {
        use reqwest::header::{AUTHORIZATION, CONTENT_TYPE, HeaderMap, HeaderValue};

        let mut headers = HeaderMap::new();
        headers.insert(AUTHORIZATION, HeaderValue::from_str(api_key)?);
        headers.insert(CONTENT_TYPE, HeaderValue::from_static("application/json"));

        let client = reqwest::Client::builder()
            .default_headers(headers)
            .build()?;

        // Resolve team ID
        let team_id = resolve_team_id(&client, team_key).await?;

        // Cache workflow states at init (avoids re-fetching on every update)
        let states = fetch_team_states(&client, &team_id).await?;

        Ok(Self {
            client,
            team_id,
            team_key: team_key.to_string(),
            states,
            state_overrides,
        })
    }

    /// Find an existing label by name in the team, or create one.
    /// Returns the Linear label ID.
    async fn find_or_create_label(&self, name: &str) -> Result<String> {
        // Query existing labels for the team
        let query_str = r#"
            query TeamLabels($teamId: String!) {
                team(id: $teamId) {
                    labels { nodes { id name } }
                }
            }
        "#;
        let resp = graphql(&self.client, query_str, json!({ "teamId": self.team_id })).await?;
        let nodes = resp
            .pointer("/data/team/labels/nodes")
            .and_then(|v| v.as_array());

        // Check if label already exists
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
                "teamId": self.team_id,
            }
        });
        let resp = graphql(&self.client, mutation, variables).await?;
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

    /// Find the Linear internal UUID for an issue by its identifier (e.g., "AGE-5").
    async fn find_issue_id(&self, external_id: &str) -> Result<String> {
        let query_str = r#"
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

        let resp = graphql(&self.client, query_str, json!({ "filter": filter })).await?;
        resp.pointer("/data/issues/nodes/0/id")
            .and_then(|v| v.as_str())
            .map(|s| s.to_string())
            .context(format!("issue {external_id} not found in Linear"))
    }

    /// Resolve a BeadState to a Linear state ID.
    /// Priority: config override → name match → type match.
    fn resolve_state_id(&self, bead_state: BeadState) -> Option<&str> {
        let bead_status = bead_state.to_string();
        let (target_type, preferred_name) = bead_state.to_linear_type();

        // 1. Config override: user explicitly mapped this bead status to a Linear name
        if let Some(override_name) = self.state_overrides.get(&bead_status)
            && let Some(s) = self.states.iter().find(|s| s.name == *override_name)
        {
            return Some(&s.id);
        }

        // 2. Preferred name match within the target type
        if let Some(s) = self
            .states
            .iter()
            .find(|s| s.state_type == target_type && s.name == preferred_name)
        {
            return Some(&s.id);
        }

        // 3. Any state with the matching type (fallback)
        self.states
            .iter()
            .find(|s| s.state_type == target_type)
            .map(|s| s.id.as_str())
    }
}

async fn graphql(client: &reqwest::Client, query: &str, variables: Value) -> Result<Value> {
    let body = json!({ "query": query, "variables": variables });
    let resp = client
        .post(LINEAR_API_URL)
        .json(&body)
        .send()
        .await
        .context("Linear API request failed")?;

    let status = resp.status();
    let text = resp.text().await.context("reading Linear response")?;

    if !status.is_success() {
        anyhow::bail!("Linear API {status}: {text}");
    }

    let json: Value = serde_json::from_str(&text).context("parsing Linear response")?;
    if let Some(errors) = json.get("errors") {
        anyhow::bail!("Linear GraphQL errors: {errors}");
    }
    Ok(json)
}

async fn resolve_team_id(client: &reqwest::Client, team_key: &str) -> Result<String> {
    let query = r#"
        query Teams {
            teams { nodes { id key } }
        }
    "#;
    let resp = graphql(client, query, json!({})).await?;
    let teams = resp
        .pointer("/data/teams/nodes")
        .and_then(|v| v.as_array())
        .context("fetching teams")?;

    for team in teams {
        if team["key"].as_str() == Some(team_key)
            && let Some(id) = team["id"].as_str()
        {
            return Ok(id.to_string());
        }
    }
    anyhow::bail!("team '{team_key}' not found")
}

/// Fetch and cache all workflow states for a team.
async fn fetch_team_states(client: &reqwest::Client, team_id: &str) -> Result<Vec<CachedState>> {
    let query = r#"
        query TeamStates($teamId: String!) {
            team(id: $teamId) {
                states { nodes { id name type } }
            }
        }
    "#;
    let resp = graphql(client, query, json!({ "teamId": team_id })).await?;
    let nodes = resp
        .pointer("/data/team/states/nodes")
        .and_then(|v| v.as_array())
        .context("fetching workflow states")?;

    Ok(nodes
        .iter()
        .filter_map(|s| {
            Some(CachedState {
                id: s["id"].as_str()?.to_string(),
                name: s["name"].as_str()?.to_string(),
                state_type: s["type"].as_str()?.to_string(),
            })
        })
        .collect())
}

/// Map Linear state (type + name) to rosary status string.
/// Uses type for stability, name for refinement within started type.
fn map_linear_status(state_type: &str, state_name: &str) -> &'static str {
    match BeadState::from_linear_type(state_type, state_name) {
        BeadState::Done => "closed",
        BeadState::Dispatched => "in_progress",
        BeadState::Verifying => "verifying",
        // DeadLetter must round-trip: from_linear_type maps
        // (canceled, "Dead Letter") → DeadLetter, and this match must
        // emit "dead_letter" so the Linear→Dolt sync path preserves
        // the operator-triage semantics. Without this explicit arm
        // the `_ => "open"` fall-through would demote DeadLetter to
        // open on every sync — the exact failure mode flagged in
        // Copilot review round 3 on PR #202.
        BeadState::DeadLetter => "dead_letter",
        _ => "open",
    }
}

/// Map rosary priority to Linear priority.
fn to_linear_priority(priority: u8) -> i32 {
    match priority {
        0 => 1, // P0 → Urgent
        1 => 2, // P1 → High
        2 => 3, // P2 → Medium
        _ => 4, // P3+ → Low
    }
}

#[async_trait::async_trait]
impl IssueTracker for LinearTracker {
    async fn list_open(&self) -> Result<Vec<ExternalIssue>> {
        let query = r#"
            query TeamIssues($teamId: String!, $filter: IssueFilter) {
                team(id: $teamId) {
                    issues(first: 250, filter: $filter) {
                        nodes {
                            identifier
                            title
                            description
                            priority
                            state { name type }
                            labels { nodes { name } }
                        }
                    }
                }
            }
        "#;

        let variables = json!({
            "teamId": self.team_id,
            "filter": {
                "state": { "type": { "in": ["started", "unstarted", "backlog"] } }
            }
        });

        let resp = graphql(&self.client, query, variables).await?;
        let nodes = resp
            .pointer("/data/team/issues/nodes")
            .and_then(|v| v.as_array())
            .context("fetching issues")?;

        let issues = nodes
            .iter()
            .map(|n| {
                let labels: Vec<String> = n
                    .pointer("/labels/nodes")
                    .and_then(|v| v.as_array())
                    .map(|arr| {
                        arr.iter()
                            .filter_map(|l| l["name"].as_str().map(String::from))
                            .collect()
                    })
                    .unwrap_or_default();

                let state_name = n
                    .pointer("/state/name")
                    .and_then(|v| v.as_str())
                    .unwrap_or("Todo");
                let state_type = n
                    .pointer("/state/type")
                    .and_then(|v| v.as_str())
                    .unwrap_or("unstarted");

                ExternalIssue {
                    external_id: n["identifier"].as_str().unwrap_or("").to_string(),
                    title: n["title"].as_str().unwrap_or("").to_string(),
                    description: n["description"].as_str().unwrap_or("").to_string(),
                    status: map_linear_status(state_type, state_name).to_string(),
                    priority: n["priority"].as_u64().unwrap_or(3) as u8,
                    labels,
                }
            })
            .collect();

        Ok(issues)
    }

    async fn create(&self, issue: &ExternalIssue) -> Result<String> {
        let mutation = r#"
            mutation CreateIssue($input: IssueCreateInput!) {
                issueCreate(input: $input) {
                    success
                    issue { identifier }
                }
            }
        "#;

        // Resolve label IDs for any perspective labels
        let mut label_ids: Vec<String> = Vec::new();
        for label_name in &issue.labels {
            if label_name.starts_with("perspective:") {
                match self.find_or_create_label(label_name).await {
                    Ok(id) => label_ids.push(id),
                    Err(e) => {
                        eprintln!("[linear] warning: could not resolve label '{label_name}': {e}");
                    }
                }
            }
        }

        let mut input = json!({
            "teamId": self.team_id,
            "title": issue.title,
            "description": issue.description,
            "priority": to_linear_priority(issue.priority),
        });

        if !label_ids.is_empty() {
            input["labelIds"] = json!(label_ids);
        }

        let variables = json!({ "input": input });

        let resp = graphql(&self.client, mutation, variables).await?;
        let success = resp
            .pointer("/data/issueCreate/success")
            .and_then(|v| v.as_bool())
            .unwrap_or(false);

        if !success {
            anyhow::bail!("issueCreate failed");
        }

        Ok(resp
            .pointer("/data/issueCreate/issue/identifier")
            .and_then(|v| v.as_str())
            .unwrap_or("???")
            .to_string())
    }

    async fn update_status(&self, external_id: &str, status: &str) -> Result<()> {
        let bead_state = BeadState::from(status);
        let target_state = self
            .resolve_state_id(bead_state)
            .context(format!(
                "no Linear state for bead status '{status}' (type={}, name={})",
                bead_state.to_linear_type().0,
                bead_state.to_linear_type().1
            ))?
            .to_string();

        let issue_id = self.find_issue_id(external_id).await?;

        let mutation = r#"
            mutation UpdateIssue($id: String!, $input: IssueUpdateInput!) {
                issueUpdate(id: $id, input: $input) { success }
            }
        "#;
        let resp = graphql(
            &self.client,
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

    async fn update_fields(&self, external_id: &str, update: &BeadUpdate) -> Result<()> {
        let issue_id = self.find_issue_id(external_id).await?;

        let mut input = json!({});
        if let Some(ref title) = update.title {
            input["title"] = json!(title);
        }
        if let Some(ref description) = update.description {
            input["description"] = json!(description);
        }
        if let Some(priority) = update.priority {
            input["priority"] = json!(to_linear_priority(priority));
        }

        if input.as_object().is_none_or(|m| m.is_empty()) {
            return Ok(()); // Nothing Linear can update
        }

        let mutation = r#"
            mutation UpdateIssue($id: String!, $input: IssueUpdateInput!) {
                issueUpdate(id: $id, input: $input) { success }
            }
        "#;
        let resp = graphql(
            &self.client,
            mutation,
            json!({ "id": issue_id, "input": input }),
        )
        .await?;

        let success = resp
            .pointer("/data/issueUpdate/success")
            .and_then(|v| v.as_bool())
            .unwrap_or(false);
        if !success {
            anyhow::bail!("issueUpdate (fields) failed for {external_id}");
        }

        Ok(())
    }

    fn name(&self) -> &str {
        "linear"
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn map_status_done() {
        assert_eq!(map_linear_status("completed", "Done"), "closed");
        assert_eq!(map_linear_status("canceled", "Cancelled"), "closed");
    }

    /// Regression for Copilot review round 3 on PR #202: the Linear→Dolt
    /// sync path used `_ => "open"` as a fall-through. With DeadLetter now
    /// a representable variant, that fall-through would have demoted any
    /// "Dead Letter" state in Linear to `open` on every sync, defeating
    /// the operator-triage semantics. This test pins the explicit
    /// `BeadState::DeadLetter => "dead_letter"` arm.
    #[test]
    fn map_status_dead_letter_preserves() {
        assert_eq!(
            map_linear_status("canceled", "Dead Letter"),
            "dead_letter",
            "Linear→rosary sync must preserve DeadLetter — not fall through to open"
        );
    }

    #[test]
    fn map_status_in_progress() {
        assert_eq!(map_linear_status("started", "In Progress"), "in_progress");
    }

    #[test]
    fn map_status_in_review() {
        assert_eq!(map_linear_status("started", "In Review"), "verifying");
    }

    #[test]
    fn map_status_open() {
        assert_eq!(map_linear_status("unstarted", "Todo"), "open");
        assert_eq!(map_linear_status("backlog", "Backlog"), "open");
    }

    #[test]
    fn map_status_custom_names() {
        // Custom team names — type-based matching still works
        assert_eq!(map_linear_status("started", "Working On It"), "in_progress");
        assert_eq!(map_linear_status("started", "Peer Review"), "verifying");
        assert_eq!(map_linear_status("completed", "Shipped"), "closed");
        assert_eq!(map_linear_status("unstarted", "Planned"), "open");
    }

    #[test]
    fn priority_mapping() {
        assert_eq!(to_linear_priority(0), 1); // P0 → Urgent
        assert_eq!(to_linear_priority(1), 2); // P1 → High
        assert_eq!(to_linear_priority(2), 3); // P2 → Medium
        assert_eq!(to_linear_priority(3), 4); // P3 → Low
    }
}
