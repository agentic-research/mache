# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in Mache, please report it responsibly.

**Do not open a public issue.** Instead, email the maintainers or use [GitHub's private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability).

We will acknowledge receipt within 72 hours and aim to provide a fix or mitigation plan within 30 days.

## Scope

Mache mounts read-only FUSE filesystems. Security concerns include:

- Path traversal in schema template rendering
- Unintended data exposure through mounted filesystems
- Denial of service via malformed schemas or data sources
