//! CLI integration tests for `rsry`.
//!
//! Exercises the rsry binary end-to-end against a sandboxed Dolt instance.
//! Tests the full user journey: help, bead CRUD, export/import, status, error cases.
//!
//! Requires: `dolt` binary in PATH. Tests are skipped if dolt is unavailable.
//! Run with: `cargo test --test cli_integration`

use std::path::{Path, PathBuf};
use std::process::{Command, Output};

use tempfile::TempDir;

// ---------------------------------------------------------------------------
// Test sandbox — fresh temp repo + dolt per test
// ---------------------------------------------------------------------------

struct CliSandbox {
    /// The temp directory containing the fake repo + .beads/
    repo_dir: TempDir,
    /// Isolated HOME directory so enable/disable don't touch real ~/.rsry/
    home_dir: TempDir,
    /// Path to the rsry binary
    rsry: PathBuf,
}

impl CliSandbox {
    /// Create a sandboxed repo with Dolt beads DB, ready for CLI tests.
    /// Uses `rsry enable` to init everything (dolt, schema, server auto-start).
    /// Returns None if dolt is not installed (test skipped).
    fn new() -> Option<Self> {
        let dolt_status = Command::new("dolt")
            .arg("version")
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::null())
            .status();
        match dolt_status {
            Ok(status) if !status.success() => {
                eprintln!("skipping: dolt installed but `dolt version` failed");
                return None;
            }
            Err(_) => {
                eprintln!("skipping: dolt not installed");
                return None;
            }
            _ => {}
        }

        let rsry = PathBuf::from(env!("CARGO_BIN_EXE_rsry"));
        if !rsry.exists() {
            eprintln!("skipping: rsry binary not found at {}", rsry.display());
            eprintln!("hint: run `cargo build` first");
            return None;
        }

        let repo_dir = TempDir::new().expect("create temp dir");
        let home_dir = TempDir::new().expect("create temp HOME");
        let repo = repo_dir.path();

        // Dolt needs ~/.dolt/ for global config. Initialize it in the isolated HOME.
        let dolt_home = home_dir.path().join(".dolt");
        std::fs::create_dir_all(&dolt_home).unwrap();
        // Run dolt config to init the global config so dolt init works
        let _ = Command::new("dolt")
            .args(["config", "--global", "--add", "user.name", "Test"])
            .env("HOME", home_dir.path())
            .output();
        let _ = Command::new("dolt")
            .args(["config", "--global", "--add", "user.email", "test@test.com"])
            .env("HOME", home_dir.path())
            .output();

        // Initialize a git repo (rsry resolves repo name from dir name)
        run_cmd("git", &["init"], repo);
        run_cmd("git", &["config", "user.email", "test@test.com"], repo);
        run_cmd("git", &["config", "user.name", "Test"], repo);
        std::fs::write(repo.join(".gitkeep"), "").unwrap();
        run_cmd("git", &["add", "."], repo);
        run_cmd("git", &["commit", "-m", "initial"], repo);

        let sandbox = CliSandbox {
            repo_dir,
            home_dir,
            rsry,
        };

        // Use `rsry enable` to init .beads/ (dolt init + schema + auto-start server)
        let out = sandbox.run(&["enable", "."]);
        let stdout = String::from_utf8_lossy(&out.stdout);
        let stderr = String::from_utf8_lossy(&out.stderr);
        assert!(
            out.status.success(),
            "rsry enable failed:\nstdout: {stdout}\nstderr: {stderr}"
        );

        Some(sandbox)
    }

    fn repo_path(&self) -> &Path {
        self.repo_dir.path()
    }

    /// Run rsry with args, in the sandbox repo directory.
    /// HOME is set to an isolated temp dir so enable/disable don't touch ~/.rsry/.
    fn run(&self, args: &[&str]) -> Output {
        let mut cmd = Command::new(&self.rsry);
        cmd.args(args)
            .current_dir(self.repo_path())
            .env("NO_COLOR", "1")
            .env("HOME", self.home_dir.path());

        if cfg!(target_os = "macos") {
            cmd.env(
                "DYLD_LIBRARY_PATH",
                format!(
                    "/usr/local/lib:{}",
                    std::env::var("DYLD_LIBRARY_PATH").unwrap_or_default()
                ),
            );
        }

        cmd.output().expect("run rsry")
    }

    /// Run rsry, assert success, return stdout (ANSI-stripped).
    fn run_ok(&self, args: &[&str]) -> String {
        let out = self.run(args);
        let stdout = String::from_utf8_lossy(&out.stdout).to_string();
        let stderr = String::from_utf8_lossy(&out.stderr).to_string();
        assert!(
            out.status.success(),
            "rsry {} failed (exit {:?}):\nstdout: {stdout}\nstderr: {stderr}",
            args.join(" "),
            out.status.code(),
        );
        strip_ansi(&stdout)
    }

    /// Run rsry, assert failure, return combined output (ANSI-stripped).
    fn run_err(&self, args: &[&str]) -> String {
        let out = self.run(args);
        assert!(
            !out.status.success(),
            "rsry {} should have failed but succeeded:\nstdout: {}",
            args.join(" "),
            String::from_utf8_lossy(&out.stdout),
        );
        strip_ansi(&format!(
            "{}{}",
            String::from_utf8_lossy(&out.stderr),
            String::from_utf8_lossy(&out.stdout),
        ))
    }

    /// Create a bead and return its ID.
    fn create_bead(&self, title: &str) -> String {
        let stdout = self.run_ok(&["bead", "create", title, "-f", "src/main.rs"]);
        extract_bead_id(&stdout)
            .unwrap_or_else(|| panic!("could not extract bead ID from: {stdout}"))
    }
}

impl Drop for CliSandbox {
    fn drop(&mut self) {
        // Kill the auto-started dolt server, checking liveness before each signal
        // to avoid hitting a reused PID.
        let pid_file = self.repo_path().join(".beads").join("dolt-server.pid");
        if let Ok(pid_str) = std::fs::read_to_string(&pid_file)
            && let Ok(pid) = pid_str.trim().parse::<i32>()
        {
            unsafe {
                if libc::kill(pid, 0) == 0 {
                    libc::kill(pid, libc::SIGTERM);
                    std::thread::sleep(std::time::Duration::from_millis(100));
                    if libc::kill(pid, 0) == 0 {
                        libc::kill(pid, libc::SIGKILL);
                    }
                }
            }
        }
        // No need to unregister — HOME is isolated, global config is in temp dir.
    }
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

fn run_cmd(cmd: &str, args: &[&str], dir: &Path) {
    let output = Command::new(cmd)
        .args(args)
        .current_dir(dir)
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .output()
        .unwrap_or_else(|e| panic!("{cmd} {}: {e}", args.join(" ")));
    assert!(
        output.status.success(),
        "{cmd} {} failed: {}",
        args.join(" "),
        String::from_utf8_lossy(&output.stderr)
    );
}

fn strip_ansi(s: &str) -> String {
    // Remove ANSI escape sequences: ESC [ ... m
    let mut result = String::with_capacity(s.len());
    let mut in_escape = false;
    for c in s.chars() {
        if c == '\x1b' {
            in_escape = true;
            continue;
        }
        if in_escape {
            if c == 'm' {
                in_escape = false;
            }
            continue;
        }
        result.push(c);
    }
    result
}

fn extract_bead_id(output: &str) -> Option<String> {
    let clean = strip_ansi(output);
    // Bead IDs match pattern: {prefix}-{6hex} (e.g., ".tmpXXXXXX-a1b2c3")
    for word in clean.split_whitespace() {
        if word.contains('-')
            && word
                .rsplit('-')
                .next()
                .is_some_and(|s| s.len() == 6 && s.chars().all(|c| c.is_ascii_hexdigit()))
        {
            return Some(word.to_string());
        }
    }
    None
}

// ---------------------------------------------------------------------------
// Tests — Phase 0: Binary basics
// ---------------------------------------------------------------------------

#[test]
fn help_succeeds() {
    let sandbox = match CliSandbox::new() {
        Some(s) => s,
        None => return,
    };
    sandbox.run_ok(&["--help"]);
}

#[test]
fn bead_help_succeeds() {
    let sandbox = match CliSandbox::new() {
        Some(s) => s,
        None => return,
    };
    sandbox.run_ok(&["bead", "--help"]);
}

#[test]
fn version_succeeds() {
    let sandbox = match CliSandbox::new() {
        Some(s) => s,
        None => return,
    };
    let stdout = sandbox.run_ok(&["--version"]);
    assert!(
        stdout.contains("rsry"),
        "version should contain 'rsry': {stdout}"
    );
}

// ---------------------------------------------------------------------------
// Tests — Phase 1: Bead CRUD lifecycle
// ---------------------------------------------------------------------------

#[test]
fn bead_create_and_list() {
    let sandbox = match CliSandbox::new() {
        Some(s) => s,
        None => return,
    };

    let stdout = sandbox.run_ok(&["bead", "create", "Test bead alpha", "-f", "src/main.rs"]);
    assert!(
        stdout.contains("created"),
        "create output should confirm creation: {stdout}"
    );
    let id = extract_bead_id(&stdout).expect("should extract bead ID");

    let stdout = sandbox.run_ok(&["bead", "list"]);
    assert!(
        stdout.contains("Test bead alpha"),
        "list should show bead: {stdout}"
    );
    assert!(
        stdout.contains(&id),
        "list should show bead ID {id}: {stdout}"
    );
}

#[test]
fn bead_search() {
    let sandbox = match CliSandbox::new() {
        Some(s) => s,
        None => return,
    };

    sandbox.create_bead("Widget regression");
    sandbox.create_bead("Unrelated task");

    let stdout = sandbox.run_ok(&["bead", "search", "Widget"]);
    assert!(
        stdout.contains("Widget regression"),
        "search should find match: {stdout}"
    );
}

#[test]
fn bead_close() {
    let sandbox = match CliSandbox::new() {
        Some(s) => s,
        None => return,
    };

    let id = sandbox.create_bead("Close me");
    let stdout = sandbox.run_ok(&["bead", "close", &id, "--force"]);
    assert!(
        stdout.contains("closed"),
        "close output should confirm: {stdout}"
    );

    // Without --force, an impl bead with no test command should be refused.
    let id2 = sandbox.create_bead("Block me");
    let out = sandbox.run(&["bead", "close", &id2]);
    let stderr = String::from_utf8_lossy(&out.stderr);
    assert!(
        !out.status.success(),
        "close without --force should fail when description has no test command: stderr={stderr}"
    );
    assert!(
        stderr.contains("verifiable test command"),
        "expected refusal message, got: {stderr}"
    );
}

#[test]
fn bead_comment() {
    let sandbox = match CliSandbox::new() {
        Some(s) => s,
        None => return,
    };

    let id = sandbox.create_bead("Comment target");
    let stdout = sandbox.run_ok(&["bead", "comment", "add", &id, "progress update"]);
    assert!(
        stdout.contains("commented"),
        "comment should confirm: {stdout}"
    );
}

/// rosary-a96b06: end-to-end CLI exercise of the comment-edit primitives.
/// add → list (find comment_id) → update (with reason) → list (verify edited)
/// → delete --reason (soft) → list --include-deleted (verify still present).
#[test]
fn bead_comment_update_delete_audit_trail() {
    let sandbox = match CliSandbox::new() {
        Some(s) => s,
        None => return,
    };

    let id = sandbox.create_bead("Audit target");

    // Add the original (deliberately leaky) comment.
    sandbox.run_ok(&["bead", "comment", "add", &id, "see /Users/alice/path"]);

    // List to find the comment_id.
    let listed = sandbox.run_ok(&["bead", "comment", "list", &id]);
    assert!(
        listed.contains("/Users/alice/path"),
        "list must show the original body: {listed}"
    );
    // First listed line has `#<id>` — extract the opaque id (UUID for Dolt,
    // stringified integer for SQLite — both valid stable comment_ids).
    let cid: String = listed
        .lines()
        .find_map(|ln| {
            ln.trim()
                .strip_prefix('#')
                .and_then(|s| s.split_whitespace().next().map(|tok| tok.to_string()))
        })
        .expect("could not parse comment_id from list output");
    assert!(!cid.is_empty(), "comment_id must be non-empty");

    // Update with a reason — first edit must capture original_text.
    let updated = sandbox.run_ok(&[
        "bead",
        "comment",
        "update",
        &id,
        &cid,
        "--body",
        "see <home>/path",
        "--reason",
        "scrub absolute path",
    ]);
    assert!(
        updated.contains("updated comment"),
        "update should confirm: {updated}"
    );

    // Verify the edit landed and the audit trail records the original.
    let after_edit = sandbox.run_ok(&["bead", "comment", "list", &id]);
    assert!(after_edit.contains("see <home>/path"), "{after_edit}");
    assert!(after_edit.contains("(edited)"), "{after_edit}");
    assert!(
        after_edit.contains("original: see /Users/alice/path"),
        "list should surface original body: {after_edit}"
    );
    assert!(
        after_edit.contains("scrub absolute path"),
        "list should surface edit reason: {after_edit}"
    );

    // Soft-delete with another reason.
    let deleted = sandbox.run_ok(&[
        "bead",
        "comment",
        "delete",
        &id,
        &cid,
        "--reason",
        "no longer relevant",
    ]);
    assert!(
        deleted.contains("soft-deleted"),
        "delete should confirm: {deleted}"
    );

    // Default list hides it.
    let list_default = sandbox.run_ok(&["bead", "comment", "list", &id]);
    assert!(
        list_default.contains("(no comments on") || !list_default.contains("(edited)"),
        "soft-deleted comment must not appear in default list: {list_default}"
    );

    // --include-deleted brings it back with both reasons preserved
    // (rosary-a96b06: edit_reason and delete_reason live in separate
    // columns so the deletion reason survives the prior edit).
    let list_all = sandbox.run_ok(&["bead", "comment", "list", &id, "--include-deleted"]);
    assert!(list_all.contains("(deleted)"), "{list_all}");
    assert!(
        list_all.contains("edit reason: scrub absolute path"),
        "edit reason must remain in audit trail: {list_all}"
    );
    assert!(
        list_all.contains("delete reason: no longer relevant"),
        "delete reason must be recorded in its own column: {list_all}"
    );
}

#[test]
fn bead_create_with_priority_and_type() {
    let sandbox = match CliSandbox::new() {
        Some(s) => s,
        None => return,
    };

    let stdout = sandbox.run_ok(&[
        "bead",
        "create",
        "Critical bug",
        "-p",
        "0",
        "-t",
        "bug",
        "-f",
        "src/main.rs",
    ]);
    assert!(stdout.contains("created"));
}

// ---------------------------------------------------------------------------
// Tests — Phase 2: Export / Import round-trip
// ---------------------------------------------------------------------------

#[test]
fn bead_export_produces_valid_json() {
    let sandbox = match CliSandbox::new() {
        Some(s) => s,
        None => return,
    };

    sandbox.create_bead("Export test bead");

    let stdout = sandbox.run_ok(&["bead", "export", "--status", "open"]);
    let parsed: serde_json::Value =
        serde_json::from_str(&stdout).expect("export should produce valid JSON");
    assert!(parsed.is_array(), "export should be a JSON array");
    let arr = parsed.as_array().unwrap();
    assert!(!arr.is_empty(), "export should contain at least 1 bead");

    let bead = &arr[0];
    assert!(bead["title"].is_string());
    assert!(bead["priority"].is_number());
    assert!(bead["issue_type"].is_string());
    assert!(bead["repo"].is_string(), "export should include repo field");
}

#[test]
fn bead_import_dedup() {
    let sandbox = match CliSandbox::new() {
        Some(s) => s,
        None => return,
    };

    sandbox.create_bead("Round trip bead");

    // Export then re-import — should skip duplicate
    let exported = sandbox.run_ok(&["bead", "export", "--status", "open"]);
    let export_path = sandbox.repo_path().join("export.json");
    std::fs::write(&export_path, &exported).unwrap();

    let stdout = sandbox.run_ok(&["bead", "import", export_path.to_str().unwrap()]);
    assert!(
        stdout.contains("skipped 1"),
        "re-import should skip duplicate: {stdout}"
    );
}

#[test]
fn bead_import_new_bead() {
    let sandbox = match CliSandbox::new() {
        Some(s) => s,
        None => return,
    };

    let import_json = r#"[{
        "title": "Imported bead",
        "description": "From JSON",
        "priority": 1,
        "issue_type": "task",
        "files": ["src/lib.rs"],
        "test_files": []
    }]"#;
    let import_path = sandbox.repo_path().join("import.json");
    std::fs::write(&import_path, import_json).unwrap();

    let stdout = sandbox.run_ok(&["bead", "import", import_path.to_str().unwrap()]);
    assert!(
        stdout.contains("Imported 1"),
        "import should create 1 bead: {stdout}"
    );

    let stdout = sandbox.run_ok(&["bead", "search", "Imported bead"]);
    assert!(
        stdout.contains("Imported bead"),
        "imported bead should be searchable: {stdout}"
    );
}

// ---------------------------------------------------------------------------
// Tests — Phase 3: Status
// ---------------------------------------------------------------------------

#[test]
fn status_json_output() {
    let sandbox = match CliSandbox::new() {
        Some(s) => s,
        None => return,
    };

    // Repo is already registered from CliSandbox::new() enable call (isolated HOME)
    sandbox.create_bead("Status test");

    let stdout = sandbox.run_ok(&["status", "--json"]);
    let parsed: serde_json::Value =
        serde_json::from_str(&stdout).expect("status --json should produce valid JSON");
    assert!(
        parsed["total"].as_u64().unwrap_or(0) >= 1,
        "should have beads: {parsed}"
    );
    assert!(
        parsed["open"].is_number(),
        "should have 'open' field: {parsed}"
    );
}

// ---------------------------------------------------------------------------
// Tests — Phase 4: Error cases
// ---------------------------------------------------------------------------

#[test]
fn bead_close_nonexistent_does_not_panic() {
    let sandbox = match CliSandbox::new() {
        Some(s) => s,
        None => return,
    };

    let out = sandbox.run(&["bead", "close", "nonexistent-000000"]);
    let combined = format!(
        "{}{}",
        String::from_utf8_lossy(&out.stdout),
        String::from_utf8_lossy(&out.stderr),
    );
    assert!(
        !combined.contains("panic"),
        "should not panic on nonexistent ID: {combined}"
    );
}

#[test]
fn bead_create_missing_title() {
    let sandbox = match CliSandbox::new() {
        Some(s) => s,
        None => return,
    };

    let out = sandbox.run(&["bead", "create"]);
    assert!(!out.status.success(), "create without title should fail");
}

#[test]
fn bead_create_task_requires_files() {
    let sandbox = match CliSandbox::new() {
        Some(s) => s,
        None => return,
    };

    let output = sandbox.run_err(&["bead", "create", "Missing files task"]);
    assert!(
        output.contains("files required"),
        "should require files for task type: {output}"
    );
}

#[test]
fn bead_create_epic_skips_file_requirement() {
    let sandbox = match CliSandbox::new() {
        Some(s) => s,
        None => return,
    };

    let stdout = sandbox.run_ok(&["bead", "create", "Planning epic", "-t", "epic"]);
    assert!(
        stdout.contains("created"),
        "epic without files should succeed: {stdout}"
    );
}

// ---------------------------------------------------------------------------
// Tests — Phase 5: Full lifecycle
// ---------------------------------------------------------------------------

#[test]
fn full_bead_lifecycle() {
    let sandbox = match CliSandbox::new() {
        Some(s) => s,
        None => return,
    };

    // Create
    let id = sandbox.create_bead("Lifecycle test bead");

    // Comment
    sandbox.run_ok(&["bead", "comment", "add", &id, "Starting work"]);
    sandbox.run_ok(&["bead", "comment", "add", &id, "Progress update"]);

    // Search
    let stdout = sandbox.run_ok(&["bead", "search", "Lifecycle"]);
    assert!(stdout.contains("Lifecycle test bead"));

    // List (open beads)
    let stdout = sandbox.run_ok(&["bead", "list"]);
    assert!(stdout.contains("Lifecycle test bead"));

    // Close
    sandbox.run_ok(&["bead", "close", &id, "--force"]);

    // Verify closed — list shows open beads only, so it should be gone
    let stdout = sandbox.run_ok(&["bead", "list"]);
    assert!(
        !stdout.contains("Lifecycle test bead"),
        "closed bead should not appear in list: {stdout}"
    );
}
