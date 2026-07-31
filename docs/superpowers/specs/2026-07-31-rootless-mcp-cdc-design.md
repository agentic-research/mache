# Rootless Codex MCP and opt-in CDC design

## Decision

Mache must continue refusing an unscoped shared HTTP daemon: a rootless
session must never inherit launchd's current working directory. A stdio
process, however, has an explicit positional source in its command line.
When MCP Roots are unavailable, Mache will use that source only for stdio;
this preserves the documented `mache serve --stdio .` invocation and keeps
the shared daemon safe.

Documentation will prefer `mache serve --stdio --path .` because it makes the
fallback unambiguous. It will state that a Roots failure occurs before any
repository scan, so it is not evidence that a monorepo is too large.

CDC remains opt-in. Add `mache serve --cdc` and pass it to Mache's managed
Leyline daemon, which activates CDC before it publishes the first arena
snapshot. Do not activate CDC for a pre-built database or an external
`serve --control` daemon: those lifecycles belong to their operators.

## Tests

- A rootless stdio session with one explicit directory source selects that
  directory; rootless HTTP still returns the existing safe diagnostic.
- Serve wiring includes `--cdc` only when requested, and the existing daemon
  lifecycle test exercises the managed child path.
- Documentation command tests accept the corrected stdio examples.

## Non-goals

- Do not make CDC implicit or change the authoritative `nodes.record` path.
- Do not teach the shared HTTP daemon to guess a repository without MCP Roots.
- Do not add a new CDC implementation in Mache; Leyline owns the derived
  index and Mache only opts its managed daemon into the published interface.
