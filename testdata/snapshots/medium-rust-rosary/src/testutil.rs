//! Shared test fixtures for pipeline and dispatch tests.
//!
//! Provides `TestRepo` (temp git repo) and `make_bead()` builder.
//! Consolidates the duplicated git-init patterns from verify.rs and workspace/tests.rs.

use std::path::{Path, PathBuf};

/// A temporary git repository for testing.
pub struct TestRepo {
    pub dir: tempfile::TempDir,
}

impl TestRepo {
    /// Create a new temp git repo with an initial commit.
    pub fn new() -> Self {
        let dir = tempfile::TempDir::new().unwrap();
        let path = dir.path();

        run_git(path, &["init"]);
        run_git(path, &["config", "user.email", "test@test.com"]);
        run_git(path, &["config", "user.name", "Test"]);

        // Initial commit so HEAD exists
        std::fs::write(path.join(".gitkeep"), "").unwrap();
        run_git(path, &["add", "."]);
        run_git(path, &["commit", "-m", "initial"]);

        Self { dir }
    }

    /// Create a commit with a bead reference (passes WorkRefCheck + CommitCheck).
    pub fn commit_with_bead_ref(&self, bead_id: &str, filename: &str, content: &str) {
        let path = self.path();
        // Ensure parent dirs exist
        if let Some(parent) = Path::new(filename).parent()
            && !parent.as_os_str().is_empty()
        {
            std::fs::create_dir_all(path.join(parent)).unwrap();
        }
        std::fs::write(path.join(filename), content).unwrap();
        run_git(path, &["add", "."]);
        let msg = format!("[{bead_id}] fix(test): changes\n\nbead:{bead_id}");
        run_git(path, &["commit", "-m", &msg]);
    }

    /// Create a plain commit without bead reference (fails WorkRefCheck).
    pub fn commit_plain(&self, filename: &str, content: &str) {
        let path = self.path();
        std::fs::write(path.join(filename), content).unwrap();
        run_git(path, &["add", "."]);
        run_git(path, &["commit", "-m", "plain commit"]);
    }

    pub fn path(&self) -> &Path {
        self.dir.path()
    }

    #[allow(dead_code)]
    pub fn pathbuf(&self) -> PathBuf {
        self.dir.path().to_path_buf()
    }

    /// Build a Bead struct pointing at this repo.
    #[allow(dead_code)]
    pub fn make_bead(&self, id: &str, issue_type: &str) -> crate::bead::Bead {
        make_bead(id, issue_type, "test-repo")
    }
}

/// Build a minimal Bead for testing.
pub fn make_bead(id: &str, issue_type: &str, repo: &str) -> crate::bead::Bead {
    crate::bead::Bead {
        id: id.to_string(),
        title: format!("Test bead {id}"),
        description: "Test description".to_string(),
        status: "open".to_string(),
        priority: 2,
        issue_type: issue_type.to_string(),
        owner: None,
        repo: repo.to_string(),
        created_at: chrono::Utc::now(),
        updated_at: chrono::Utc::now(),
        dependency_count: 0,
        dependent_count: 0,
        comment_count: 0,
        branch: None,
        pr_url: None,
        jj_change_id: None,
        external_ref: None,
        files: Vec::new(),
        test_files: Vec::new(),
        created_by: None,
        scope: String::new(),
        derived_from: vec![],
    }
}

fn run_git(dir: &Path, args: &[&str]) {
    let output = std::process::Command::new("git")
        .args(args)
        .current_dir(dir)
        .output()
        .unwrap_or_else(|e| panic!("git {}: {e}", args.join(" ")));
    assert!(
        output.status.success(),
        "git {} failed: {}",
        args.join(" "),
        String::from_utf8_lossy(&output.stderr)
    );
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_repo_has_initial_commit() {
        let repo = TestRepo::new();
        let output = std::process::Command::new("git")
            .args(["log", "--oneline", "-1"])
            .current_dir(repo.path())
            .output()
            .unwrap();
        assert!(output.status.success());
        let log = String::from_utf8_lossy(&output.stdout);
        assert!(log.contains("initial"), "expected initial commit: {log}");
    }

    #[test]
    fn commit_with_bead_ref_passes_log() {
        let repo = TestRepo::new();
        repo.commit_with_bead_ref("rsry-test1", "foo.txt", "content");

        let output = std::process::Command::new("git")
            .args(["log", "--oneline", "-1"])
            .current_dir(repo.path())
            .output()
            .unwrap();
        let log = String::from_utf8_lossy(&output.stdout);
        assert!(log.contains("[rsry-test1]"), "bead ref in log: {log}");
    }

    #[test]
    fn make_bead_has_correct_fields() {
        let bead = make_bead("rsry-abc", "bug", "rosary");
        assert_eq!(bead.id, "rsry-abc");
        assert_eq!(bead.issue_type, "bug");
        assert_eq!(bead.repo, "rosary");
        assert_eq!(bead.status, "open");
    }
}
