//! Assay scan mode — runs `hook = "assay.scan"` plugins and files chore beads
//! for each stale code reference they report.
//!
//! Invoked by `rsry scan --assay`. No LLM required: the stale ref is the bead
//! title; the source markdown file is the file_scope.

use anyhow::Result;

use crate::bead_sqlite::connect_bead_store;
use crate::config::RepoConfig;
use crate::plugin::{PluginRegistry, StaleRef};
use crate::scanner::expand_path;

/// Run assay scan across all repos and file P3 chore beads for stale refs.
///
/// Returns the number of chore beads created.
pub async fn run_assay_scan(repos: &[RepoConfig], registry: &PluginRegistry) -> Result<u32> {
    let mut created = 0u32;

    for repo in repos {
        let path = expand_path(&repo.path);
        let work_dir = path.to_string_lossy();

        let stale = registry.assay_scan(&repo.name, &work_dir);
        if stale.is_empty() {
            continue;
        }

        let beads_dir = path.join(".beads");
        if !beads_dir.exists() {
            eprintln!(
                "[assay] no .beads/ in {}, skipping chore creation",
                repo.name
            );
            continue;
        }

        let store = connect_bead_store(&beads_dir).await?;

        for stale_ref in &stale {
            let title = chore_title(stale_ref);
            let description = chore_description(stale_ref);
            let id = crate::generate_bead_id(&title);

            match store
                .create_bead_full(
                    &id,
                    &title,
                    &description,
                    3, // P3
                    "chore",
                    "dev-agent",
                    std::slice::from_ref(&stale_ref.source_file),
                    &[],
                    &[],
                    None,
                    "",
                    &[],
                )
                .await
            {
                Ok(()) => {
                    eprintln!(
                        "[assay] filed chore bead {id}: stale `{}` in {}",
                        stale_ref.symbol, stale_ref.source_file
                    );
                    created += 1;
                }
                Err(e) => {
                    eprintln!("[assay] failed to create bead for stale ref: {e:#}");
                }
            }
        }
    }

    Ok(created)
}

fn chore_title(r: &StaleRef) -> String {
    format!("chore: fix stale ref `{}` in {}", r.symbol, r.source_file)
}

fn chore_description(r: &StaleRef) -> String {
    let loc = match r.line {
        Some(n) => format!(" (line {n})"),
        None => String::new(),
    };
    format!(
        "Stale code reference detected by assay scan.\n\
         Source file: {}{}\n\
         Symbol: `{}`\n\
         \n\
         The referenced symbol no longer exists. Update or remove the reference.",
        r.source_file, loc, r.symbol
    )
}

// ── Tests ─────────────────────────────────────────────────────────────────────

#[cfg(test)]
mod scan_assay_stale {
    use super::*;
    use crate::plugin::StaleRef;

    fn make_ref(source_file: &str, symbol: &str, line: Option<u32>) -> StaleRef {
        StaleRef {
            source_file: source_file.to_string(),
            symbol: symbol.to_string(),
            line,
        }
    }

    #[test]
    fn chore_title_format() {
        let r = make_ref("docs/guide.md", "old_fn", None);
        assert_eq!(
            chore_title(&r),
            "chore: fix stale ref `old_fn` in docs/guide.md"
        );
    }

    #[test]
    fn chore_description_with_line() {
        let r = make_ref("docs/guide.md", "old_fn", Some(42));
        let desc = chore_description(&r);
        assert!(desc.contains("line 42"));
        assert!(desc.contains("old_fn"));
        assert!(desc.contains("docs/guide.md"));
    }

    #[test]
    fn chore_description_without_line() {
        let r = make_ref("docs/guide.md", "old_fn", None);
        let desc = chore_description(&r);
        assert!(!desc.contains("line"));
        assert!(desc.contains("docs/guide.md"));
    }

    #[tokio::test]
    async fn assay_scan_creates_chore_beads() {
        use crate::config::{PluginConfig, PluginKind, RepoConfig};
        use crate::plugin::PluginRegistry;
        use std::io::Write;

        // Write a shell script that outputs a stale assay result.
        let dir = tempfile::tempdir().unwrap();
        let script_path = dir.path().join("assay.sh");
        let mut f = std::fs::File::create(&script_path).unwrap();
        writeln!(
            f,
            "#!/bin/sh\necho '{{\"verdict\":\"stale\",\"stale_refs\":[\
             {{\"source_file\":\"docs/guide.md\",\"symbol\":\"old_fn\",\"line\":10}}]}}'"
        )
        .unwrap();
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(&script_path, std::fs::Permissions::from_mode(0o755)).unwrap();
        }

        let plugin = PluginConfig {
            name: "test-assay".to_string(),
            kind: PluginKind::Hook,
            hook: "assay.scan".to_string(),
            command: vec![script_path.to_str().unwrap().to_string()],
            url: None,
        };
        let registry = PluginRegistry::new(vec![plugin]);

        // Set up a repo with .beads/
        let repo_dir = tempfile::tempdir().unwrap();
        let beads_dir = repo_dir.path().join(".beads");
        std::fs::create_dir_all(&beads_dir).unwrap();
        let store = crate::bead_sqlite::connect_bead_store(&beads_dir)
            .await
            .unwrap();
        drop(store);

        let repo = RepoConfig {
            name: "test-repo".to_string(),
            path: repo_dir.path().to_path_buf(),
            lang: None,
            self_managed: false,
            approval: crate::config::DispatchApproval::Approved,
        };

        let count = run_assay_scan(&[repo], &registry).await.unwrap();
        assert_eq!(count, 1, "expected 1 chore bead created");

        // Verify the bead exists in the store
        let store = crate::bead_sqlite::connect_bead_store(&beads_dir)
            .await
            .unwrap();
        let beads = store.list_beads("test-repo").await.unwrap();
        assert_eq!(beads.len(), 1);
        assert!(
            beads[0].title.contains("old_fn"),
            "bead title should contain symbol: {}",
            beads[0].title
        );
        assert_eq!(beads[0].issue_type, "chore");
    }

    #[tokio::test]
    async fn assay_scan_skips_pass_verdict() {
        use crate::config::{PluginConfig, PluginKind, RepoConfig};
        use crate::plugin::PluginRegistry;
        use std::io::Write;

        let dir = tempfile::tempdir().unwrap();
        let script_path = dir.path().join("assay.sh");
        let mut f = std::fs::File::create(&script_path).unwrap();
        writeln!(f, "#!/bin/sh\necho '{{\"verdict\":\"pass\"}}'").unwrap();
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(&script_path, std::fs::Permissions::from_mode(0o755)).unwrap();
        }

        let plugin = PluginConfig {
            name: "clean-assay".to_string(),
            kind: PluginKind::Hook,
            hook: "assay.scan".to_string(),
            command: vec![script_path.to_str().unwrap().to_string()],
            url: None,
        };
        let registry = PluginRegistry::new(vec![plugin]);

        let repo_dir = tempfile::tempdir().unwrap();
        let beads_dir = repo_dir.path().join(".beads");
        std::fs::create_dir_all(&beads_dir).unwrap();
        let store = crate::bead_sqlite::connect_bead_store(&beads_dir)
            .await
            .unwrap();
        drop(store);

        let repo = RepoConfig {
            name: "clean-repo".to_string(),
            path: repo_dir.path().to_path_buf(),
            lang: None,
            self_managed: false,
            approval: crate::config::DispatchApproval::Approved,
        };

        let count = run_assay_scan(&[repo], &registry).await.unwrap();
        assert_eq!(count, 0, "pass verdict should create no beads");
    }

    #[tokio::test]
    async fn assay_scan_no_plugins_returns_zero() {
        use crate::config::RepoConfig;
        use crate::plugin::PluginRegistry;

        let registry = PluginRegistry::new(vec![]);
        let repo_dir = tempfile::tempdir().unwrap();
        let repo = RepoConfig {
            name: "empty-repo".to_string(),
            path: repo_dir.path().to_path_buf(),
            lang: None,
            self_managed: false,
            approval: crate::config::DispatchApproval::Approved,
        };
        let count = run_assay_scan(&[repo], &registry).await.unwrap();
        assert_eq!(count, 0);
    }
}
