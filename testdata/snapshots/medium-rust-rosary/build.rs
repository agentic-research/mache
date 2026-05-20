use std::process::Command;

fn main() {
    // Embed git commit hash at compile time for version tracking.
    // Allows detection of stale MCP processes running old binaries.
    let hash = Command::new("git")
        .args(["rev-parse", "--short", "HEAD"])
        .output()
        .ok()
        .filter(|o| o.status.success())
        .map(|o| String::from_utf8_lossy(&o.stdout).trim().to_string())
        .unwrap_or_else(|| "unknown".to_string());

    println!("cargo:rustc-env=RSRY_BUILD_HASH={hash}");

    // Embed build timestamp (UTC ISO 8601)
    let timestamp = Command::new("date")
        .args(["-u", "+%Y-%m-%dT%H:%M:%SZ"])
        .output()
        .ok()
        .filter(|o| o.status.success())
        .map(|o| String::from_utf8_lossy(&o.stdout).trim().to_string())
        .unwrap_or_else(|| "unknown".to_string());
    println!("cargo:rustc-env=RSRY_BUILD_TIME={timestamp}");

    // Only re-run when HEAD changes (new commits), not on every build.
    println!("cargo:rerun-if-changed=.git/HEAD");
    println!("cargo:rerun-if-changed=.git/refs/heads/");

    // capnp codegen for the cloister↔rosary wire schema (rosary-6371e3).
    // Schema is vendored from cloister/wire/cloister.capnp; bump in sync
    // with cloister when that file evolves.
    //
    // Prereq: the `capnp` schema compiler must be on PATH. The `capnpc`
    // crate shells out to it — no external compiler, no Rust bindings.
    // Install: `brew install capnp` (macOS) or `apt-get install capnproto`
    // (Debian/Ubuntu) — both ship a recent-enough version.
    //
    // CI/contributors without capnp installed will see a clear error
    // from this build step rather than a downstream cryptic miss.
    println!("cargo:rerun-if-changed=schemas/cloister.capnp");
    capnpc::CompilerCommand::new()
        .src_prefix("schemas")
        .file("schemas/cloister.capnp")
        .run()
        .expect(
            "capnpc codegen for schemas/cloister.capnp failed — \
             ensure the `capnp` schema compiler is on PATH \
             (`brew install capnp` on macOS, `apt-get install capnproto` on Debian)",
        );
}
