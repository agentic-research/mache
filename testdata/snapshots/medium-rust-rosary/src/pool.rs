//! Long-lived connection pool for bead stores across registered repos.
//!
//! The MCP server creates a `RepoPool` on startup, connecting to all
//! repos with .beads/ directories. Connections are reused across tool
//! calls — no per-request connect overhead.

use std::collections::HashMap;
use std::path::{Path, PathBuf};

use anyhow::Result;

use crate::config;
use crate::store::BeadStore;

/// Read the current Dolt server port from a `.beads/` directory.
/// Returns `None` if the port file is absent or unparseable.
fn read_dolt_port(beads_dir: &Path) -> Option<u16> {
    let port_file = beads_dir.join("dolt-server.port");
    let s = std::fs::read_to_string(port_file).ok()?;
    s.trim().parse().ok()
}

/// Return `true` if the Dolt port for this repo has changed since the pool was built.
/// Stale connections must not be returned from `get()` — the MySQL pool is pinned
/// to the old port and all queries will fail after a dolt-server restart.
fn is_dolt_port_stale(
    known_ports: &HashMap<String, u16>,
    beads_dirs: &HashMap<String, PathBuf>,
    repo_name: &str,
) -> bool {
    let Some(known) = known_ports.get(repo_name) else {
        return false;
    };
    let Some(beads_dir) = beads_dirs.get(repo_name) else {
        return false;
    };
    // Only check for Dolt repos (SQLite repos don't have port files).
    if !beads_dir.join("dolt").exists() {
        return false;
    }
    match read_dolt_port(beads_dir) {
        Some(current) => current != *known,
        None => false, // port file gone → server not running, don't invalidate
    }
}

/// Long-lived pool of BeadStore connections keyed by repo name.
pub struct RepoPool {
    clients: HashMap<String, Box<dyn BeadStore>>,
    paths: HashMap<String, PathBuf>,
    /// `.beads/` directories used when connecting (for reconnect + port check).
    beads_dirs: HashMap<String, PathBuf>,
    /// Dolt server port observed at connect time. Used to detect restarts.
    known_ports: HashMap<String, u16>,
}

impl RepoPool {
    /// Create an empty pool (for testing and HTTP server startup with no repos).
    #[allow(dead_code)] // used in tests
    pub fn empty() -> Self {
        RepoPool {
            clients: HashMap::new(),
            paths: HashMap::new(),
            beads_dirs: HashMap::new(),
            known_ports: HashMap::new(),
        }
    }

    /// Create a pool and connect to all repos in the given config.
    /// Repos that fail to connect are logged and skipped (best-effort).
    pub async fn from_config(config_path: &str) -> Result<Self> {
        let cfg = config::load_merged(config_path)?;
        let mut clients: HashMap<String, Box<dyn BeadStore>> = HashMap::new();
        let mut paths = HashMap::new();
        let mut beads_dirs = HashMap::new();
        let mut known_ports: HashMap<String, u16> = HashMap::new();

        for repo in &cfg.repo {
            let path = crate::scanner::expand_path(&repo.path);
            let beads_dir = path.join(".beads");
            if !beads_dir.exists() {
                continue;
            }

            paths.insert(repo.name.clone(), path.clone());

            // Record the Dolt port used at connect time so we can detect restarts later.
            let port = read_dolt_port(&beads_dir);
            if let Some(p) = port {
                known_ports.insert(repo.name.clone(), p);
            }

            match crate::bead_sqlite::connect_bead_store(&beads_dir).await {
                Ok(store) => {
                    eprintln!("[pool] connected: {}", repo.name);
                    clients.insert(repo.name.clone(), store);
                    beads_dirs.insert(repo.name.clone(), beads_dir);
                }
                Err(e) => {
                    eprintln!("[pool] skipping {} (connect failed: {e})", repo.name);
                }
            }
        }

        Ok(RepoPool {
            clients,
            paths,
            beads_dirs,
            known_ports,
        })
    }

    /// Get a BeadStore by repo name.
    /// Returns `None` if the repo is unknown or its Dolt port changed since startup
    /// (stale connection — caller will fall through to a fresh connect).
    pub fn get(&self, repo_name: &str) -> Option<&dyn BeadStore> {
        if is_dolt_port_stale(&self.known_ports, &self.beads_dirs, repo_name) {
            eprintln!("[pool] {repo_name}: Dolt port changed, bypassing stale pool entry");
            return None;
        }
        self.clients.get(repo_name).map(|b| b.as_ref())
    }

    /// Get a BeadStore by repo path (resolves name from path).
    /// Resolves repo path via discover_repo_root (no symlink resolution).
    /// Returns `None` if the repo's Dolt port changed since startup.
    pub fn get_by_path(&self, repo_path: &str) -> Option<(&str, &dyn BeadStore)> {
        let target = Path::new(repo_path);
        let discovered = config::discover_repo_root(target).unwrap_or_else(|| target.to_path_buf());
        let root = crate::scanner::expand_path(&discovered);

        for (name, path) in &self.paths {
            if *path == root {
                if is_dolt_port_stale(&self.known_ports, &self.beads_dirs, name) {
                    eprintln!("[pool] {name}: Dolt port changed, bypassing stale pool entry");
                    return None;
                }
                if let Some(client) = self.clients.get(name) {
                    return Some((name.as_str(), client.as_ref()));
                }
            }
        }
        None
    }

    /// Number of connected repos.
    pub fn len(&self) -> usize {
        self.clients.len()
    }

    /// Whether pool has no connections (required by clippy alongside `len()`).
    #[allow(dead_code)]
    pub fn is_empty(&self) -> bool {
        self.clients.is_empty()
    }

    /// List connected repo names.
    pub fn repo_names(&self) -> Vec<&str> {
        self.clients.keys().map(|s| s.as_str()).collect()
    }

    /// Iterate over all (repo_name, client) pairs. Used by webhook handler.
    #[allow(dead_code)]
    pub fn iter_clients(&self) -> impl Iterator<Item = (&str, &dyn BeadStore)> {
        self.clients.iter().map(|(k, v)| (k.as_str(), v.as_ref()))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn empty_pool() {
        let pool = RepoPool {
            clients: HashMap::new(),
            paths: HashMap::new(),
            beads_dirs: HashMap::new(),
            known_ports: HashMap::new(),
        };
        assert!(pool.is_empty());
        assert_eq!(pool.len(), 0);
        assert!(pool.get("nonexistent").is_none());
        assert!(pool.get_by_path("/tmp/fake").is_none());
        assert!(pool.repo_names().is_empty());
    }

    #[test]
    fn pool_paths_resolve() {
        let mut paths = HashMap::new();
        paths.insert("myrepo".to_string(), PathBuf::from("/tmp/myrepo"));

        let pool = RepoPool {
            clients: HashMap::new(),
            paths,
            beads_dirs: HashMap::new(),
            known_ports: HashMap::new(),
        };

        // No client for this path, but path resolution works
        assert!(pool.get_by_path("/tmp/myrepo").is_none()); // no client
        assert!(pool.get("myrepo").is_none()); // no client
    }

    #[tokio::test]
    async fn from_config_handles_missing_config() {
        let result = RepoPool::from_config("/nonexistent/rosary.toml").await;
        // Should succeed with empty pool (load_merged falls back to global)
        assert!(result.is_ok());
    }

    #[tokio::test]
    async fn from_config_skips_repos_without_beads() {
        // Create a temp config with a repo that has no .beads/
        let tmp = tempfile::TempDir::new().unwrap();
        let config_path = tmp.path().join("test.toml");
        std::fs::write(
            &config_path,
            r#"
[[repo]]
name = "fake"
path = "/tmp/no-such-repo-xyz"
"#,
        )
        .unwrap();

        let pool = RepoPool::from_config(config_path.to_str().unwrap())
            .await
            .unwrap();
        // The fake repo should not be connected (no .beads/ dir).
        // Note: load_merged may connect real repos from ~/.rsry/config.toml.
        assert!(pool.get("fake").is_none());
    }

    #[test]
    fn stale_dolt_port_returns_none() {
        let tmp = tempfile::TempDir::new().unwrap();
        let beads_dir = tmp.path().to_path_buf();

        // Create a fake dolt/ dir so is_dolt_port_stale treats this as a Dolt repo.
        std::fs::create_dir(beads_dir.join("dolt")).unwrap();

        // Write port 12345 as the "current" port.
        std::fs::write(beads_dir.join("dolt-server.port"), "12345").unwrap();

        let repo_name = "stalerepo";
        let mut known_ports = HashMap::new();
        // Pool was built with port 9999 — server restarted on 12345.
        known_ports.insert(repo_name.to_string(), 9999u16);

        let mut beads_dirs = HashMap::new();
        beads_dirs.insert(repo_name.to_string(), beads_dir.clone());

        assert!(
            is_dolt_port_stale(&known_ports, &beads_dirs, repo_name),
            "port changed from 9999 to 12345 → should be stale"
        );

        // Same port → not stale
        known_ports.insert(repo_name.to_string(), 12345u16);
        assert!(
            !is_dolt_port_stale(&known_ports, &beads_dirs, repo_name),
            "port unchanged → not stale"
        );
    }

    #[test]
    fn repo_names_returns_connected() {
        let clients = HashMap::new();
        let mut paths = HashMap::new();

        // We can't easily create a BeadStore without a real database,
        // so test the names/paths logic separately
        paths.insert("alpha".to_string(), PathBuf::from("/tmp/alpha"));
        paths.insert("beta".to_string(), PathBuf::from("/tmp/beta"));

        let pool = RepoPool {
            clients,
            paths,
            beads_dirs: HashMap::new(),
            known_ports: HashMap::new(),
        };
        // No clients connected, so repo_names is empty
        assert!(pool.repo_names().is_empty());
    }
}
