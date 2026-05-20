//! Encrypted notes with `age` and recipient rotation.
//!
//! Notes live under `notes/<scope>/`. The recipient list for a scope is
//! `notes/<scope>/.recipients` — newline-separated age recipient strings
//! (SSH public keys or `age1...` recipients).
//!
//! `rotate_scope` re-encrypts every `*.age` file under a scope after applying
//! `--add-recipient` / `--remove-recipient` edits to the recipient list.
//! Decryption uses the caller's identity file (default `~/.config/age/keys.txt`).
//!
//! All `age` and `age-keygen` calls shell out to the system binaries.

use anyhow::{Context, Result, anyhow};
use std::path::{Path, PathBuf};
use std::process::Stdio;
use tokio::io::AsyncWriteExt;

/// Inputs for `rotate_scope`.
pub struct RotateOpts<'a> {
    /// Repo root containing `notes/`.
    pub repo_root: &'a Path,
    /// Scope name (becomes `notes/<scope>/`).
    pub scope: &'a str,
    /// Recipient strings to add (idempotent — duplicates are ignored).
    pub add_recipients: &'a [String],
    /// Recipient strings to remove (no-op if absent).
    pub remove_recipients: &'a [String],
    /// Identity file used for decryption. None → `~/.config/age/keys.txt`.
    pub identity: Option<&'a Path>,
}

/// Result of a rotation pass.
#[derive(Debug)]
pub struct RotateResult {
    pub files_rotated: usize,
    pub final_recipients: Vec<String>,
}

/// Rotate a scope: re-encrypt every `*.age` file with the updated recipient list.
pub async fn rotate_scope(opts: &RotateOpts<'_>) -> Result<RotateResult> {
    let scope_dir = opts.repo_root.join("notes").join(opts.scope);
    if !scope_dir.exists() {
        anyhow::bail!("scope directory does not exist: {}", scope_dir.display());
    }

    let recipients_path = scope_dir.join(".recipients");
    let mut recipients = read_recipients(&recipients_path)?;
    apply_recipient_edits(&mut recipients, opts.add_recipients, opts.remove_recipients);
    if recipients.is_empty() {
        anyhow::bail!("rotation would leave scope with no recipients — refusing");
    }

    let files = collect_age_files(&scope_dir)?;
    let identity_path = opts
        .identity
        .map(Path::to_path_buf)
        .unwrap_or_else(default_identity_path);

    for file in &files {
        let plaintext = age_decrypt(&identity_path, file).await.with_context(|| {
            format!(
                "decrypting {} with {}",
                file.display(),
                identity_path.display()
            )
        })?;
        let ciphertext = age_encrypt(&recipients, &plaintext)
            .await
            .with_context(|| format!("re-encrypting {}", file.display()))?;
        atomic_write(file, &ciphertext).with_context(|| format!("writing {}", file.display()))?;
    }

    write_recipients(&recipients_path, &recipients)?;

    Ok(RotateResult {
        files_rotated: files.len(),
        final_recipients: recipients,
    })
}

/// Read a `.recipients` file. Empty/missing → empty list.
pub fn read_recipients(path: &Path) -> Result<Vec<String>> {
    if !path.exists() {
        return Ok(vec![]);
    }
    let raw =
        std::fs::read_to_string(path).with_context(|| format!("reading {}", path.display()))?;
    Ok(raw
        .lines()
        .map(str::trim)
        .filter(|l| !l.is_empty() && !l.starts_with('#'))
        .map(str::to_string)
        .collect())
}

/// Write recipients (one per line, trailing newline).
pub fn write_recipients(path: &Path, recipients: &[String]) -> Result<()> {
    let body = format!("{}\n", recipients.join("\n"));
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent)?;
    }
    atomic_write(path, body.as_bytes())
}

/// Idempotent add/remove. Order preserved for additions; existing entries kept.
pub fn apply_recipient_edits(recipients: &mut Vec<String>, add: &[String], remove: &[String]) {
    recipients.retain(|r| !remove.contains(r));
    for new in add {
        if !recipients.contains(new) {
            recipients.push(new.clone());
        }
    }
}

/// Walk `<dir>/**/*.age` (one level deep + recursive).
pub fn collect_age_files(dir: &Path) -> Result<Vec<PathBuf>> {
    let mut out = vec![];
    walk_age(dir, &mut out)?;
    out.sort();
    Ok(out)
}

fn walk_age(dir: &Path, out: &mut Vec<PathBuf>) -> Result<()> {
    for entry in
        std::fs::read_dir(dir).with_context(|| format!("reading directory {}", dir.display()))?
    {
        let entry = entry?;
        let path = entry.path();
        if path.is_dir() {
            walk_age(&path, out)?;
        } else if path.extension().map(|e| e == "age").unwrap_or(false) {
            out.push(path);
        }
    }
    Ok(())
}

/// Atomic file write: write to `<path>.tmp` then rename.
fn atomic_write(path: &Path, content: &[u8]) -> Result<()> {
    let tmp = path.with_extension(format!(
        "{}.tmp",
        path.extension().and_then(|s| s.to_str()).unwrap_or("")
    ));
    std::fs::write(&tmp, content).with_context(|| format!("writing {}", tmp.display()))?;
    std::fs::rename(&tmp, path).with_context(|| format!("renaming to {}", path.display()))?;
    Ok(())
}

fn default_identity_path() -> PathBuf {
    if let Ok(home) = std::env::var("HOME") {
        return PathBuf::from(home).join(".config/age/keys.txt");
    }
    PathBuf::from(".config/age/keys.txt")
}

/// Decrypt `path` with `age -d -i <identity>`. Returns plaintext.
async fn age_decrypt(identity: &Path, path: &Path) -> Result<Vec<u8>> {
    let out = tokio::process::Command::new("age")
        .args([
            "-d",
            "-i",
            identity.to_str().context("identity path is not utf-8")?,
            path.to_str().context("file path is not utf-8")?,
        ])
        .output()
        .await
        .context("spawning age -d")?;
    if !out.status.success() {
        return Err(anyhow!(
            "age -d exited {}: {}",
            out.status,
            String::from_utf8_lossy(&out.stderr)
        ));
    }
    Ok(out.stdout)
}

/// Encrypt `plaintext` with `age -r <r1> -r <r2> ...`. Returns ciphertext.
async fn age_encrypt(recipients: &[String], plaintext: &[u8]) -> Result<Vec<u8>> {
    let mut args = vec!["-a".to_string()]; // armored, easier to inspect
    for r in recipients {
        args.push("-r".to_string());
        args.push(r.clone());
    }
    let mut child = tokio::process::Command::new("age")
        .args(&args)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .context("spawning age")?;

    if let Some(mut stdin) = child.stdin.take() {
        stdin
            .write_all(plaintext)
            .await
            .context("writing plaintext")?;
        stdin.shutdown().await.context("closing stdin")?;
    }
    let out = child.wait_with_output().await.context("waiting for age")?;
    if !out.status.success() {
        return Err(anyhow!(
            "age exited {}: {}",
            out.status,
            String::from_utf8_lossy(&out.stderr)
        ));
    }
    Ok(out.stdout)
}

// ── Tests ─────────────────────────────────────────────────────────────────────

#[cfg(test)]
mod rotation {
    use super::*;
    use std::process::Command;

    fn age_available() -> bool {
        Command::new("age")
            .arg("--version")
            .output()
            .map(|o| o.status.success())
            .unwrap_or(false)
            && Command::new("age-keygen")
                .arg("--version")
                .output()
                .map(|o| o.status.success())
                .unwrap_or(false)
    }

    fn generate_identity(dir: &Path, name: &str) -> (PathBuf, String) {
        let identity_path = dir.join(format!("{name}.key"));
        let out = Command::new("age-keygen")
            .args(["-o", identity_path.to_str().unwrap()])
            .output()
            .expect("age-keygen");
        assert!(out.status.success(), "age-keygen failed: {:?}", out);
        // Stderr line "Public key: age1..." is the recipient.
        let stderr = String::from_utf8_lossy(&out.stderr);
        let recipient = stderr
            .lines()
            .find_map(|l| l.strip_prefix("Public key: "))
            .expect("no public key in age-keygen stderr")
            .to_string();
        (identity_path, recipient)
    }

    #[test]
    fn read_recipients_skips_comments_and_blanks() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("recipients");
        std::fs::write(&path, "# header\nage1abc\n\nage1def\n# trailing\n").unwrap();
        let r = read_recipients(&path).unwrap();
        assert_eq!(r, vec!["age1abc", "age1def"]);
    }

    #[test]
    fn read_recipients_missing_file_returns_empty() {
        let dir = tempfile::tempdir().unwrap();
        let r = read_recipients(&dir.path().join("nope")).unwrap();
        assert!(r.is_empty());
    }

    #[test]
    fn apply_edits_dedupes_additions() {
        let mut r = vec!["a".to_string(), "b".to_string()];
        apply_recipient_edits(&mut r, &["b".to_string(), "c".to_string()], &[]);
        assert_eq!(r, vec!["a", "b", "c"]);
    }

    #[test]
    fn apply_edits_removes_then_adds() {
        let mut r = vec!["a".to_string(), "b".to_string()];
        apply_recipient_edits(
            &mut r,
            &["b".to_string()],
            &["a".to_string(), "missing".to_string()],
        );
        // 'a' removed, 'b' kept (already present, not duplicated)
        assert_eq!(r, vec!["b"]);
    }

    #[test]
    fn collect_age_files_recurses_and_filters() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("a.age"), b"x").unwrap();
        std::fs::write(dir.path().join("b.txt"), b"x").unwrap();
        let sub = dir.path().join("sub");
        std::fs::create_dir(&sub).unwrap();
        std::fs::write(sub.join("c.age"), b"x").unwrap();
        let files = collect_age_files(dir.path()).unwrap();
        assert_eq!(files.len(), 2);
        assert!(files.iter().any(|p| p.ends_with("a.age")));
        assert!(files.iter().any(|p| p.ends_with("c.age")));
    }

    #[test]
    fn write_recipients_then_read_roundtrips() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(".recipients");
        write_recipients(&path, &vec!["age1x".to_string(), "age1y".to_string()]).unwrap();
        let back = read_recipients(&path).unwrap();
        assert_eq!(back, vec!["age1x", "age1y"]);
    }

    #[tokio::test]
    async fn rotate_refuses_empty_recipient_list() {
        let dir = tempfile::tempdir().unwrap();
        let scope_dir = dir.path().join("notes/work");
        std::fs::create_dir_all(&scope_dir).unwrap();
        std::fs::write(scope_dir.join(".recipients"), "age1abc\n").unwrap();

        let opts = RotateOpts {
            repo_root: dir.path(),
            scope: "work",
            add_recipients: &[],
            remove_recipients: &["age1abc".to_string()],
            identity: None,
        };
        let err = rotate_scope(&opts).await.unwrap_err();
        assert!(err.to_string().contains("no recipients"), "got: {err}");
    }

    #[tokio::test]
    async fn rotate_errors_when_scope_missing() {
        let dir = tempfile::tempdir().unwrap();
        let opts = RotateOpts {
            repo_root: dir.path(),
            scope: "ghost",
            add_recipients: &["age1x".to_string()],
            remove_recipients: &[],
            identity: None,
        };
        let err = rotate_scope(&opts).await.unwrap_err();
        assert!(
            err.to_string().contains("does not exist"),
            "expected missing-scope error, got: {err}"
        );
    }

    #[tokio::test]
    async fn rotate_with_real_age_adds_recipient_and_rotates() {
        if !age_available() {
            eprintln!("skipping: age/age-keygen not on PATH");
            return;
        }

        let dir = tempfile::tempdir().unwrap();
        let scope_dir = dir.path().join("notes/work");
        std::fs::create_dir_all(&scope_dir).unwrap();

        // Generate two identities.
        let (id1_path, rec1) = generate_identity(dir.path(), "id1");
        let (id2_path, rec2) = generate_identity(dir.path(), "id2");

        // Write initial recipients (just rec1).
        std::fs::write(scope_dir.join(".recipients"), format!("{rec1}\n")).unwrap();

        // Encrypt a sample file with rec1.
        let sample_plain = b"the quick brown fox";
        let ciphertext = age_encrypt(&[rec1.clone()], sample_plain).await.unwrap();
        let sample_path = scope_dir.join("note1.age");
        std::fs::write(&sample_path, &ciphertext).unwrap();

        // Rotate: add rec2.
        let opts = RotateOpts {
            repo_root: dir.path(),
            scope: "work",
            add_recipients: &[rec2.clone()],
            remove_recipients: &[],
            identity: Some(&id1_path),
        };
        let result = rotate_scope(&opts).await.unwrap();
        assert_eq!(result.files_rotated, 1);
        assert!(result.final_recipients.contains(&rec1));
        assert!(result.final_recipients.contains(&rec2));

        // Both identities should now decrypt the rotated file.
        let pt1 = age_decrypt(&id1_path, &sample_path).await.unwrap();
        assert_eq!(pt1, sample_plain);
        let pt2 = age_decrypt(&id2_path, &sample_path).await.unwrap();
        assert_eq!(pt2, sample_plain);

        // .recipients on disk should match.
        let on_disk = read_recipients(&scope_dir.join(".recipients")).unwrap();
        assert!(on_disk.contains(&rec1));
        assert!(on_disk.contains(&rec2));
    }
}
