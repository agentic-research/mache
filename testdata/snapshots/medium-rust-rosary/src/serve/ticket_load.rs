//! Phase 0 of the Linear-escalation-triage EPIC (rosary-5dc9b0).
//!
//! Consolidates Linear + GitHub + Zendesk + bead context for a single ticket
//! into one MCP response, replacing the 4-5 separate manual lookups the user
//! performs today.
//!
//! Four pure-ish helpers compose the orchestrator `tool_ticket_load`:
//! - `extract_github_link` / `extract_zendesk_link` — URL-shape detection in free text
//! - `find_triage_bead` — locates an existing tracking bead by ticket id
//! - `assemble_context` — pure JSON assembly from the fetched parts
//!
//! Live integration (hitting Linear's GraphQL) is gated behind LINEAR_API_KEY
//! to keep `cargo test` deterministic and network-free.

use serde_json::Value;

use crate::pool::RepoPool;
use crate::store::BeadStore;

/// Strip query string and fragment from a path segment (e.g. `42?tab=foo` → `42`).
/// Used to keep numeric-id parsing robust against URLs that carry trackers.
fn strip_url_tail(segment: &str) -> &str {
    let cut = segment.find(['?', '#']).unwrap_or(segment.len());
    &segment[..cut]
}

/// Detect a GitHub issue or pull-request URL embedded in free text.
///
/// Matches `https://github.com/{org}/{repo}/(issues|pull)/{N}`. Returns the
/// first match, canonicalized to the four-segment form (query strings,
/// fragments, slugs — anything past the numeric id — are dropped). No regex
/// — whitespace tokenization + `str::strip_prefix` only.
pub(crate) fn extract_github_link(text: &str) -> Option<String> {
    for raw in text.split_whitespace() {
        let token = raw.trim_end_matches(|c: char| !c.is_alphanumeric() && c != '/');
        let Some(rest) = token.strip_prefix("https://github.com/") else {
            continue;
        };
        let parts: Vec<&str> = rest.split('/').collect();
        if parts.len() < 4 {
            continue;
        }
        if parts[2] != "issues" && parts[2] != "pull" {
            continue;
        }
        let id = strip_url_tail(parts[3]);
        if id.parse::<u64>().is_err() {
            continue;
        }
        return Some(format!(
            "https://github.com/{}/{}/{}/{}",
            parts[0], parts[1], parts[2], id
        ));
    }
    None
}

/// Detect a Zendesk ticket URL embedded in free text.
///
/// Matches `https://{subdomain}.zendesk.com/agent/tickets/{N}`. Returns the
/// first match with query strings and fragments stripped from the numeric id.
/// No regex — whitespace tokenization + `str::strip_prefix`.
pub(crate) fn extract_zendesk_link(text: &str) -> Option<String> {
    for raw in text.split_whitespace() {
        let token = raw.trim_end_matches(|c: char| !c.is_alphanumeric() && c != '/');
        let Some(rest) = token.strip_prefix("https://") else {
            continue;
        };
        let parts: Vec<&str> = rest.split('/').collect();
        if parts.len() < 4 {
            continue;
        }
        if !parts[0].ends_with(".zendesk.com") {
            continue;
        }
        if parts[1] != "agent" || parts[2] != "tickets" {
            continue;
        }
        let id = strip_url_tail(parts[3]);
        if id.parse::<u64>().is_err() {
            continue;
        }
        return Some(format!(
            "https://{}/{}/{}/{}",
            parts[0], parts[1], parts[2], id
        ));
    }
    None
}

/// Single-store search. Returns the first bead whose title/description/comments
/// match the ticket id, or None. Factored out so it's testable against an
/// in-memory `SqliteBeadStore` without standing up a full `RepoPool`.
pub(crate) async fn find_triage_bead_in_store(
    store: &dyn BeadStore,
    repo_name: &str,
    ticket_id: &str,
) -> Option<Value> {
    let results = store.search_beads(ticket_id, repo_name, 1).await.ok()?;
    let bead = results.into_iter().next()?;
    serde_json::to_value(bead).ok()
}

/// Walk every repo in the pool, return the first bead found that references
/// `ticket_id`. Errors from any single store are skipped — one broken repo
/// doesn't block discovery in the others.
pub(crate) async fn find_triage_bead(pool: &RepoPool, ticket_id: &str) -> Option<Value> {
    for (repo_name, client) in pool.iter_clients() {
        if let Some(found) = find_triage_bead_in_store(client, repo_name, ticket_id).await {
            return Some(found);
        }
    }
    None
}

/// Pure JSON assembly: combine the fetched Linear data, comments, optional GH
/// link, optional Zendesk URL, and optional existing-bead reference into the
/// `rsry_ticket_load` response shape documented in the bead.
pub(crate) fn assemble_context(
    linear: Value,
    comments: Vec<Value>,
    github: Option<Value>,
    zendesk: Option<String>,
    bead: Option<Value>,
) -> Value {
    serde_json::json!({
        "linear": linear,
        "comments": comments,
        "linked_github": github,
        "linked_zendesk": zendesk,
        "existing_bead": bead,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    /// `extract_github_link` recognizes the canonical issues URL shape.
    #[test]
    fn extract_github_link_finds_issue_url() {
        let body = "Related: https://github.com/agentic-research/rosary/issues/42 — see thread.";
        assert_eq!(
            extract_github_link(body).as_deref(),
            Some("https://github.com/agentic-research/rosary/issues/42"),
        );
    }

    /// Absent the canonical URL shape, returns None — null-safe for callers.
    #[test]
    fn extract_github_link_returns_none_when_absent() {
        assert!(extract_github_link("no links here, plain prose").is_none());
    }

    /// Recognizes the Zendesk agent ticket URL shape.
    #[test]
    fn extract_zendesk_link_finds_ticket_url() {
        let body = "Customer ref: https://chainguard.zendesk.com/agent/tickets/12345 — escalated.";
        assert_eq!(
            extract_zendesk_link(body).as_deref(),
            Some("https://chainguard.zendesk.com/agent/tickets/12345"),
        );
    }

    /// Mid-sentence URLs trail punctuation; the detector must strip it so the
    /// canonical URL doesn't include the comma/period/paren.
    #[test]
    fn extract_github_link_strips_trailing_punctuation() {
        let body = "See https://github.com/agentic-research/rosary/issues/42, plus more.";
        assert_eq!(
            extract_github_link(body).as_deref(),
            Some("https://github.com/agentic-research/rosary/issues/42"),
        );
    }

    /// Pull-request URLs are equally valid `linked_github`.
    #[test]
    fn extract_github_link_accepts_pull_request_form() {
        let body = "Backports https://github.com/agentic-research/rosary/pull/214 to v0.x";
        assert_eq!(
            extract_github_link(body).as_deref(),
            Some("https://github.com/agentic-research/rosary/pull/214"),
        );
    }

    /// Non-numeric trailing segment is not an issue number; reject so callers
    /// don't get garbage links.
    #[test]
    fn extract_github_link_rejects_non_numeric_issue() {
        let body = "https://github.com/agentic-research/rosary/issues/foo";
        assert!(extract_github_link(body).is_none());
    }

    /// First match wins — deterministic behavior so the response doesn't
    /// shuffle on retries.
    #[test]
    fn extract_github_link_returns_first_when_multiple() {
        let body = "https://github.com/o/r/issues/1 and also https://github.com/o/r/issues/2 here.";
        assert_eq!(
            extract_github_link(body).as_deref(),
            Some("https://github.com/o/r/issues/1"),
        );
    }

    /// Non-zendesk hostnames must not be misclassified, even if they share
    /// the `/agent/tickets/` path shape.
    #[test]
    fn extract_zendesk_link_rejects_non_zendesk_host() {
        let body = "https://example.com/agent/tickets/9";
        assert!(extract_zendesk_link(body).is_none());
    }

    /// Copilot review #215: URLs often carry `?tab=...` or `#anchor`; the
    /// canonical form must still emerge from those inputs, not be rejected by
    /// `parse::<u64>()` choking on the trailing characters.
    #[test]
    fn extract_github_link_handles_query_string() {
        let body = "https://github.com/agentic-research/rosary/issues/42?tab=files";
        assert_eq!(
            extract_github_link(body).as_deref(),
            Some("https://github.com/agentic-research/rosary/issues/42"),
        );
    }

    /// Same shape, fragment instead of query.
    #[test]
    fn extract_github_link_handles_fragment() {
        let body = "see https://github.com/agentic-research/rosary/pull/214#discussion_r1";
        assert_eq!(
            extract_github_link(body).as_deref(),
            Some("https://github.com/agentic-research/rosary/pull/214"),
        );
    }

    /// Zendesk URLs carry tabs/anchors too.
    #[test]
    fn extract_zendesk_link_handles_query_string() {
        let body = "https://chainguard.zendesk.com/agent/tickets/12345?source=email";
        assert_eq!(
            extract_zendesk_link(body).as_deref(),
            Some("https://chainguard.zendesk.com/agent/tickets/12345"),
        );
    }

    /// All-optional-absent path: every linked-* and existing_bead field is
    /// JSON null. Caller distinguishes "we looked and there was nothing"
    /// (null) from "we didn't look" (field absent).
    #[test]
    fn assemble_context_renders_absent_fields_as_null() {
        let linear = json!({ "title": "x" });
        let out = assemble_context(linear, vec![], None, None, None);
        assert!(out["linked_github"].is_null());
        assert!(out["linked_zendesk"].is_null());
        assert!(out["existing_bead"].is_null());
        assert!(
            out["comments"]
                .as_array()
                .map(|a| a.is_empty())
                .unwrap_or(false)
        );
    }

    /// Empty pool → no repos to search → None. Caller renders as `null` in the
    /// `existing_bead` field of the consolidated response.
    #[tokio::test]
    async fn find_triage_bead_returns_none_for_empty_pool() {
        let pool = RepoPool::empty();
        assert!(find_triage_bead(&pool, "CUS-495").await.is_none());
    }

    /// Discovers a bead whose title references the ticket id. Hits the
    /// store-level helper directly so the test doesn't need a `RepoPool`
    /// factory.
    #[tokio::test]
    async fn find_triage_bead_returns_match_when_ticket_in_title() {
        use crate::bead_sqlite::SqliteBeadStore;
        use std::path::Path;

        let store = SqliteBeadStore::connect(Path::new(":memory:")).unwrap();
        store
            .create_bead(
                "rosary-trk1",
                "Track CUS-495 escalation",
                "from-linear",
                1,
                "task",
            )
            .await
            .unwrap();

        let hit = find_triage_bead_in_store(&store, "rosary", "CUS-495")
            .await
            .expect("seeded bead should be discoverable by ticket id");
        assert_eq!(hit["id"].as_str(), Some("rosary-trk1"));
        assert_eq!(hit["status"].as_str(), Some("open"));
    }

    /// No hit → None — the orchestrator renders this as `existing_bead: null`.
    #[tokio::test]
    async fn find_triage_bead_returns_none_when_no_match() {
        use crate::bead_sqlite::SqliteBeadStore;
        use std::path::Path;

        let store = SqliteBeadStore::connect(Path::new(":memory:")).unwrap();
        store
            .create_bead("rosary-x", "Unrelated bead", "", 2, "task")
            .await
            .unwrap();

        assert!(
            find_triage_bead_in_store(&store, "rosary", "CUS-495")
                .await
                .is_none()
        );
    }

    /// `assemble_context` produces the documented MCP response shape:
    /// non-null fields where data was provided, JSON null where absent.
    #[test]
    fn assemble_context_handles_all_optionals_present() {
        let linear = json!({ "title": "x", "body": "y", "status": "Triage" });
        let comments = vec![json!({"body": "first"})];
        let gh = Some(json!({"url": "https://github.com/o/r/issues/1", "state": "open"}));
        let zendesk = Some("https://zd.example.com/agent/tickets/9".to_string());
        let bead: Option<Value> = None;

        let out = assemble_context(linear, comments, gh, zendesk, bead);

        assert_eq!(out["linear"]["title"].as_str(), Some("x"));
        assert_eq!(out["linked_github"]["state"].as_str(), Some("open"));
        assert_eq!(
            out["linked_zendesk"].as_str(),
            Some("https://zd.example.com/agent/tickets/9"),
        );
        assert!(out["existing_bead"].is_null());
    }
}
