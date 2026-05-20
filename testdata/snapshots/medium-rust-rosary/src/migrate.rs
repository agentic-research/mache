//! Backend migration: export, import, backup, restore, verify.
//!
//! Moves orchestrator state between backends (Dolt → SQLite).
//! Source is never modified. Target uses upserts for idempotency.

use anyhow::Result;
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};

use crate::store::*;

/// Snapshot of all orchestrator state, fully serializable.
#[derive(Debug, Serialize, Deserialize)]
pub struct BackendSnapshot {
    pub version: u32,
    pub created_at: DateTime<Utc>,
    pub source_provider: String,
    pub decades: Vec<DecadeRecord>,
    pub threads: Vec<ThreadRecord>,
    /// (thread_id, bead_ref)
    pub thread_members: Vec<(String, WorkRef)>,
    pub pipelines: Vec<PipelineState>,
    pub dispatches: Vec<DispatchRecord>,
    pub dependencies: Vec<CrossRepoDep>,
    pub linear_links: Vec<LinearLink>,
    pub user_repos: Vec<UserRepo>,
}

/// Counts per table — used for verification.
#[derive(Debug, PartialEq)]
pub struct TableCounts {
    pub decades: usize,
    pub threads: usize,
    pub thread_members: usize,
    pub pipelines: usize,
    pub dispatches: usize,
    pub dependencies: usize,
    pub linear_links: usize,
    pub user_repos: usize,
}

impl BackendSnapshot {
    pub fn counts(&self) -> TableCounts {
        TableCounts {
            decades: self.decades.len(),
            threads: self.threads.len(),
            thread_members: self.thread_members.len(),
            pipelines: self.pipelines.len(),
            dispatches: self.dispatches.len(),
            dependencies: self.dependencies.len(),
            linear_links: self.linear_links.len(),
            user_repos: self.user_repos.len(),
        }
    }
}

/// Write a snapshot to disk as individual JSON files.
pub fn save_backup(snapshot: &BackendSnapshot, dir: &std::path::Path) -> Result<()> {
    use std::fs;
    fs::create_dir_all(dir)?;

    let manifest = serde_json::json!({
        "version": snapshot.version,
        "created_at": snapshot.created_at.to_rfc3339(),
        "source_provider": &snapshot.source_provider,
        "counts": {
            "decades": snapshot.decades.len(),
            "threads": snapshot.threads.len(),
            "thread_members": snapshot.thread_members.len(),
            "pipelines": snapshot.pipelines.len(),
            "dispatches": snapshot.dispatches.len(),
            "dependencies": snapshot.dependencies.len(),
            "linear_links": snapshot.linear_links.len(),
            "user_repos": snapshot.user_repos.len(),
        }
    });
    fs::write(
        dir.join("manifest.json"),
        serde_json::to_string_pretty(&manifest)?,
    )?;
    fs::write(
        dir.join("decades.json"),
        serde_json::to_string_pretty(&snapshot.decades)?,
    )?;
    fs::write(
        dir.join("threads.json"),
        serde_json::to_string_pretty(&snapshot.threads)?,
    )?;
    fs::write(
        dir.join("thread_members.json"),
        serde_json::to_string_pretty(&snapshot.thread_members)?,
    )?;
    fs::write(
        dir.join("pipelines.json"),
        serde_json::to_string_pretty(&snapshot.pipelines)?,
    )?;
    fs::write(
        dir.join("dispatches.json"),
        serde_json::to_string_pretty(&snapshot.dispatches)?,
    )?;
    fs::write(
        dir.join("dependencies.json"),
        serde_json::to_string_pretty(&snapshot.dependencies)?,
    )?;
    fs::write(
        dir.join("linear_links.json"),
        serde_json::to_string_pretty(&snapshot.linear_links)?,
    )?;
    fs::write(
        dir.join("user_repos.json"),
        serde_json::to_string_pretty(&snapshot.user_repos)?,
    )?;
    Ok(())
}

/// Read a backup from disk.
pub fn load_backup(dir: &std::path::Path) -> Result<BackendSnapshot> {
    use std::fs;
    let manifest: serde_json::Value =
        serde_json::from_str(&fs::read_to_string(dir.join("manifest.json"))?)?;
    Ok(BackendSnapshot {
        version: manifest["version"].as_u64().unwrap_or(1) as u32,
        created_at: DateTime::parse_from_rfc3339(
            manifest["created_at"].as_str().unwrap_or_default(),
        )?
        .with_timezone(&Utc),
        source_provider: manifest["source_provider"]
            .as_str()
            .unwrap_or("unknown")
            .to_string(),
        decades: serde_json::from_str(&fs::read_to_string(dir.join("decades.json"))?)?,
        threads: serde_json::from_str(&fs::read_to_string(dir.join("threads.json"))?)?,
        thread_members: serde_json::from_str(&fs::read_to_string(
            dir.join("thread_members.json"),
        )?)?,
        pipelines: serde_json::from_str(&fs::read_to_string(dir.join("pipelines.json"))?)?,
        dispatches: serde_json::from_str(&fs::read_to_string(dir.join("dispatches.json"))?)?,
        dependencies: serde_json::from_str(&fs::read_to_string(dir.join("dependencies.json"))?)?,
        linear_links: serde_json::from_str(&fs::read_to_string(dir.join("linear_links.json"))?)?,
        user_repos: serde_json::from_str(&fs::read_to_string(dir.join("user_repos.json"))?)?,
    })
}

/// Result of a migration — includes counts and verification status.
#[derive(Debug)]
pub struct MigrationReport {
    pub source_counts: TableCounts,
    pub target_counts: TableCounts,
    pub verified: bool,
    pub backup_path: Option<std::path::PathBuf>,
}

/// Full migration: export from source, backup, import to target, verify.
pub async fn migrate(
    source: &dyn BackendExport,
    target: &dyn BackendExport,
    source_provider: &str,
    backup_dir: Option<&std::path::Path>,
) -> Result<MigrationReport> {
    // 1. Export from source
    let snapshot = export_snapshot(source, source_provider).await?;
    let source_counts = snapshot.counts();

    // 2. Optional backup
    let backup_path = if let Some(dir) = backup_dir {
        save_backup(&snapshot, dir)?;
        Some(dir.to_path_buf())
    } else {
        None
    };

    // 3. Import to target
    import_snapshot(target, &snapshot).await?;

    // 4. Verify — re-export from target and compare counts.
    // Target may have MORE decades than source (stub decades for FK integrity),
    // so we check target >= source for decades, exact match for everything else.
    let verify_snapshot = export_snapshot(target, "verify").await?;
    let target_counts = verify_snapshot.counts();
    let verified = target_counts.decades >= source_counts.decades
        && target_counts.threads == source_counts.threads
        && target_counts.thread_members == source_counts.thread_members
        && target_counts.pipelines == source_counts.pipelines
        && target_counts.dispatches == source_counts.dispatches
        && target_counts.dependencies == source_counts.dependencies
        && target_counts.linear_links == source_counts.linear_links
        && target_counts.user_repos == source_counts.user_repos;

    Ok(MigrationReport {
        source_counts,
        target_counts,
        verified,
        backup_path,
    })
}

/// Import a snapshot into a backend store. Uses upserts for idempotency.
/// Auto-creates stub decades for any threads referencing missing decade IDs
/// (Dolt doesn't enforce FKs, so source data may have orphans).
pub async fn import_snapshot(
    target: &dyn BackendStore,
    snapshot: &BackendSnapshot,
) -> Result<TableCounts> {
    // Collect known decade IDs — mutable so we can track stubs
    let mut known_decades: std::collections::HashSet<String> =
        snapshot.decades.iter().map(|d| d.id.clone()).collect();

    // Auto-create stub decades for orphaned thread references (deduped)
    let mut stub_count = 0;
    for t in &snapshot.threads {
        if !known_decades.contains(&t.decade_id) {
            target
                .upsert_decade(&DecadeRecord {
                    id: t.decade_id.clone(),
                    title: t.decade_id.clone(),
                    source_path: String::new(),
                    status: "active".into(),
                })
                .await?;
            known_decades.insert(t.decade_id.clone());
            stub_count += 1;
        }
    }
    if stub_count > 0 {
        eprintln!("[migrate] created {stub_count} stub decades for orphaned thread references");
    }

    for d in &snapshot.decades {
        target.upsert_decade(d).await?;
    }
    for t in &snapshot.threads {
        target.upsert_thread(t).await?;
    }
    for (tid, bref) in &snapshot.thread_members {
        target.add_bead_to_thread(tid, bref).await?;
    }
    for p in &snapshot.pipelines {
        target.upsert_pipeline(p).await?;
    }
    for d in &snapshot.dispatches {
        target.upsert_dispatch(d).await?;
    }
    for dep in &snapshot.dependencies {
        target.add_dependency(dep).await?;
    }
    for ll in &snapshot.linear_links {
        target.upsert_linear_link(ll).await?;
    }
    for ur in &snapshot.user_repos {
        target.register_repo(ur).await?;
    }

    Ok(snapshot.counts())
}

/// Export all orchestrator state into a snapshot.
pub async fn export_snapshot(
    source: &dyn BackendExport,
    provider_name: &str,
) -> Result<BackendSnapshot> {
    Ok(BackendSnapshot {
        version: 1,
        created_at: Utc::now(),
        source_provider: provider_name.to_string(),
        decades: source.list_decades(None).await?,
        threads: source.all_threads().await?,
        thread_members: source.all_thread_members().await?,
        pipelines: source.list_active_pipelines().await?,
        dispatches: source.all_dispatches().await?,
        dependencies: source.all_dependencies().await?,
        linear_links: source.all_linear_links().await?,
        user_repos: source.all_user_repos().await?,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::store_sqlite::SqliteBackend;

    fn temp_backend() -> (SqliteBackend, tempfile::TempDir) {
        let dir = tempfile::TempDir::new().unwrap();
        let backend = SqliteBackend::connect(&dir.path().join("test.db")).unwrap();
        (backend, dir)
    }

    #[tokio::test]
    async fn export_empty_backend_returns_empty_snapshot() {
        let (store, _dir) = temp_backend();
        let snap = export_snapshot(&store, "sqlite").await.unwrap();
        assert_eq!(snap.version, 1);
        assert_eq!(snap.source_provider, "sqlite");
        let counts = snap.counts();
        assert_eq!(counts.decades, 0);
        assert_eq!(counts.threads, 0);
        assert_eq!(counts.thread_members, 0);
        assert_eq!(counts.pipelines, 0);
        assert_eq!(counts.dispatches, 0);
        assert_eq!(counts.dependencies, 0);
        assert_eq!(counts.linear_links, 0);
        assert_eq!(counts.user_repos, 0);
    }

    #[tokio::test]
    async fn export_populated_backend() {
        let (store, _dir) = temp_backend();

        // Decade
        store
            .upsert_decade(&DecadeRecord {
                id: "d1".into(),
                title: "Test Decade".into(),
                source_path: "docs/adr/001.md".into(),
                status: "active".into(),
            })
            .await
            .unwrap();

        // Thread
        store
            .upsert_thread(&ThreadRecord {
                id: "t1".into(),
                name: "Test Thread".into(),
                decade_id: "d1".into(),
                feature_branch: None,
            })
            .await
            .unwrap();

        // Thread member
        let bead = WorkRef {
            repo: "rosary".into(),
            bead_id: "b1".into(),
            scope: String::new(),
        };
        store.add_bead_to_thread("t1", &bead).await.unwrap();

        // Pipeline
        store
            .upsert_pipeline(&PipelineState {
                bead_ref: bead.clone(),
                pipeline_phase: 0,
                pipeline_agent: "dev-agent".into(),
                phase_status: "executing".into(),
                retries: 0,
                consecutive_reverts: 0,
                highest_verify_tier: None,
                last_generation: 1,
                backoff_until: None,
            })
            .await
            .unwrap();

        // Dispatch
        store
            .record_dispatch(&DispatchRecord {
                id: "disp-1".into(),
                bead_ref: bead.clone(),
                agent: "dev-agent".into(),
                provider: "claude".into(),
                started_at: Utc::now(),
                completed_at: None,
                outcome: None,
                work_dir: "/tmp/work".into(),
                session_id: None,
                workspace_path: None,
                chain_hash: None,
            })
            .await
            .unwrap();

        // Dependency
        let dep_target = WorkRef {
            repo: "mache".into(),
            bead_id: "m1".into(),
            scope: String::new(),
        };
        store
            .add_dependency(&CrossRepoDep {
                from: bead.clone(),
                to: dep_target,
                dep_type: "blocks".into(),
                evidence_tier: EvidenceTier::Asserted,
                source: "human".into(),
                observed_at: chrono::Utc::now(),
            })
            .await
            .unwrap();

        // Linear link
        store
            .upsert_linear_link(&LinearLink {
                bead_ref: bead.clone(),
                linear_id: "AGE-100".into(),
                linear_type: "issue".into(),
            })
            .await
            .unwrap();

        // User repo
        store
            .register_repo(&UserRepo {
                user_id: "james".into(),
                repo_url: "https://github.com/art/rosary".into(),
                repo_name: "rosary".into(),
                github_token_ref: None,
            })
            .await
            .unwrap();

        let snap = export_snapshot(&store, "sqlite").await.unwrap();
        let counts = snap.counts();
        assert_eq!(counts.decades, 1);
        assert_eq!(counts.threads, 1);
        assert_eq!(counts.thread_members, 1);
        assert_eq!(counts.pipelines, 1);
        assert_eq!(counts.dispatches, 1);
        assert_eq!(counts.dependencies, 1);
        assert_eq!(counts.linear_links, 1);
        assert_eq!(counts.user_repos, 1);
    }

    /// Helper: populate a backend with one of everything and export.
    async fn populated_snapshot(store: &SqliteBackend) -> BackendSnapshot {
        store
            .upsert_decade(&DecadeRecord {
                id: "d1".into(),
                title: "Decade".into(),
                source_path: "docs/adr/001.md".into(),
                status: "active".into(),
            })
            .await
            .unwrap();
        store
            .upsert_thread(&ThreadRecord {
                id: "t1".into(),
                name: "Thread".into(),
                decade_id: "d1".into(),
                feature_branch: None,
            })
            .await
            .unwrap();
        let bead = WorkRef {
            repo: "rosary".into(),
            bead_id: "b1".into(),
            scope: String::new(),
        };
        store.add_bead_to_thread("t1", &bead).await.unwrap();
        store
            .upsert_pipeline(&PipelineState {
                bead_ref: bead.clone(),
                pipeline_phase: 0,
                pipeline_agent: "dev-agent".into(),
                phase_status: "executing".into(),
                retries: 0,
                consecutive_reverts: 0,
                highest_verify_tier: None,
                last_generation: 1,
                backoff_until: None,
            })
            .await
            .unwrap();
        store
            .record_dispatch(&DispatchRecord {
                id: "disp-1".into(),
                bead_ref: bead.clone(),
                agent: "dev-agent".into(),
                provider: "claude".into(),
                started_at: Utc::now(),
                completed_at: None,
                outcome: None,
                work_dir: "/tmp/work".into(),
                session_id: None,
                workspace_path: None,
                chain_hash: None,
            })
            .await
            .unwrap();
        store
            .add_dependency(&CrossRepoDep {
                from: bead.clone(),
                to: WorkRef {
                    repo: "mache".into(),
                    bead_id: "m1".into(),
                    scope: String::new(),
                },
                dep_type: "blocks".into(),
                evidence_tier: EvidenceTier::Asserted,
                source: "human".into(),
                observed_at: chrono::Utc::now(),
            })
            .await
            .unwrap();
        store
            .upsert_linear_link(&LinearLink {
                bead_ref: bead.clone(),
                linear_id: "AGE-100".into(),
                linear_type: "issue".into(),
            })
            .await
            .unwrap();
        store
            .register_repo(&UserRepo {
                user_id: "james".into(),
                repo_url: "https://github.com/art/rosary".into(),
                repo_name: "rosary".into(),
                github_token_ref: None,
            })
            .await
            .unwrap();
        export_snapshot(store, "sqlite").await.unwrap()
    }

    #[tokio::test]
    async fn import_into_empty_backend() {
        let (source, _d1) = temp_backend();
        let snap = populated_snapshot(&source).await;

        let (target, _d2) = temp_backend();
        let counts = import_snapshot(&target, &snap).await.unwrap();
        assert_eq!(counts.decades, 1);
        assert_eq!(counts.threads, 1);
        assert_eq!(counts.thread_members, 1);
        assert_eq!(counts.pipelines, 1);
        assert_eq!(counts.dispatches, 1);
        assert_eq!(counts.dependencies, 1);
        assert_eq!(counts.linear_links, 1);
        assert_eq!(counts.user_repos, 1);

        // Verify data actually landed
        let re_snap = export_snapshot(&target, "sqlite").await.unwrap();
        assert_eq!(re_snap.counts(), snap.counts());
    }

    #[tokio::test]
    async fn import_is_idempotent() {
        let (source, _d1) = temp_backend();
        let snap = populated_snapshot(&source).await;

        let (target, _d2) = temp_backend();
        import_snapshot(&target, &snap).await.unwrap();
        // Second import — should not duplicate
        import_snapshot(&target, &snap).await.unwrap();
        let re_snap = export_snapshot(&target, "sqlite").await.unwrap();
        assert_eq!(snap.counts(), re_snap.counts());
    }

    #[tokio::test]
    async fn roundtrip_export_import_export() {
        let (source, _d1) = temp_backend();
        let snap1 = populated_snapshot(&source).await;

        let (target, _d2) = temp_backend();
        import_snapshot(&target, &snap1).await.unwrap();
        let snap2 = export_snapshot(&target, "sqlite").await.unwrap();

        assert_eq!(snap1.counts(), snap2.counts());
        // Verify specific records survived
        assert_eq!(snap1.decades[0].id, snap2.decades[0].id);
        assert_eq!(snap1.threads[0].name, snap2.threads[0].name);
        assert_eq!(snap1.dispatches[0].agent, snap2.dispatches[0].agent);
    }

    #[tokio::test]
    async fn save_and_load_backup_roundtrip() {
        let (store, _d1) = temp_backend();
        let snap = populated_snapshot(&store).await;

        let backup_dir = tempfile::TempDir::new().unwrap();
        save_backup(&snap, backup_dir.path()).unwrap();
        let loaded = load_backup(backup_dir.path()).unwrap();
        assert_eq!(snap.counts(), loaded.counts());
        assert_eq!(loaded.version, 1);
        assert_eq!(loaded.source_provider, "sqlite");
    }

    #[test]
    fn load_backup_missing_dir_errors() {
        assert!(load_backup(std::path::Path::new("/tmp/nonexistent-rsry-backup-xyz")).is_err());
    }

    #[tokio::test]
    async fn import_creates_stub_decades_for_orphaned_threads() {
        // Simulate Dolt data where threads reference decades that don't exist
        let snap = BackendSnapshot {
            version: 1,
            created_at: Utc::now(),
            source_provider: "dolt".into(),
            decades: vec![], // No decades!
            threads: vec![ThreadRecord {
                id: "missing-decade/thread1".into(),
                name: "Thread in missing decade".into(),
                decade_id: "missing-decade".into(),
                feature_branch: None,
            }],
            thread_members: vec![],
            pipelines: vec![],
            dispatches: vec![],
            dependencies: vec![],
            linear_links: vec![],
            user_repos: vec![],
        };

        let (target, _dir) = temp_backend();
        // This should NOT fail — stubs should be created
        let counts = import_snapshot(&target, &snap).await.unwrap();
        assert_eq!(counts.threads, 1);

        // Verify stub decade was created
        let decade = target.get_decade("missing-decade").await.unwrap();
        assert!(decade.is_some());
        assert_eq!(decade.unwrap().title, "missing-decade");
    }

    #[tokio::test]
    async fn migrate_sqlite_to_sqlite_with_verification() {
        let (source, _d1) = temp_backend();
        let _snap = populated_snapshot(&source).await;

        let (target, _d2) = temp_backend();
        let backup_dir = tempfile::TempDir::new().unwrap();

        let report = migrate(&source, &target, "sqlite", Some(backup_dir.path()))
            .await
            .unwrap();
        assert!(report.verified);
        assert_eq!(report.source_counts, report.target_counts);
        assert!(report.backup_path.is_some());
        // Backup files should exist on disk
        assert!(backup_dir.path().join("manifest.json").exists());
    }

    #[tokio::test]
    async fn migrate_without_backup() {
        let (source, _d1) = temp_backend();
        let _snap = populated_snapshot(&source).await;

        let (target, _d2) = temp_backend();

        let report = migrate(&source, &target, "sqlite", None).await.unwrap();
        assert!(report.verified);
        assert!(report.backup_path.is_none());
    }
}
