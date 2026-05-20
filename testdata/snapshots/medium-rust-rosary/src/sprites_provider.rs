//! SpritesProvider — `ComputeProvider` impl backed by sprites.dev.
//!
//! Lifecycle mapping:
//!   provision → POST /sprites (create container)
//!   exec      → `sprite exec` CLI (WebSocket streaming under the hood)
//!   checkpoint → `sprite checkpoint create` CLI (sequential v0/v1/v2 IDs)
//!   destroy   → DELETE /sprites/{name}

use anyhow::{Context, Result};

use crate::backend::{ComputeProvider, ExecHandle, ExecResult, ProvisionOpts};
use crate::sprites::{CreateOpts, SpritesClient};

/// Compute provider that runs agents inside sprites.dev containers.
pub struct SpritesProvider {
    client: SpritesClient,
    /// Default network egress allowlist.
    pub network_allowlist: Vec<String>,
    /// Whether to create checkpoints on completion.
    pub checkpoint_on_complete: bool,
}

impl SpritesProvider {
    pub fn new(client: SpritesClient) -> Self {
        Self {
            client,
            network_allowlist: Vec::new(),
            checkpoint_on_complete: false,
        }
    }

    pub fn with_network_allowlist(mut self, domains: Vec<String>) -> Self {
        self.network_allowlist = domains;
        self
    }

    pub fn with_checkpoints(mut self, enabled: bool) -> Self {
        self.checkpoint_on_complete = enabled;
        self
    }

    /// Deterministic sprite name from bead ID.
    fn sprite_name(bead_id: &str) -> String {
        format!("rsry-{bead_id}")
    }

    /// Shell-join a command slice, quoting args that contain spaces or quotes.
    fn shell_join(cmd: &[&str]) -> String {
        cmd.iter()
            .map(|s| {
                if s.contains(' ') || s.contains('\'') || s.contains('"') {
                    format!("'{}'", s.replace('\'', "'\\''"))
                } else {
                    s.to_string()
                }
            })
            .collect::<Vec<_>>()
            .join(" ")
    }
}

#[async_trait::async_trait]
impl ComputeProvider for SpritesProvider {
    async fn provision(&self, opts: &ProvisionOpts) -> Result<ExecHandle> {
        let name = Self::sprite_name(&opts.bead_id);

        let create_opts = CreateOpts::default();

        self.client
            .create_sprite(&name, &create_opts)
            .await
            .with_context(|| format!("provisioning sprite {name}"))?;

        // NOTE: resource and network policies are not yet available via the
        // sprites.dev API (endpoints returned 404 during validation 2026-03-14).
        // When the API adds these, wire them here.

        Ok(ExecHandle {
            id: name,
            backend: "sprites".to_string(),
        })
    }

    async fn exec(&self, handle: &ExecHandle, cmd: &[&str]) -> Result<ExecResult> {
        anyhow::ensure!(!cmd.is_empty(), "empty command");
        let cmd_str = Self::shell_join(cmd);

        let output = self
            .client
            .exec_with_exit_code(&handle.id, &cmd_str)
            .await
            .with_context(|| format!("exec in {}: {cmd_str}", handle.id))?;

        Ok(ExecResult {
            exit_code: output.exit_code,
            stdout: output.stdout,
            stderr: output.stderr,
        })
    }

    async fn destroy(&self, handle: &ExecHandle) -> Result<()> {
        self.client
            .delete_sprite(&handle.id)
            .await
            .with_context(|| format!("destroying sprite {}", handle.id))
    }

    async fn checkpoint(&self, handle: &ExecHandle) -> Result<Option<String>> {
        if !self.checkpoint_on_complete {
            return Ok(None);
        }
        let cp = self
            .client
            .create_checkpoint(&handle.id, Some("rosary auto-checkpoint"))
            .await
            .with_context(|| format!("checkpoint for {}", handle.id))?;
        Ok(Some(cp.id))
    }

    fn name(&self) -> &str {
        "sprites"
    }
}

// ---------------------------------------------------------------------------
// Tests — use MockProvider from backend.rs for trait tests.
// SpritesProvider-specific tests mock the SpritesClient at the HTTP level.
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sprite_name_deterministic() {
        assert_eq!(SpritesProvider::sprite_name("abc-123"), "rsry-abc-123");
        assert_eq!(SpritesProvider::sprite_name("loom-7sd"), "rsry-loom-7sd");
    }

    #[test]
    fn provider_name() {
        let client = SpritesClient::with_base_url("tok", "http://localhost:1").unwrap();
        let provider = SpritesProvider::new(client);
        assert_eq!(provider.name(), "sprites");
    }

    #[test]
    fn builder_network_allowlist() {
        let client = SpritesClient::with_base_url("tok", "http://localhost:1").unwrap();
        let provider =
            SpritesProvider::new(client).with_network_allowlist(vec!["api.github.com".into()]);
        assert_eq!(provider.network_allowlist, vec!["api.github.com"]);
    }

    #[test]
    fn builder_checkpoints() {
        let client = SpritesClient::with_base_url("tok", "http://localhost:1").unwrap();
        let provider = SpritesProvider::new(client).with_checkpoints(true);
        assert!(provider.checkpoint_on_complete);
    }

    #[test]
    fn shell_join_simple() {
        assert_eq!(
            SpritesProvider::shell_join(&["echo", "hello", "world"]),
            "echo hello world"
        );
    }

    #[test]
    fn shell_join_with_spaces() {
        assert_eq!(
            SpritesProvider::shell_join(&["echo", "hello world"]),
            "echo 'hello world'"
        );
    }

    #[test]
    fn trait_object_is_send_sync() {
        fn assert_send_sync<T: Send + Sync>() {}
        assert_send_sync::<SpritesProvider>();
    }
}
