//! Pluggable compute provider for agent execution.
//!
//! The `ComputeProvider` trait abstracts WHERE agent code runs, orthogonal
//! to `AgentProvider` (WHICH model runs). Think of it like fly/gcp/aws —
//! sprites.dev is just the first remote provider.
//!
//! - `LocalProvider`: current behavior — tokio subprocess on the host
//! - `SpritesProvider`: provision a sprite container, exec inside it
//! - Future: K8sProvider, DockerProvider, etc.

use anyhow::{Context as _, Result};

// ---------------------------------------------------------------------------
// Core types
// ---------------------------------------------------------------------------

/// Options for provisioning a compute environment.
#[derive(Debug, Clone)]
pub struct ProvisionOpts {
    /// Bead ID — used to name the environment deterministically.
    pub bead_id: String,
    /// Repo name — for labeling/tagging the environment.
    pub repo: String,
    /// CPU cores (provider interprets; None = provider default).
    pub cpu: Option<u32>,
    /// Memory in MB (provider interprets; None = provider default).
    pub memory_mb: Option<u32>,
    /// Network egress allowlist (domains). Empty = provider default.
    pub network_allowlist: Vec<String>,
}

impl ProvisionOpts {
    pub fn new(bead_id: &str, repo: &str) -> Self {
        Self {
            bead_id: bead_id.to_string(),
            repo: repo.to_string(),
            cpu: None,
            memory_mb: None,
            network_allowlist: Vec::new(),
        }
    }

    pub fn cpu(mut self, cores: u32) -> Self {
        self.cpu = Some(cores);
        self
    }

    pub fn memory_mb(mut self, mb: u32) -> Self {
        self.memory_mb = Some(mb);
        self
    }

    pub fn network_allowlist(mut self, domains: Vec<String>) -> Self {
        self.network_allowlist = domains;
        self
    }
}

/// Handle to a provisioned compute environment.
///
/// Opaque to callers — the provider knows how to use the `id` field
/// to exec commands, checkpoint, and destroy.
#[derive(Debug, Clone)]
pub struct ExecHandle {
    /// Provider-specific identifier (sprite name, PID, pod name, etc.).
    pub id: String,
    /// Provider name — for logging.
    pub backend: String,
}

/// Result of executing a command in a compute environment.
#[derive(Debug, Clone)]
pub struct ExecResult {
    /// Process exit code (0 = success).
    pub exit_code: i32,
    /// Captured stdout.
    pub stdout: String,
    /// Captured stderr.
    pub stderr: String,
}

impl ExecResult {
    pub fn success(&self) -> bool {
        self.exit_code == 0
    }
}

// ---------------------------------------------------------------------------
// Trait
// ---------------------------------------------------------------------------

/// Abstraction over compute providers (local, sprites, k8s, docker, ...).
///
/// Lifecycle: provision → exec (1..n) → checkpoint? → destroy.
#[async_trait::async_trait]
pub trait ComputeProvider: Send + Sync {
    /// Provision a compute environment. Returns a handle for subsequent ops.
    async fn provision(&self, opts: &ProvisionOpts) -> Result<ExecHandle>;

    /// Execute a command in the environment. Blocks until completion.
    async fn exec(&self, handle: &ExecHandle, cmd: &[&str]) -> Result<ExecResult>;

    /// Tear down the environment and release resources.
    async fn destroy(&self, handle: &ExecHandle) -> Result<()>;

    /// Create a checkpoint/snapshot of the current environment state.
    /// Returns checkpoint ID if supported, None if not.
    async fn checkpoint(&self, _handle: &ExecHandle) -> Result<Option<String>> {
        Ok(None)
    }

    /// Human-readable provider name.
    fn name(&self) -> &str;
}

// ---------------------------------------------------------------------------
// LocalProvider — runs commands as host subprocesses
// ---------------------------------------------------------------------------

/// Compute provider that runs commands as local subprocesses.
///
/// This is the zero-config default. `provision()` is a no-op (returns a
/// handle pointing at the host). `exec()` shells out via tokio::process.
pub struct LocalProvider;

#[async_trait::async_trait]
impl ComputeProvider for LocalProvider {
    async fn provision(&self, opts: &ProvisionOpts) -> Result<ExecHandle> {
        Ok(ExecHandle {
            id: format!("local-{}", opts.bead_id),
            backend: "local".to_string(),
        })
    }

    async fn exec(&self, _handle: &ExecHandle, cmd: &[&str]) -> Result<ExecResult> {
        anyhow::ensure!(!cmd.is_empty(), "empty command");

        let output = tokio::process::Command::new(cmd[0])
            .args(&cmd[1..])
            .output()
            .await
            .with_context(|| format!("executing: {}", cmd.join(" ")))?;

        Ok(ExecResult {
            exit_code: output.status.code().unwrap_or(-1),
            stdout: String::from_utf8_lossy(&output.stdout).to_string(),
            stderr: String::from_utf8_lossy(&output.stderr).to_string(),
        })
    }

    async fn destroy(&self, _handle: &ExecHandle) -> Result<()> {
        Ok(()) // no-op for local
    }

    fn name(&self) -> &str {
        "local"
    }
}

// ---------------------------------------------------------------------------
// DockerProvider — runs commands inside Docker containers
// ---------------------------------------------------------------------------

/// Compute provider that runs commands inside local Docker containers.
///
/// Lifecycle:
/// - `provision()`: `docker create` a container from the configured image
/// - `exec()`: `docker exec` inside the running container
/// - `checkpoint()`: `docker commit` → returns image ID
/// - `destroy()`: `docker rm -f`
pub struct DockerProvider {
    /// Docker image to use (default: built from rig Dockerfile).
    pub image: String,
}

impl Default for DockerProvider {
    fn default() -> Self {
        Self {
            image: "ghcr.io/agentic-research/rig:latest".to_string(),
        }
    }
}

impl DockerProvider {
    pub fn with_image(image: &str) -> Self {
        Self {
            image: image.to_string(),
        }
    }

    fn container_name(bead_id: &str) -> String {
        format!("rsry-{bead_id}")
    }
}

#[async_trait::async_trait]
impl ComputeProvider for DockerProvider {
    async fn provision(&self, opts: &ProvisionOpts) -> Result<ExecHandle> {
        let name = Self::container_name(&opts.bead_id);

        let mut args = vec![
            "run".to_string(),
            "-d".to_string(),
            "--name".to_string(),
            name.clone(),
            "--label".to_string(),
            format!("rsry.bead={}", opts.bead_id),
            "--label".to_string(),
            format!("rsry.repo={}", opts.repo),
        ];

        if let Some(cpu) = opts.cpu {
            args.extend(["--cpus".to_string(), cpu.to_string()]);
        }
        if let Some(mem) = opts.memory_mb {
            args.extend(["-m".to_string(), format!("{mem}m")]);
        }

        // Keep container alive (agent exec comes later)
        args.push(self.image.clone());
        args.extend(["sleep".to_string(), "infinity".to_string()]);

        let output = tokio::process::Command::new("docker")
            .args(&args)
            .output()
            .await
            .with_context(|| format!("docker run for {}", opts.bead_id))?;

        if !output.status.success() {
            let stderr = String::from_utf8_lossy(&output.stderr);
            anyhow::bail!("docker run failed: {stderr}");
        }

        Ok(ExecHandle {
            id: name,
            backend: "docker".to_string(),
        })
    }

    async fn exec(&self, handle: &ExecHandle, cmd: &[&str]) -> Result<ExecResult> {
        anyhow::ensure!(!cmd.is_empty(), "empty command");

        let mut args = vec!["exec", &handle.id];
        args.extend(cmd);

        let output = tokio::process::Command::new("docker")
            .args(&args)
            .output()
            .await
            .with_context(|| format!("docker exec in {}: {}", handle.id, cmd.join(" ")))?;

        Ok(ExecResult {
            exit_code: output.status.code().unwrap_or(-1),
            stdout: String::from_utf8_lossy(&output.stdout).to_string(),
            stderr: String::from_utf8_lossy(&output.stderr).to_string(),
        })
    }

    async fn destroy(&self, handle: &ExecHandle) -> Result<()> {
        let output = tokio::process::Command::new("docker")
            .args(["rm", "-f", &handle.id])
            .output()
            .await
            .with_context(|| format!("docker rm {}", handle.id))?;

        if !output.status.success() {
            let stderr = String::from_utf8_lossy(&output.stderr);
            eprintln!("[docker] destroy {}: {stderr}", handle.id);
        }
        Ok(())
    }

    async fn checkpoint(&self, handle: &ExecHandle) -> Result<Option<String>> {
        let output = tokio::process::Command::new("docker")
            .args(["commit", &handle.id])
            .output()
            .await
            .with_context(|| format!("docker commit {}", handle.id))?;

        if output.status.success() {
            let image_id = String::from_utf8_lossy(&output.stdout).trim().to_string();
            Ok(Some(image_id))
        } else {
            Ok(None)
        }
    }

    fn name(&self) -> &str {
        "docker"
    }
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use std::collections::VecDeque;
    use std::sync::Mutex;

    // -----------------------------------------------------------------------
    // MockProvider — records calls, returns canned results
    // -----------------------------------------------------------------------

    /// Mock compute provider for testing dispatch/reconciler without real infra.
    pub struct MockProvider {
        pub provisions: Mutex<Vec<ProvisionOpts>>,
        pub execs: Mutex<Vec<Vec<String>>>,
        pub destroys: Mutex<Vec<String>>,
        pub checkpoints: Mutex<Vec<String>>,
        results: Mutex<VecDeque<ExecResult>>,
    }

    impl MockProvider {
        pub fn new() -> Self {
            Self {
                provisions: Mutex::new(Vec::new()),
                execs: Mutex::new(Vec::new()),
                destroys: Mutex::new(Vec::new()),
                checkpoints: Mutex::new(Vec::new()),
                results: Mutex::new(VecDeque::new()),
            }
        }

        /// Queue an ExecResult to be returned by the next exec() call.
        pub fn enqueue_result(&self, result: ExecResult) {
            self.results.lock().unwrap().push_back(result);
        }
    }

    #[async_trait::async_trait]
    impl ComputeProvider for MockProvider {
        async fn provision(&self, opts: &ProvisionOpts) -> Result<ExecHandle> {
            self.provisions.lock().unwrap().push(opts.clone());
            Ok(ExecHandle {
                id: format!("mock-{}", opts.bead_id),
                backend: "mock".to_string(),
            })
        }

        async fn exec(&self, _handle: &ExecHandle, cmd: &[&str]) -> Result<ExecResult> {
            self.execs
                .lock()
                .unwrap()
                .push(cmd.iter().map(|s| s.to_string()).collect());

            let result = self
                .results
                .lock()
                .unwrap()
                .pop_front()
                .unwrap_or(ExecResult {
                    exit_code: 0,
                    stdout: String::new(),
                    stderr: String::new(),
                });
            Ok(result)
        }

        async fn destroy(&self, handle: &ExecHandle) -> Result<()> {
            self.destroys.lock().unwrap().push(handle.id.clone());
            Ok(())
        }

        async fn checkpoint(&self, handle: &ExecHandle) -> Result<Option<String>> {
            self.checkpoints.lock().unwrap().push(handle.id.clone());
            Ok(Some(format!("cp-{}", handle.id)))
        }

        fn name(&self) -> &str {
            "mock"
        }
    }

    // -----------------------------------------------------------------------
    // ProvisionOpts tests
    // -----------------------------------------------------------------------

    #[test]
    fn provision_opts_builder() {
        let opts = ProvisionOpts::new("bead-1", "rosary")
            .cpu(4)
            .memory_mb(8192)
            .network_allowlist(vec!["api.github.com".into()]);

        assert_eq!(opts.bead_id, "bead-1");
        assert_eq!(opts.repo, "rosary");
        assert_eq!(opts.cpu, Some(4));
        assert_eq!(opts.memory_mb, Some(8192));
        assert_eq!(opts.network_allowlist, vec!["api.github.com"]);
    }

    #[test]
    fn provision_opts_defaults() {
        let opts = ProvisionOpts::new("x", "r");
        assert!(opts.cpu.is_none());
        assert!(opts.memory_mb.is_none());
        assert!(opts.network_allowlist.is_empty());
    }

    // -----------------------------------------------------------------------
    // ExecResult tests
    // -----------------------------------------------------------------------

    #[test]
    fn exec_result_success() {
        let r = ExecResult {
            exit_code: 0,
            stdout: "ok".into(),
            stderr: String::new(),
        };
        assert!(r.success());
    }

    #[test]
    fn exec_result_failure() {
        let r = ExecResult {
            exit_code: 1,
            stdout: String::new(),
            stderr: "error".into(),
        };
        assert!(!r.success());
    }

    #[test]
    fn exec_result_negative_exit_code() {
        let r = ExecResult {
            exit_code: -1,
            stdout: String::new(),
            stderr: "signal".into(),
        };
        assert!(!r.success());
    }

    // -----------------------------------------------------------------------
    // ExecHandle tests
    // -----------------------------------------------------------------------

    #[test]
    fn exec_handle_clone() {
        let h = ExecHandle {
            id: "test-1".into(),
            backend: "local".into(),
        };
        let h2 = h.clone();
        assert_eq!(h.id, h2.id);
        assert_eq!(h.backend, h2.backend);
    }

    // -----------------------------------------------------------------------
    // MockProvider tests
    // -----------------------------------------------------------------------

    #[tokio::test]
    async fn mock_provision_records_opts() {
        let mock = MockProvider::new();
        let opts = ProvisionOpts::new("bead-42", "mache");
        let handle = mock.provision(&opts).await.unwrap();

        assert_eq!(handle.id, "mock-bead-42");
        assert_eq!(handle.backend, "mock");

        let provisions = mock.provisions.lock().unwrap();
        assert_eq!(provisions.len(), 1);
        assert_eq!(provisions[0].bead_id, "bead-42");
    }

    #[tokio::test]
    async fn mock_exec_records_commands() {
        let mock = MockProvider::new();
        let handle = ExecHandle {
            id: "mock-1".into(),
            backend: "mock".into(),
        };

        mock.enqueue_result(ExecResult {
            exit_code: 0,
            stdout: "hello".into(),
            stderr: String::new(),
        });

        let result = mock.exec(&handle, &["echo", "hello"]).await.unwrap();
        assert!(result.success());
        assert_eq!(result.stdout, "hello");

        let execs = mock.execs.lock().unwrap();
        assert_eq!(execs.len(), 1);
        assert_eq!(execs[0], vec!["echo", "hello"]);
    }

    #[tokio::test]
    async fn mock_exec_default_success() {
        let mock = MockProvider::new();
        let handle = ExecHandle {
            id: "mock-1".into(),
            backend: "mock".into(),
        };

        // No enqueued result — should return default (exit_code=0)
        let result = mock.exec(&handle, &["test"]).await.unwrap();
        assert!(result.success());
    }

    #[tokio::test]
    async fn mock_destroy_records() {
        let mock = MockProvider::new();
        let handle = ExecHandle {
            id: "mock-xyz".into(),
            backend: "mock".into(),
        };

        mock.destroy(&handle).await.unwrap();

        let destroys = mock.destroys.lock().unwrap();
        assert_eq!(*destroys, vec!["mock-xyz"]);
    }

    #[tokio::test]
    async fn mock_checkpoint_returns_id() {
        let mock = MockProvider::new();
        let handle = ExecHandle {
            id: "mock-1".into(),
            backend: "mock".into(),
        };

        let cp = mock.checkpoint(&handle).await.unwrap();
        assert_eq!(cp, Some("cp-mock-1".to_string()));

        let checkpoints = mock.checkpoints.lock().unwrap();
        assert_eq!(*checkpoints, vec!["mock-1"]);
    }

    // -----------------------------------------------------------------------
    // LocalProvider tests
    // -----------------------------------------------------------------------

    #[tokio::test]
    async fn local_provision_returns_handle() {
        let local = LocalProvider;
        let opts = ProvisionOpts::new("bead-99", "rosary");
        let handle = local.provision(&opts).await.unwrap();

        assert_eq!(handle.id, "local-bead-99");
        assert_eq!(handle.backend, "local");
    }

    #[tokio::test]
    async fn local_exec_echo() {
        let local = LocalProvider;
        let handle = ExecHandle {
            id: "local-1".into(),
            backend: "local".into(),
        };

        let result = local.exec(&handle, &["echo", "hello world"]).await.unwrap();
        assert!(result.success());
        assert_eq!(result.stdout.trim(), "hello world");
    }

    #[tokio::test]
    async fn local_exec_captures_exit_code() {
        let local = LocalProvider;
        let handle = ExecHandle {
            id: "local-1".into(),
            backend: "local".into(),
        };

        let result = local.exec(&handle, &["false"]).await.unwrap();
        assert!(!result.success());
        assert_eq!(result.exit_code, 1);
    }

    #[tokio::test]
    async fn local_exec_empty_command_errors() {
        let local = LocalProvider;
        let handle = ExecHandle {
            id: "local-1".into(),
            backend: "local".into(),
        };

        let result = local.exec(&handle, &[]).await;
        assert!(result.is_err());
    }

    #[tokio::test]
    async fn local_exec_captures_stderr() {
        let local = LocalProvider;
        let handle = ExecHandle {
            id: "local-1".into(),
            backend: "local".into(),
        };

        let result = local
            .exec(&handle, &["sh", "-c", "echo oops >&2; exit 1"])
            .await
            .unwrap();
        assert!(!result.success());
        assert!(result.stderr.contains("oops"));
    }

    #[tokio::test]
    async fn local_destroy_is_noop() {
        let local = LocalProvider;
        let handle = ExecHandle {
            id: "local-1".into(),
            backend: "local".into(),
        };

        // Should succeed without error
        local.destroy(&handle).await.unwrap();
    }

    #[tokio::test]
    async fn local_checkpoint_returns_none() {
        let local = LocalProvider;
        let handle = ExecHandle {
            id: "local-1".into(),
            backend: "local".into(),
        };

        // Default impl returns None
        let cp = local.checkpoint(&handle).await.unwrap();
        assert!(cp.is_none());
    }

    #[test]
    fn local_provider_name() {
        assert_eq!(LocalProvider.name(), "local");
    }

    // -----------------------------------------------------------------------
    // Trait object tests — verify Send + Sync + dyn dispatch
    // -----------------------------------------------------------------------

    #[test]
    fn trait_object_is_send_sync() {
        fn assert_send_sync<T: Send + Sync>() {}
        assert_send_sync::<LocalProvider>();
        assert_send_sync::<MockProvider>();
    }

    #[tokio::test]
    async fn trait_object_dispatch() {
        let provider: Box<dyn ComputeProvider> = Box::new(LocalProvider);
        assert_eq!(provider.name(), "local");

        let opts = ProvisionOpts::new("dyn-test", "repo");
        let handle = provider.provision(&opts).await.unwrap();
        assert!(handle.id.starts_with("local-"));

        let result = provider.exec(&handle, &["true"]).await.unwrap();
        assert!(result.success());

        provider.destroy(&handle).await.unwrap();
    }
}
