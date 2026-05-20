//! Cross-repo external-ref sync.
//!
//! When a bead carries an `external_ref` (e.g. `"kiln:ll-packaging"`), this
//! module creates a **mirror bead** in the target repo with a back-reference
//! and propagates status changes between source and mirror on each scan.
use crate::bead::Bead;
use crate::store::BeadStore;
use std::collections::HashMap;

/// A parsed external reference: "repo_name:label".
#[derive(Debug, Clone)]
pub struct ExternalRef {
    /// The bead that holds this external ref.
    pub source_bead_id: String,
    /// The repo the source bead lives in.
    pub source_repo: String,
    /// The target repo name (maps to a repo in rosary.toml).
    pub target_repo: String,
    /// The label/identifier within the target repo.
    pub label: String,
    /// Current status of the source bead.
    pub source_status: String,
    /// Title of the source bead (used when creating mirror).
    pub source_title: String,
}

/// Parse external refs from scanned beads.
pub fn find_external_refs(beads: &[Bead]) -> Vec<ExternalRef> {
    beads
        .iter()
        .filter_map(|bead| {
            // Skip xref mirrors to prevent infinite recursion
            if bead.id.starts_with("xref-") {
                return None;
            }
            let ext_ref = bead.external_ref.as_deref()?;
            let (target_repo, label) = ext_ref.split_once(':')?;
            Some(ExternalRef {
                source_bead_id: bead.id.clone(),
                source_repo: bead.repo.clone(),
                target_repo: target_repo.to_string(),
                label: label.to_string(),
                source_status: bead.status.clone(),
                source_title: bead.title.clone(),
            })
        })
        .collect()
}

/// Sync external refs: ensure mirror beads exist in target repos.
///
/// For each external ref:
/// 1. Check if a bead with a back-reference already exists in the target repo
/// 2. If not, create one via the target repo's BeadStore
/// 3. If yes, sync status if the source has changed
pub async fn sync_external_refs(
    refs: &[ExternalRef],
    dolt_clients: &HashMap<String, Box<dyn BeadStore>>,
    all_beads: &[Bead],
) {
    for ext_ref in refs {
        let Some(client) = dolt_clients.get(&ext_ref.target_repo) else {
            continue;
        };

        let back_ref = format!("{}:{}", ext_ref.source_repo, ext_ref.source_bead_id);

        // Check if mirror already exists in target repo
        let mirror_exists = all_beads
            .iter()
            .any(|b| b.repo == ext_ref.target_repo && b.external_ref.as_deref() == Some(&back_ref));

        if mirror_exists {
            // Find the mirror bead and sync status if needed
            if let Some(mirror) = all_beads.iter().find(|b| {
                b.repo == ext_ref.target_repo && b.external_ref.as_deref() == Some(&back_ref)
            }) && mirror.status != ext_ref.source_status
            {
                eprintln!(
                    "[xref] status drift: {} ({}) != {} ({}), source wins",
                    ext_ref.source_bead_id, ext_ref.source_status, mirror.id, mirror.status,
                );
                if let Err(e) = client
                    .update_status(&mirror.id, &ext_ref.source_status)
                    .await
                {
                    eprintln!("[xref] failed to sync status to {}: {e}", mirror.id);
                }
            }
        } else {
            // Create mirror bead in target repo
            let mirror_id = format!("xref-{}-{}", ext_ref.source_repo, &ext_ref.source_bead_id);
            let title = format!(
                "[cross-repo] {} (from {})",
                ext_ref.source_title, ext_ref.source_repo
            );
            let description = format!(
                "Mirror of {} in {} repo.\nExternal ref: {}\nLabel: {}",
                ext_ref.source_bead_id, ext_ref.source_repo, back_ref, ext_ref.label,
            );

            eprintln!(
                "[xref] creating mirror bead {} in {} for {}",
                mirror_id, ext_ref.target_repo, ext_ref.source_bead_id
            );

            if let Err(e) = client
                .create_bead(&mirror_id, &title, &description, 2, "task")
                .await
            {
                eprintln!("[xref] failed to create mirror {mirror_id}: {e}");
            } else {
                client
                    .log_event(&mirror_id, "xref_created", &back_ref)
                    .await;
                if let Err(e) = client.set_external_ref(&mirror_id, &back_ref).await {
                    eprintln!("[xref] failed to set back-ref on {mirror_id}: {e}");
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::Utc;

    fn make_bead(id: &str, repo: &str, ext_ref: Option<&str>) -> Bead {
        Bead {
            id: id.to_string(),
            title: format!("Test bead {id}"),
            description: String::new(),
            status: "open".to_string(),
            priority: 2,
            issue_type: "task".to_string(),
            owner: None,
            repo: repo.to_string(),
            created_at: Utc::now(),
            updated_at: Utc::now(),
            dependency_count: 0,
            dependent_count: 0,
            comment_count: 0,
            branch: None,
            pr_url: None,
            jj_change_id: None,
            external_ref: ext_ref.map(|s| s.to_string()),
            files: Vec::new(),
            test_files: Vec::new(),
            created_by: None,
            scope: String::new(),
            derived_from: vec![],
        }
    }

    #[test]
    fn parse_external_refs() {
        let beads = vec![
            make_bead("loom-abc", "rosary", Some("kiln:ll-packaging")),
            make_bead("loom-def", "rosary", None),
            make_bead("loom-ghi", "rosary", Some("mache:structural-query")),
        ];

        let refs = find_external_refs(&beads);
        assert_eq!(refs.len(), 2);
        assert_eq!(refs[0].target_repo, "kiln");
        assert_eq!(refs[0].label, "ll-packaging");
        assert_eq!(refs[0].source_bead_id, "loom-abc");
        assert_eq!(refs[1].target_repo, "mache");
        assert_eq!(refs[1].label, "structural-query");
    }

    #[test]
    fn parse_invalid_external_ref() {
        let beads = vec![make_bead("loom-x", "rosary", Some("no-colon"))];
        let refs = find_external_refs(&beads);
        assert!(refs.is_empty(), "refs without colon should be skipped");
    }

    #[test]
    fn parse_empty_beads() {
        let refs = find_external_refs(&[]);
        assert!(refs.is_empty());
    }
}
