# Rootless Codex MCP and opt-in CDC design

## Decision

Mache must continue refusing an unscoped shared HTTP daemon: a rootless
session must never inherit launchd's current working directory. A stdio
process, however, has an explicit positional source in its command line.
When MCP Roots are unavailable, Mache will use that source only for stdio;
this preserves the documented `mache serve --stdio .` invocation and keeps
the shared daemon safe.

Documentation will prefer `mache serve --stdio --path .` because it makes the
fallback unambiguous and associates that explicit path with a managed daemon's
`--source`. A positional pre-built database remains authoritative even when
`--path` is present. Documentation will state that a Roots failure occurs
before any repository scan, so it is not evidence that a monorepo is too
large.

CDC remains opt-in. Add `mache serve --cdc` and pass it to Mache's managed
Leyline daemon only when the selected graph is source-backed, which activates
CDC before it publishes the first arena snapshot. Do not activate CDC for a
pre-built database or an external `serve --control` daemon: those lifecycles
belong to their operators. A pre-built database may still cause an on-demand
daemon launch, but that launch receives neither the database as `--source` nor
Mache's `--cdc` request.

Leyline already supplies symbol-resolution evidence through `node_refs`,
`node_defs`, `_lsp_refs`, `_lsp_defs`, and the append-only binding log.
Mache currently consumes only the `node_refs` call/reference projection through
the daemon's `find_callers` and `find_callees` operations. It will expose that
missing consumer operation as a function/symbol-centric `get_dataflow` MCP
tool. It resolves a supplied function or symbol, then returns a bounded
interprocedural reference/call graph whose edges are explicitly labeled
`node_ref`. This is inspectable reference-flow evidence for an LLM, not a
claim of LSP-confirmed binding, SSA, taint analysis, or full data dependence.
Production `node_refs` identify a caller through its non-directory `source`
child. For traversal and construct-level output, Mache maps that shape to its
parent only when the parent resolves as a graph directory; backends that
already return construct IDs remain unchanged.

LSP-confirmed edge provenance requires a separate Leyline UDS operation; it
is deliberately outside this Mache-only PR rather than being inferred from
unavailable data.

The response is deterministic because its roots, nodes, and edges are sorted,
but it is not a CDC comparison surface. A source-backed serve auto-parses a
frozen temporary SQLite graph, and `get_dataflow` reads that graph rather than
the managed daemon arena. Equality between CDC-off and CDC-on flow responses
therefore does not validate CDC. Snapshot generation/root remains separate
through `get_sheaf_status` when a daemon is reachable, but current cache state
is also not a controlled CDC contrast. Mache can validate only the managed
launch boundary (`--source` in both isolated runs and `--cdc` in the opted-in
run); semantic CDC output must be evaluated through Leyline's own command/API.

## Tests

- A rootless stdio session with one explicit directory source selects that
  directory; rootless HTTP still returns the existing safe diagnostic.
- Serve wiring includes `--cdc` only when requested, and the existing daemon
  lifecycle test exercises the managed child path.
- Documentation command tests accept the corrected stdio examples.
- A fixture with a caller and callee proves that `get_dataflow` returns
  bounded `node_ref`-typed edges for a symbol root.
- A fresh managed-daemon run logs its argv (or uses the lifecycle argv test)
  to prove that both source-backed invocations receive `--source` and only the
  CDC-on invocation receives `--cdc`; a positional database and external
  daemon never opt into that flag.

## Non-goals

- Do not make CDC implicit or change the authoritative `nodes.record` path.
- Do not teach the shared HTTP daemon to guess a repository without MCP Roots.
- Do not add a new CDC implementation in Mache; Leyline owns the derived
  index and Mache only opts its managed daemon into the published interface.
- Do not call the first flow tool taint analysis, data dependence, or SSA.
