//! End-to-end test for `rsry mcp --ipc-socket` running inside the built
//! distroless image (rosary:0.2.0).
//!
//! Skipped if docker or the image are unavailable. `cargo test` continues
//! to pass on dev machines without docker.
//!
//! What this exercises that the in-process test (serve::ipc::tests::
//! roundtrip_rsry_status) does not:
//!
//!   - The actual built image, not a debug binary
//!   - The musl static binary inside chainguard/static (not the host build)
//!   - cluster.capnp's literal launch args (`mcp --ipc-socket <path>`)
//!
//! ## Topology
//!
//! Both ends of the UDS live INSIDE the container — the server runs as
//! `mcp --ipc-socket /tmp/rosary.sock`, the client is `docker exec`'d
//! against the same container to run `rsry ipc-call --ipc-socket
//! /tmp/rosary.sock --tool rsry_status`. Same VM, same process tree
//! ancestry → no Docker Desktop host→container AF_UNIX boundary.
//!
//! This is the "mid-step" test: covers bytes-on-the-wire correctness for
//! capnp ToolCall/ToolResult without requiring cloister-companion (still
//! a stub at cloister/src/manifest/backends/uds-forward.ts:52). When
//! companion lands, a sibling test can swap `docker exec` for "cloister
//! → companion → rosary" through a shared volume — same bytes, more
//! hops.

use std::process::Command;
use std::time::{Duration, Instant};

const IMAGE_TAG: &str = "rosary:0.2.0";
/// UDS path inside the container. No volume mount — both server and client
/// live inside the container's tmpfs.
const CONTAINER_SOCK: &str = "/tmp/rosary.sock";

/// Container platform. Defaults to the host architecture so the test runs
/// on whatever arch a contributor built the image for; override via
/// `ROSARY_E2E_PLATFORM=linux/amd64` (etc.) if you built `rosary:0.2.0`
/// for a non-host arch and have binfmt/QEMU configured.
fn platform() -> String {
    if let Ok(p) = std::env::var("ROSARY_E2E_PLATFORM") {
        return p;
    }
    match std::env::consts::ARCH {
        "aarch64" => "linux/arm64".to_string(),
        "x86_64" => "linux/amd64".to_string(),
        other => format!("linux/{other}"),
    }
}

fn docker_available() -> bool {
    Command::new("docker")
        .arg("info")
        .output()
        .map(|o| o.status.success())
        .unwrap_or(false)
}

fn image_exists(tag: &str) -> bool {
    // `docker image inspect` only sees legacy classic-storage images;
    // buildx (containerd storage) needs `docker images --format`.
    let out = match Command::new("docker")
        .args(["images", "--format", "{{.Repository}}:{{.Tag}}"])
        .output()
    {
        Ok(o) if o.status.success() => o.stdout,
        _ => return false,
    };
    String::from_utf8_lossy(&out)
        .lines()
        .any(|line| line == tag)
}

/// Drop-guard that force-removes a container by name on test exit.
struct ContainerGuard(String);

impl ContainerGuard {
    fn new(name: &str) -> Self {
        let _ = Command::new("docker").args(["rm", "-f", name]).output();
        Self(name.to_string())
    }
}

impl Drop for ContainerGuard {
    fn drop(&mut self) {
        let _ = Command::new("docker").args(["rm", "-f", &self.0]).output();
    }
}

/// Outcome of one `ipc-call` invocation. Both `Ok` and `Err` variants
/// represent successful WIRE round-trips — they differ in whether the
/// handler returned `Ok` (JSON body) or `Err` (encoded as `isError=true`
/// with `text="Error: ..."`). Either proves the wire works.
#[derive(Debug)]
enum WireOutcome {
    Ok(String),    // exit 0, stdout = JSON
    Error(String), // exit 1, stdout = "Error: ..."
}

/// Probe via `docker exec`. Distinguishes "wire failed entirely" (no
/// stdout, e.g. server not yet bound, exec failed) from "wire returned
/// a result the handler couldn't fulfil" (stdout populated, exit 1).
/// Only the former is retryable.
fn try_ipc_call(container: &str, tool: &str) -> Result<WireOutcome, String> {
    let out = Command::new("docker")
        .args([
            "exec",
            container,
            "/usr/bin/rsry",
            "ipc-call",
            "--ipc-socket",
            CONTAINER_SOCK,
            "--tool",
            tool,
        ])
        .output()
        .map_err(|e| format!("spawn docker exec: {e}"))?;
    let stdout = String::from_utf8_lossy(&out.stdout).to_string();
    let stderr = String::from_utf8_lossy(&out.stderr).to_string();
    if stdout.trim().is_empty() {
        // No payload at all — connect failed or server crashed before responding.
        return Err(format!(
            "no ipc-call output (exit {}, stderr: {})",
            out.status,
            stderr.trim()
        ));
    }
    if out.status.success() {
        Ok(WireOutcome::Ok(stdout))
    } else {
        Ok(WireOutcome::Error(stdout))
    }
}

#[test]
fn capnp_uds_roundtrip_against_built_image() {
    if !docker_available() {
        eprintln!("[skip] docker unavailable");
        return;
    }
    if !image_exists(IMAGE_TAG) {
        eprintln!("[skip] {IMAGE_TAG} not built — run `task image` first");
        return;
    }

    let container = format!("rosary-ipc-e2e-{}", std::process::id());
    let _guard = ContainerGuard::new(&container);
    let platform_arg = platform();

    let run = Command::new("docker")
        .args([
            "run",
            "-d",
            "--rm",
            "--name",
            &container,
            "--platform",
            &platform_arg,
            // Run the server with the in-container UDS path.
            IMAGE_TAG,
            "mcp",
            "--ipc-socket",
            CONTAINER_SOCK,
        ])
        .output()
        .expect("docker run");
    assert!(
        run.status.success(),
        "docker run failed: {}",
        String::from_utf8_lossy(&run.stderr),
    );

    // Poll until the wire round-trips. Both Ok(JSON) and Error("Error: …")
    // outcomes prove the wire works — the container has no rosary.toml so
    // rsry_status returns the handler-error path. That's still a valid
    // capnp round-trip and exercises the isError encoding contract.
    let deadline = Instant::now() + Duration::from_secs(15);
    let outcome = loop {
        match try_ipc_call(&container, "rsry_status") {
            Ok(o) => break o,
            Err(e) if Instant::now() >= deadline => {
                let logs = Command::new("docker")
                    .args(["logs", &container])
                    .output()
                    .map(|o| {
                        format!(
                            "stdout:\n{}\nstderr:\n{}",
                            String::from_utf8_lossy(&o.stdout),
                            String::from_utf8_lossy(&o.stderr),
                        )
                    })
                    .unwrap_or_else(|e| format!("(docker logs failed: {e})"));
                panic!(
                    "wire never round-tripped within 15s. Last error: {e}\n--- container logs ---\n{logs}"
                );
            }
            Err(_) => std::thread::sleep(Duration::from_millis(200)),
        }
    };

    match outcome {
        WireOutcome::Ok(s) => {
            let trimmed = s.trim();
            let parsed: serde_json::Value = serde_json::from_str(trimmed).unwrap_or_else(|e| {
                panic!("happy-path stdout is not JSON: {e}\noutput: {trimmed}")
            });
            assert!(parsed.is_object(), "expected JSON object, got: {trimmed}");
            eprintln!("[ok] wire round-trip (happy): {trimmed}");
        }
        WireOutcome::Error(s) => {
            let trimmed = s.trim();
            // Server wraps handler Err as text="Error: {e}", isError=true.
            // Asserting on the "Error:" prefix pins the wire's error-encoding
            // contract — client decoded the union variant and the isError flag.
            assert!(
                trimmed.starts_with("Error:"),
                "expected 'Error: …' wire-error text, got: {trimmed}",
            );
            eprintln!("[ok] wire round-trip (handler error encoded): {trimmed}");
        }
    }
}
