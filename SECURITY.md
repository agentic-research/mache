# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in Mache, please report it responsibly.

**Do not open a public issue.** Instead, email the maintainers or use [GitHub's private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability).

We will acknowledge receipt within 72 hours and aim to provide a fix or mitigation plan within 30 days.

## Scope

Mache mounts read-only filesystems (NFS or FUSE). Security concerns include:

- Path traversal in schema template rendering
- Unintended data exposure through mounted filesystems
- Denial of service via malformed schemas or data sources

## NFS Backend

The NFS server binds to `127.0.0.1` (localhost only) on an ephemeral port with
no authentication (`NullAuthHandler`). Any local process can connect to the port
and read the mounted data. This is standard for local NFS mounts but means
**mache should not be used on shared multi-tenant hosts** without additional
network isolation (e.g. containers, network namespaces).

## Snapshot Mode

`--snapshot` copies the data source to a temporary directory before mounting.
The copy is **not atomic** — if interrupted mid-copy, a partial snapshot may
remain in `$TMPDIR/mache/snapshots/`. Use `mache clean` to remove orphaned
snapshots. Writable snapshots are preserved on clean unmount so agent edits
survive; read-only snapshots are automatically deleted.
