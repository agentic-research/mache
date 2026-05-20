use super::*;

#[test]
fn detect_vcs_jj() {
    let tmp = tempfile::TempDir::new().unwrap();
    std::fs::create_dir_all(tmp.path().join(".jj")).unwrap();
    assert_eq!(detect_vcs(tmp.path()), VcsKind::Jj);
}

#[test]
fn detect_vcs_git() {
    let tmp = tempfile::TempDir::new().unwrap();
    std::fs::create_dir_all(tmp.path().join(".git")).unwrap();
    assert_eq!(detect_vcs(tmp.path()), VcsKind::Git);
}

#[test]
fn detect_vcs_colocated_uses_git() {
    let tmp = tempfile::TempDir::new().unwrap();
    std::fs::create_dir_all(tmp.path().join(".jj")).unwrap();
    std::fs::create_dir_all(tmp.path().join(".git")).unwrap();
    // Colocated: git worktree for agents, jj tracks via colocation
    assert_eq!(detect_vcs(tmp.path()), VcsKind::Git);
}

#[test]
fn detect_vcs_none() {
    let tmp = tempfile::TempDir::new().unwrap();
    assert_eq!(detect_vcs(tmp.path()), VcsKind::None);
}

#[tokio::test]
async fn workspace_create_no_isolation() {
    let tmp = tempfile::TempDir::new().unwrap();
    let ws = Workspace::create("test-1", "repo", tmp.path(), false)
        .await
        .unwrap();
    assert_eq!(ws.vcs, VcsKind::None);
    // work_dir should match the input path (expand_path, no symlink resolution)
    assert_eq!(ws.work_dir, tmp.path().to_path_buf());
    assert!(ws.exec_handle.is_none());
}

#[tokio::test]
async fn workspace_create_no_vcs_with_isolate_errors() {
    let tmp = tempfile::TempDir::new().unwrap();
    // No .jj or .git — isolate=true must error, not silently fall back
    let result = Workspace::create("test-1", "repo", tmp.path(), true).await;
    assert!(
        result.is_err(),
        "Workspace::create with isolate=true must fail when no VCS is available, \
         not silently fall back to in-place"
    );
}

#[tokio::test]
async fn workspace_create_no_vcs_without_isolate_falls_through() {
    let tmp = tempfile::TempDir::new().unwrap();
    // No .jj or .git — isolate=false allows in-place execution
    let ws = Workspace::create("test-1", "repo", tmp.path(), false)
        .await
        .unwrap();
    assert_eq!(ws.vcs, VcsKind::None);
    assert_eq!(ws.work_dir, tmp.path().to_path_buf());
}

#[tokio::test]
async fn workspace_provision_and_exec() {
    use crate::backend::tests::MockProvider;

    let tmp = tempfile::TempDir::new().unwrap();
    let mock = MockProvider::new();

    let mut ws = Workspace::create("test-1", "repo", tmp.path(), false)
        .await
        .unwrap();
    ws.provision(&mock).await.unwrap();
    assert!(ws.exec_handle.is_some());

    let result = ws.exec(&mock, &["echo", "hi"]).await.unwrap();
    assert!(result.success());

    let provisions = mock.provisions.lock().unwrap();
    assert_eq!(provisions.len(), 1);
    assert_eq!(provisions[0].bead_id, "test-1");
}

/// Regression (GAP 2): Workspace::cleanup() must not panic for VcsKind::None.
/// cleanup() is called in dispatch::spawn() when spawn_agent fails after workspace
/// creation. If cleanup() is removed or panics, orphaned worktrees accumulate.
#[tokio::test]
async fn workspace_cleanup_noop_is_safe() {
    let tmp = tempfile::TempDir::new().unwrap();
    let ws = Workspace::create("gap2-noop", "repo", tmp.path(), false)
        .await
        .unwrap();
    assert_eq!(ws.vcs, VcsKind::None);
    // Must not panic and must leave the work_dir intact (it's the repo dir itself)
    let work_dir = ws.work_dir.clone();
    ws.cleanup();
    assert!(
        work_dir.exists(),
        "cleanup on VcsKind::None must not remove the repo dir"
    );
}

/// Regression: git worktree must branch from HEAD, not an orphan.
/// Bug: worktree only had the `.beads/` init commit, no source code.
#[tokio::test]
async fn git_worktree_has_source_code_not_just_beads() {
    let tmp = tempfile::TempDir::new().unwrap();
    let repo = tmp.path();

    std::process::Command::new("git")
        .args(["init"])
        .current_dir(repo)
        .output()
        .unwrap();
    std::process::Command::new("git")
        .args(["config", "user.email", "test@test.com"])
        .current_dir(repo)
        .output()
        .unwrap();
    std::process::Command::new("git")
        .args(["config", "user.name", "Test"])
        .current_dir(repo)
        .output()
        .unwrap();
    std::fs::write(repo.join("src.rs"), "fn main() {}").unwrap();
    std::process::Command::new("git")
        .args(["add", "."])
        .current_dir(repo)
        .output()
        .unwrap();
    let commit = std::process::Command::new("git")
        .args(["commit", "-m", "initial"])
        .current_dir(repo)
        .output()
        .unwrap();
    assert!(commit.status.success(), "git commit must succeed");

    // Simulate .beads/ (Dolt creates its own git repo inside)
    std::fs::create_dir_all(repo.join(".beads").join("dolt")).unwrap();

    let wt_path = sweep::create_git_worktree(repo, "test-regression").await;
    assert!(wt_path.is_ok(), "worktree creation should succeed");
    let wt_path = wt_path.unwrap();

    assert!(
        wt_path.join("src.rs").exists(),
        "worktree must contain source files from HEAD, not just .beads/"
    );

    sweep::cleanup_git_worktree(repo, "test-regression");
}

#[tokio::test]
async fn workspace_exec_without_provision_uses_local() {
    let tmp = tempfile::TempDir::new().unwrap();
    let ws = Workspace::create("test-1", "repo", tmp.path(), false)
        .await
        .unwrap();

    // No provision — should fall back to LocalProvider
    let mock = crate::backend::tests::MockProvider::new();
    let result = ws.exec(&mock, &["echo", "fallback"]).await.unwrap();
    // This actually runs locally via LocalProvider, not through mock
    assert!(result.success());
    assert!(result.stdout.contains("fallback"));
}

#[tokio::test]
async fn workspace_teardown_destroys_compute() {
    use crate::backend::tests::MockProvider;

    let tmp = tempfile::TempDir::new().unwrap();
    let mock = MockProvider::new();

    let mut ws = Workspace::create("test-1", "repo", tmp.path(), false)
        .await
        .unwrap();
    ws.provision(&mock).await.unwrap();
    ws.teardown(&mock).await.unwrap();

    let destroys = mock.destroys.lock().unwrap();
    assert_eq!(destroys.len(), 1);
    assert_eq!(destroys[0], "mock-test-1");
}

#[tokio::test]
async fn workspace_teardown_without_provision_ok() {
    let tmp = tempfile::TempDir::new().unwrap();
    let mock = crate::backend::tests::MockProvider::new();

    let ws = Workspace::create("test-1", "repo", tmp.path(), false)
        .await
        .unwrap();
    // Should not error even without provisioning
    ws.teardown(&mock).await.unwrap();

    let destroys = mock.destroys.lock().unwrap();
    assert_eq!(destroys.len(), 0);
}

#[test]
fn vcs_kind_eq() {
    assert_eq!(VcsKind::Jj, VcsKind::Jj);
    assert_ne!(VcsKind::Jj, VcsKind::Git);
    assert_ne!(VcsKind::Git, VcsKind::None);
}

// -----------------------------------------------------------------------
// Helper: create a git+jj colocated repo in a tempdir.
//
// Returns (TempDir, canonical repo path). TempDir must be held alive
// for the lifetime of the test (drop deletes it).
// -----------------------------------------------------------------------
async fn setup_colocated_repo() -> (tempfile::TempDir, std::path::PathBuf) {
    let tmp = tempfile::TempDir::new().unwrap();
    let repo = tmp.path().canonicalize().unwrap();

    // git init
    let out = std::process::Command::new("git")
        .args(["init"])
        .current_dir(&repo)
        .output()
        .unwrap();
    assert!(out.status.success(), "git init failed");

    // Configure git user (needed for commits in CI / clean environments)
    std::process::Command::new("git")
        .args(["config", "user.email", "test@rosary.dev"])
        .current_dir(&repo)
        .output()
        .unwrap();
    std::process::Command::new("git")
        .args(["config", "user.name", "rosary-test"])
        .current_dir(&repo)
        .output()
        .unwrap();

    // Seed a source file so HEAD exists
    std::fs::write(repo.join("lib.rs"), "pub fn hello() {}").unwrap();
    std::process::Command::new("git")
        .args(["add", "."])
        .current_dir(&repo)
        .output()
        .unwrap();
    std::process::Command::new("git")
        .args(["commit", "-m", "initial"])
        .current_dir(&repo)
        .output()
        .unwrap();

    // jj init --colocate (creates .jj/ alongside existing .git/)
    let jj = std::process::Command::new("jj")
        .args(["git", "init", "--colocate"])
        .current_dir(&repo)
        .output()
        .unwrap();
    assert!(
        jj.status.success(),
        "jj git init --colocate failed: {}",
        String::from_utf8_lossy(&jj.stderr)
    );

    // Sanity: both dirs exist
    assert!(repo.join(".git").exists(), ".git must exist");
    assert!(repo.join(".jj").exists(), ".jj must exist");

    (tmp, repo)
}

// -----------------------------------------------------------------------
// Regression test for rosary-a0eb7c / commit 120fd5a:
//
// In old code, detect_vcs() returned Jj for colocated repos, which
// created jj workspaces where git paths resolved wrong (agent git
// add/commit saw parent-relative paths). The fix returns Git for
// colocated repos so git worktree is used instead.
//
// This test exercises the FULL dispatch lifecycle:
//   1. detect_vcs → Git (not Jj) for colocated repo
//   2. Workspace::create → git worktree with proper .git file
//   3. git rev-parse inside worktree → worktree path (not parent)
//   4. git add + commit inside worktree → clean paths (no prefix)
//   5. Workspace::checkpoint → returns a SHA
//   6. cleanup → worktree removed, work visible in main repo log
// -----------------------------------------------------------------------
#[tokio::test]
async fn e2e_colocated_workspace_isolation() {
    // Skip if jj is not installed (CI without jj)
    if std::process::Command::new("jj")
        .arg("--help")
        .output()
        .is_err()
    {
        eprintln!("SKIP: jj not installed");
        return;
    }

    let (_tmp, repo) = setup_colocated_repo().await;
    let bead_id = "e2e-colocated-test";

    // ----- Step 1: detect_vcs returns Git for colocated repos ----------
    assert_eq!(
        detect_vcs(&repo),
        VcsKind::Git,
        "colocated repo (both .jj/ and .git/) must use Git worktree, not Jj"
    );

    // ----- Step 2: Workspace::create produces a git worktree ----------
    let ws = Workspace::create(bead_id, "test-repo", &repo, true)
        .await
        .expect("workspace create must succeed");

    assert_eq!(ws.vcs, VcsKind::Git, "workspace vcs should be Git");
    assert_ne!(
        ws.work_dir, ws.repo_path,
        "worktree dir must differ from repo root"
    );
    assert!(
        ws.work_dir.exists(),
        "worktree directory must exist on disk"
    );

    // The worktree should contain a .git *file* (not directory) pointing
    // back to the parent repo's worktree metadata.
    let dot_git = ws.work_dir.join(".git");
    assert!(dot_git.exists(), "worktree must have a .git file");
    assert!(
        dot_git.is_file(),
        ".git in worktree must be a file (gitdir pointer), not a directory"
    );

    // Source files from HEAD must be present
    assert!(
        ws.work_dir.join("lib.rs").exists(),
        "worktree must contain source files from HEAD"
    );

    // ----- Step 3: git rev-parse --show-toplevel → worktree path ------
    let toplevel = tokio::process::Command::new("git")
        .args(["rev-parse", "--show-toplevel"])
        .current_dir(&ws.work_dir)
        .output()
        .await
        .expect("git rev-parse must succeed");
    assert!(toplevel.status.success());

    let toplevel_path =
        std::path::PathBuf::from(String::from_utf8_lossy(&toplevel.stdout).trim().to_string());
    // Canonicalize both to handle macOS /private/var vs /var symlinks
    assert_eq!(
        toplevel_path.canonicalize().unwrap(),
        ws.work_dir.canonicalize().unwrap(),
        "git rev-parse --show-toplevel must return the WORKTREE path, not the parent repo"
    );

    // ----- Step 4: git add + commit inside worktree → clean paths -----
    let test_file = ws.work_dir.join("agent-output.txt");
    std::fs::write(&test_file, "agent wrote this").unwrap();

    let add = tokio::process::Command::new("git")
        .args(["add", "agent-output.txt"])
        .current_dir(&ws.work_dir)
        .output()
        .await
        .expect("git add must succeed");
    assert!(
        add.status.success(),
        "git add failed: {}",
        String::from_utf8_lossy(&add.stderr)
    );

    let commit = tokio::process::Command::new("git")
        .args(["commit", "-m", "agent: test commit"])
        .current_dir(&ws.work_dir)
        .output()
        .await
        .expect("git commit must succeed");
    assert!(
        commit.status.success(),
        "git commit failed: {}",
        String::from_utf8_lossy(&commit.stderr)
    );

    // Verify committed paths don't have a .rsry-workspaces/ prefix.
    // `git diff-tree` lists paths in the last commit — they should be
    // root-relative within the worktree, not parent-relative.
    let diff_tree = tokio::process::Command::new("git")
        .args(["diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD"])
        .current_dir(&ws.work_dir)
        .output()
        .await
        .expect("git diff-tree must succeed");
    let committed_paths = String::from_utf8_lossy(&diff_tree.stdout);
    assert!(
        committed_paths.contains("agent-output.txt"),
        "committed file must appear in diff-tree"
    );
    assert!(
        !committed_paths.contains(".rsry-workspaces"),
        "committed paths must NOT contain .rsry-workspaces/ prefix — \
         this means git is resolving paths relative to parent, not worktree. \
         Got: {committed_paths}"
    );

    // ----- Step 5: Workspace::checkpoint → returns a SHA ---------------
    // Write another file so checkpoint has something to commit
    std::fs::write(ws.work_dir.join("checkpoint-file.txt"), "checkpoint data").unwrap();

    let sha = ws
        .checkpoint("e2e: checkpoint test")
        .await
        .expect("checkpoint must succeed");
    assert!(
        sha.is_some(),
        "checkpoint must return a SHA when there are dirty files"
    );
    let sha = sha.unwrap();
    assert!(!sha.is_empty(), "checkpoint SHA must be non-empty");

    // Verify the checkpoint commit also has clean paths
    let diff_tree2 = tokio::process::Command::new("git")
        .args(["diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD"])
        .current_dir(&ws.work_dir)
        .output()
        .await
        .unwrap();
    let checkpoint_paths = String::from_utf8_lossy(&diff_tree2.stdout);
    assert!(
        checkpoint_paths.contains("checkpoint-file.txt"),
        "checkpoint commit must include the new file"
    );
    assert!(
        !checkpoint_paths.contains(".rsry-workspaces"),
        "checkpoint paths must not have workspace prefix"
    );

    // ----- Step 6: cleanup → worktree gone, work in main repo log -----
    let worktree_dir = ws.work_dir.clone();

    // Record the branch name to look up in main repo after cleanup
    let branch_out = tokio::process::Command::new("git")
        .args(["rev-parse", "--abbrev-ref", "HEAD"])
        .current_dir(&worktree_dir)
        .output()
        .await
        .unwrap();
    let _branch_name = String::from_utf8_lossy(&branch_out.stdout)
        .trim()
        .to_string();

    // Get the full SHA of the branch tip before cleanup
    let full_sha_out = tokio::process::Command::new("git")
        .args(["rev-parse", "HEAD"])
        .current_dir(&worktree_dir)
        .output()
        .await
        .unwrap();
    let full_sha = String::from_utf8_lossy(&full_sha_out.stdout)
        .trim()
        .to_string();

    // cleanup_git_worktree removes the worktree AND deletes the branch,
    // so we must verify the commit is reachable by SHA before cleanup.
    // But since the branch is force-deleted, the commit becomes
    // unreachable (gc would collect it). Instead, verify the SHA exists
    // in the main repo's object store before cleanup.
    let verify_before = std::process::Command::new("git")
        .args(["cat-file", "-t", &full_sha])
        .current_dir(&repo)
        .output()
        .unwrap();
    assert!(
        verify_before.status.success(),
        "commit SHA must exist in main repo object store before cleanup"
    );
    let obj_type = String::from_utf8_lossy(&verify_before.stdout)
        .trim()
        .to_string();
    assert_eq!(obj_type, "commit", "SHA must point to a commit object");

    // Now clean up
    sweep::cleanup_git_worktree(&repo, bead_id);

    // Worktree directory should be removed
    assert!(
        !worktree_dir.exists(),
        "worktree directory must be removed after cleanup"
    );

    // The commit object still exists in git's object store (it's not
    // garbage collected immediately). Verify it's still there.
    let verify_after = std::process::Command::new("git")
        .args(["cat-file", "-t", &full_sha])
        .current_dir(&repo)
        .output()
        .unwrap();
    assert!(
        verify_after.status.success(),
        "commit object must still exist in main repo after worktree removal \
         (git objects persist until gc)"
    );
}

/// Verify that .beads/ directory from the parent repo is accessible
/// inside a git worktree (it comes from HEAD, so if .beads/ is
/// committed or if it's an untracked dir, agents can still reach Dolt).
///
/// This test creates a .beads/ marker in the repo and verifies the
/// worktree can see it. Actual Dolt connectivity requires a running
/// Dolt server, so this test only checks file-level accessibility.
#[tokio::test]
#[ignore] // requires jj installed; run with `cargo test -- --ignored`
async fn e2e_colocated_worktree_beads_accessible() {
    if std::process::Command::new("jj")
        .arg("--help")
        .output()
        .is_err()
    {
        eprintln!("SKIP: jj not installed");
        return;
    }

    let (_tmp, repo) = setup_colocated_repo().await;

    // Create and commit a .beads/ marker file (simulates Dolt init)
    std::fs::create_dir_all(repo.join(".beads")).unwrap();
    std::fs::write(repo.join(".beads").join("marker"), "dolt-placeholder").unwrap();
    std::process::Command::new("git")
        .args(["add", ".beads/"])
        .current_dir(&repo)
        .output()
        .unwrap();
    std::process::Command::new("git")
        .args(["commit", "-m", "add .beads marker"])
        .current_dir(&repo)
        .output()
        .unwrap();

    let ws = Workspace::create("beads-access-test", "test-repo", &repo, true)
        .await
        .expect("workspace create must succeed");

    assert_eq!(ws.vcs, VcsKind::Git);

    // .beads/ should be present in the worktree (branched from HEAD)
    assert!(
        ws.work_dir.join(".beads").join("marker").exists(),
        ".beads/ must be accessible in the git worktree — \
         agents need this to reach Dolt"
    );

    sweep::cleanup_git_worktree(&repo, "beads-access-test");
}

/// Concurrent multi-agent isolation: two worktrees from the same repo
/// must not cross-contaminate. Each agent writes a different file;
/// neither file appears in the other worktree or in main.
#[tokio::test]
async fn concurrent_worktree_isolation() {
    if std::process::Command::new("jj")
        .arg("--help")
        .output()
        .is_err()
    {
        eprintln!("SKIP: jj not installed");
        return;
    }

    let (_tmp, repo) = setup_colocated_repo().await;

    // Create two worktrees concurrently
    let ws_a = Workspace::create("agent-alpha", "test-repo", &repo, true)
        .await
        .expect("workspace A must succeed");
    let ws_b = Workspace::create("agent-beta", "test-repo", &repo, true)
        .await
        .expect("workspace B must succeed");

    assert_ne!(ws_a.work_dir, ws_b.work_dir, "worktrees must be distinct");
    assert_ne!(ws_a.work_dir, repo, "worktree A must differ from main");
    assert_ne!(ws_b.work_dir, repo, "worktree B must differ from main");

    // Each "agent" writes a unique file
    std::fs::write(ws_a.work_dir.join("alpha.txt"), "alpha output").unwrap();
    std::fs::write(ws_b.work_dir.join("beta.txt"), "beta output").unwrap();

    // Commit in each worktree
    for (label, ws) in [("alpha", &ws_a), ("beta", &ws_b)] {
        let add = std::process::Command::new("git")
            .args(["add", "-A"])
            .current_dir(&ws.work_dir)
            .output()
            .unwrap();
        assert!(add.status.success(), "{label} git add failed");

        let commit = std::process::Command::new("git")
            .args(["commit", "-m", &format!("{label} work")])
            .current_dir(&ws.work_dir)
            .output()
            .unwrap();
        assert!(commit.status.success(), "{label} git commit failed");
    }

    // Verify isolation: alpha.txt must NOT exist in beta or main
    assert!(
        ws_a.work_dir.join("alpha.txt").exists(),
        "alpha.txt must exist in worktree A"
    );
    assert!(
        !ws_b.work_dir.join("alpha.txt").exists(),
        "alpha.txt must NOT leak into worktree B"
    );
    assert!(
        !repo.join("alpha.txt").exists(),
        "alpha.txt must NOT leak into main repo"
    );

    // Verify isolation: beta.txt must NOT exist in alpha or main
    assert!(
        ws_b.work_dir.join("beta.txt").exists(),
        "beta.txt must exist in worktree B"
    );
    assert!(
        !ws_a.work_dir.join("beta.txt").exists(),
        "beta.txt must NOT leak into worktree A"
    );
    assert!(
        !repo.join("beta.txt").exists(),
        "beta.txt must NOT leak into main repo"
    );

    // Verify each worktree's git log only has its own commit
    let log_a = std::process::Command::new("git")
        .args(["log", "--oneline", "-1", "--format=%s"])
        .current_dir(&ws_a.work_dir)
        .output()
        .unwrap();
    let msg_a = String::from_utf8_lossy(&log_a.stdout).trim().to_string();
    assert_eq!(
        msg_a, "alpha work",
        "worktree A HEAD must be alpha's commit"
    );

    let log_b = std::process::Command::new("git")
        .args(["log", "--oneline", "-1", "--format=%s"])
        .current_dir(&ws_b.work_dir)
        .output()
        .unwrap();
    let msg_b = String::from_utf8_lossy(&log_b.stdout).trim().to_string();
    assert_eq!(msg_b, "beta work", "worktree B HEAD must be beta's commit");

    // Verify main's git status is clean — no unstaged diffs from worktree ops.
    // Regression: worktree isolation leak caused agent changes to appear as
    // unstaged diffs in main, blocking ff-merge of other agents' work.
    let main_status = std::process::Command::new("git")
        .args(["status", "--porcelain"])
        .current_dir(&repo)
        .output()
        .unwrap();
    let main_status_str = String::from_utf8_lossy(&main_status.stdout)
        .trim()
        .to_string();
    assert!(
        main_status_str.is_empty(),
        "main repo git status must be clean after worktree operations, got: {main_status_str}"
    );

    // Cleanup
    sweep::cleanup_git_worktree(&repo, "agent-alpha");
    sweep::cleanup_git_worktree(&repo, "agent-beta");
}

/// Regression: create_git_worktree must handle an existing branch
/// from a previous failed dispatch by cleaning up and retrying.
#[tokio::test]
async fn git_worktree_retries_on_existing_branch() {
    let tmp = tempfile::TempDir::new().unwrap();
    let repo = tmp.path().canonicalize().unwrap();

    // Set up git repo
    std::process::Command::new("git")
        .args(["init"])
        .current_dir(&repo)
        .output()
        .unwrap();
    std::process::Command::new("git")
        .args(["config", "user.email", "test@rosary.dev"])
        .current_dir(&repo)
        .output()
        .unwrap();
    std::process::Command::new("git")
        .args(["config", "user.name", "rosary-test"])
        .current_dir(&repo)
        .output()
        .unwrap();
    std::fs::write(repo.join("main.rs"), "fn main() {}").unwrap();
    std::process::Command::new("git")
        .args(["add", "."])
        .current_dir(&repo)
        .output()
        .unwrap();
    std::process::Command::new("git")
        .args(["commit", "-m", "initial"])
        .current_dir(&repo)
        .output()
        .unwrap();

    // Create a stale branch (simulates a previous failed dispatch)
    std::process::Command::new("git")
        .args(["branch", "fix/stale-bead"])
        .current_dir(&repo)
        .output()
        .unwrap();

    // Now try to create a worktree for the same bead ID — should succeed
    // by cleaning up the stale branch
    let result = sweep::create_git_worktree(&repo, "stale-bead").await;
    assert!(
        result.is_ok(),
        "create_git_worktree must retry after cleaning stale branch, got: {:?}",
        result.err()
    );

    let wt_path = result.unwrap();
    assert!(wt_path.exists(), "worktree directory must exist");
    assert!(
        wt_path.join("main.rs").exists(),
        "worktree must contain source files"
    );

    sweep::cleanup_git_worktree(&repo, "stale-bead");
}

/// When isolate=true and VCS setup fails, Workspace::create must
/// return an error instead of silently falling back to in-place.
#[tokio::test]
async fn workspace_create_isolate_true_no_silent_fallback() {
    let tmp = tempfile::TempDir::new().unwrap();
    let repo = tmp.path().canonicalize().unwrap();

    // Create .git dir so detect_vcs returns Git, but don't init git
    // so git worktree add will fail
    std::fs::create_dir(repo.join(".git")).unwrap();

    let result = Workspace::create("test-no-fallback", "repo", &repo, true).await;
    assert!(
        result.is_err(),
        "Workspace::create with isolate=true must fail when VCS setup fails, \
         not silently fall back to in-place"
    );
}

/// E2E pipeline integration: exercises the full dispatch lifecycle
/// across two pipeline phases (dev-agent → staging-agent) in a single
/// worktree. Tests workspace reuse, handoff writing, checkpoint, and
/// merge_or_pr.
///
/// No Dolt, no real Claude — just the workspace + handoff + merge mechanics.
#[tokio::test]
async fn e2e_pipeline_two_phase_lifecycle() {
    if std::process::Command::new("jj")
        .arg("--help")
        .output()
        .is_err()
    {
        eprintln!("SKIP: jj not installed");
        return;
    }

    let (_tmp, repo) = setup_colocated_repo().await;
    let bead_id = "pipeline-e2e-test";

    // === Phase 1: dev-agent ===
    let ws = Workspace::create(bead_id, "test-repo", &repo, true)
        .await
        .expect("phase 1 workspace create");
    assert_eq!(ws.vcs, VcsKind::Git);

    // Stub agent work: write a file and commit
    std::fs::write(ws.work_dir.join("fix.rs"), "fn fix() { /* dev-agent */ }").unwrap();
    let commit = std::process::Command::new("git")
        .args(["add", "-A"])
        .current_dir(&ws.work_dir)
        .output()
        .unwrap();
    assert!(commit.status.success());
    let commit = std::process::Command::new("git")
        .args(["commit", "-m", &format!("bead:{bead_id} dev-agent fix")])
        .current_dir(&ws.work_dir)
        .output()
        .unwrap();
    assert!(commit.status.success());

    // Checkpoint (orchestrator does this after agent exits)
    let sha1 = ws
        .checkpoint("fix(pipeline-e2e-test): dev-agent work")
        .await
        .expect("phase 1 checkpoint");
    // Checkpoint may return None if nothing new to commit (agent already committed)
    // That's fine — the agent's commit is what matters

    // Write handoff for phase 1
    let work1 = crate::manifest::Work {
        commits: vec![crate::manifest::CommitInfo {
            sha: sha1.clone().unwrap_or_else(|| "agent-sha".to_string()),
            message: format!("bead:{bead_id} dev-agent fix"),
            author: "dev-agent".to_string(),
        }],
        files_changed: vec!["fix.rs".to_string()],
        lines_added: 1,
        lines_removed: 0,
        diff_stat: None,
    };
    let handoff1 = crate::handoff::Handoff::new(
        0,
        "dev-agent",
        Some("staging-agent"),
        bead_id,
        "test",
        &work1,
        None,
    );
    let handoff_path = handoff1.write_to(&ws.work_dir).expect("write handoff 1");
    assert!(handoff_path.exists(), "handoff file must exist");

    // === Phase 2: staging-agent (reuse same workspace) ===
    // The reconciler reopens the bead with the new owner and dispatches again.
    // Workspace::create should reuse the existing worktree.
    let ws2 = Workspace::create(bead_id, "test-repo", &repo, true)
        .await
        .expect("phase 2 workspace create (reuse)");

    assert_eq!(
        ws.work_dir, ws2.work_dir,
        "workspace must be REUSED across pipeline phases"
    );

    // The previous agent's files must be present
    assert!(
        ws2.work_dir.join("fix.rs").exists(),
        "dev-agent's fix.rs must persist into phase 2"
    );

    // Handoff chain must be readable by the next agent
    let chain = crate::handoff::Handoff::read_chain(&ws2.work_dir);
    assert_eq!(chain.len(), 1, "handoff chain must have phase 0");
    assert_eq!(chain[0].from_agent, "dev-agent");
    assert_eq!(chain[0].to_agent.as_deref(), Some("staging-agent"));

    // Staging-agent work: add a test file
    std::fs::write(
        ws2.work_dir.join("fix_test.rs"),
        "#[test] fn test_fix() { fix(); }",
    )
    .unwrap();
    let commit = std::process::Command::new("git")
        .args(["add", "-A"])
        .current_dir(&ws2.work_dir)
        .output()
        .unwrap();
    assert!(commit.status.success());
    let commit = std::process::Command::new("git")
        .args([
            "commit",
            "-m",
            &format!("bead:{bead_id} staging-agent review"),
        ])
        .current_dir(&ws2.work_dir)
        .output()
        .unwrap();
    assert!(commit.status.success());

    // Checkpoint phase 2
    let _sha2 = ws2
        .checkpoint("fix(pipeline-e2e-test): staging-agent review")
        .await
        .expect("phase 2 checkpoint");

    // === Terminal step: merge to main ===
    let branch = format!("fix/{bead_id}");
    let merge_result = sweep::merge_or_pr(&repo, &branch, bead_id, "bug").await;
    assert!(
        merge_result.is_ok(),
        "merge_or_pr must succeed for bug type, got: {:?}",
        merge_result.err()
    );
    let result = merge_result.unwrap();
    // In test repos without a remote, push fails gracefully — just check it ran
    assert!(
        !result.message.is_empty(),
        "merge_or_pr must return a message"
    );

    // Verify: both files are now in main
    assert!(
        repo.join("fix.rs").exists(),
        "dev-agent's fix.rs must be in main after merge"
    );
    assert!(
        repo.join("fix_test.rs").exists(),
        "staging-agent's fix_test.rs must be in main after merge"
    );

    // Verify: main's git log has both commits
    let log = std::process::Command::new("git")
        .args(["log", "--oneline", "--format=%s"])
        .current_dir(&repo)
        .output()
        .unwrap();
    let log_output = String::from_utf8_lossy(&log.stdout);
    assert!(
        log_output.contains("staging-agent review"),
        "main log must include staging-agent commit"
    );
    assert!(
        log_output.contains("dev-agent fix"),
        "main log must include dev-agent commit"
    );

    sweep::cleanup_git_worktree(&repo, bead_id);
}

#[test]
fn parse_owner_repo_ssh() {
    let (owner, repo) =
        sweep::parse_owner_repo("git@github.com:agentic-research/rosary.git").unwrap();
    assert_eq!(owner, "agentic-research");
    assert_eq!(repo, "rosary");
}

#[test]
fn parse_owner_repo_https() {
    let (owner, repo) =
        sweep::parse_owner_repo("https://github.com/agentic-research/rosary.git").unwrap();
    assert_eq!(owner, "agentic-research");
    assert_eq!(repo, "rosary");
}

#[test]
fn parse_owner_repo_https_no_git_suffix() {
    let (owner, repo) =
        sweep::parse_owner_repo("https://github.com/agentic-research/rosary").unwrap();
    assert_eq!(owner, "agentic-research");
    assert_eq!(repo, "rosary");
}

#[test]
fn parse_owner_repo_invalid() {
    assert!(sweep::parse_owner_repo("https://gitlab.com/foo/bar").is_err());
}

#[test]
fn thread_branch_name_simple() {
    assert_eq!(
        sweep::thread_branch_name("rosary", "unified-query"),
        "rosary/unified-query"
    );
}

#[test]
fn thread_branch_name_slugifies() {
    assert_eq!(
        sweep::thread_branch_name("rosary", "GitHub App Auth"),
        "rosary/github-app-auth"
    );
}

#[test]
fn thread_branch_name_custom_prefix() {
    assert_eq!(
        sweep::thread_branch_name("agent", "core/pipeline"),
        "agent/core-pipeline"
    );
}
