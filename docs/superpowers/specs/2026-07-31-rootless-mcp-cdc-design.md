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

Leyline already supplies symbol-resolution evidence through `node_refs`,
`node_defs`, `_lsp_refs`, `_lsp_defs`, and the append-only binding log.
Mache currently consumes only the `node_refs` call/reference projection through
the daemon's `find_callers` and `find_callees` operations. It will expose that
missing consumer operation as a function/symbol-centric `get_dataflow` MCP
tool. It resolves a supplied function or symbol, then returns a bounded
interprocedural reference/call graph whose edges are explicitly labeled
`node_ref`. This is inspectable reference-flow evidence for an LLM, not a
claim of LSP-confirmed binding, SSA, taint analysis, or full data dependence.

LSP-confirmed edge provenance requires a separate Leyline UDS operation; it
is deliberately outside this Mache-only PR rather than being inferred from
unavailable data.

The response is deterministic because its roots, nodes, and edges are sorted;
compare the complete canonical `get_dataflow` JSON for CDC-off and CDC-on runs
before treating any latency measurement as meaningful. The MCP response does
not carry elapsed time or a result digest. A validation client may hash the
canonical response externally, but that hash is test evidence rather than a
wire contract. Snapshot generation/root remains separate through
`get_sheaf_status` when a daemon is reachable; adding it to the flow response
needs a distinct daemon metadata operation. A frozen source snapshot or a
short-lived stdio session may truthfully report the daemon unavailable, so this
status must not be mistaken for a CDC behavioral comparison.

## Tests

- A rootless stdio session with one explicit directory source selects that
  directory; rootless HTTP still returns the existing safe diagnostic.
- Serve wiring includes `--cdc` only when requested, and the existing daemon
  lifecycle test exercises the managed child path.
- Documentation command tests accept the corrected stdio examples.
- A fixture with a caller and callee proves that `get_dataflow` returns
  bounded `node_ref`-typed edges for a symbol root.
- CDC-on and CDC-off runs over the same fixture have equal canonical flow
  responses (or equal externally calculated hashes of those responses).
- A fresh managed-daemon run logs its argv (or uses the lifecycle argv test)
  to prove that only the CDC-on invocation receives `--cdc`; existing or
  external daemons are deliberately outside that proof.

## Non-goals

- Do not make CDC implicit or change the authoritative `nodes.record` path.
- Do not teach the shared HTTP daemon to guess a repository without MCP Roots.
- Do not add a new CDC implementation in Mache; Leyline owns the derived
  index and Mache only opts its managed daemon into the published interface.
- Do not call the first flow tool taint analysis, data dependence, or SSA.
