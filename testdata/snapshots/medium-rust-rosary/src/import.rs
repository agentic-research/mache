//! Shared bead import logic — used by both CLI and MCP handlers.

use anyhow::{Context, Result};
use serde_json::Value;

use crate::store::BeadStore;

/// Read a JSON bead array from a file path or stdin.
pub fn read_beads_json(file: Option<String>) -> Result<Vec<Value>> {
    let json_str = match file {
        Some(path) => std::fs::read_to_string(&path).with_context(|| format!("reading {path}"))?,
        None => {
            use std::io::Read as _;
            let mut buf = String::new();
            std::io::stdin().read_to_string(&mut buf)?;
            buf
        }
    };
    Ok(serde_json::from_str(&json_str)?)
}

/// Serialize beads to the export JSON format (includes repo, status for cross-repo round-trip).
pub fn export_beads_json(beads: &[crate::bead::Bead]) -> Vec<Value> {
    beads
        .iter()
        .map(|b| {
            serde_json::json!({
                "repo": b.repo,
                "title": b.title,
                "description": b.description,
                "priority": b.priority,
                "issue_type": b.issue_type,
                "status": b.status,
                "files": b.files,
                "test_files": b.test_files,
            })
        })
        .collect()
}

/// Result of importing a batch of beads into a single repo.
pub struct ImportResult {
    pub imported: usize,
    pub skipped: usize,
    pub ids: Vec<String>,
}

/// Parse a JSON bead value into fields and create it via the BeadStore.
/// Returns `Some(id)` if created, `None` if skipped (duplicate title).
pub async fn import_bead(
    bead: &Value,
    client: &dyn BeadStore,
    repo_name: &str,
) -> Result<Option<String>> {
    let title = bead["title"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("bead title required"))?;

    // Dedup: skip if exact title match exists
    let existing = client.search_beads(title, repo_name, 10).await?;
    if existing.iter().any(|b| b.title == title) {
        return Ok(None);
    }

    let description = bead["description"].as_str().unwrap_or("");
    let priority = bead["priority"].as_u64().unwrap_or(2) as u8;
    let issue_type = bead["issue_type"].as_str().unwrap_or("task");
    let owner = crate::dispatch::default_agent(issue_type);
    let files: Vec<String> = bead
        .get("files")
        .and_then(|v| v.as_array())
        .map(|arr| {
            arr.iter()
                .filter_map(|v| v.as_str().map(String::from))
                .collect()
        })
        .unwrap_or_default();
    let test_files: Vec<String> = bead
        .get("test_files")
        .and_then(|v| v.as_array())
        .map(|arr| {
            arr.iter()
                .filter_map(|v| v.as_str().map(String::from))
                .collect()
        })
        .unwrap_or_default();

    let id = crate::generate_bead_id(repo_name);
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
            &[],
            None,
            "",
            &[],
        )
        .await?;

    Ok(Some(id))
}

/// Import a batch of beads into a single repo. Returns counts + created IDs.
pub async fn import_beads(
    beads: &[Value],
    client: &dyn BeadStore,
    repo_name: &str,
) -> Result<ImportResult> {
    let mut result = ImportResult {
        imported: 0,
        skipped: 0,
        ids: Vec::new(),
    };

    for bead in beads {
        match import_bead(bead, client, repo_name).await? {
            Some(id) => {
                result.ids.push(id);
                result.imported += 1;
            }
            None => result.skipped += 1,
        }
    }

    Ok(result)
}
