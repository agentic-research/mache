# 15. Syntax-Aware Write Protection

Date: 2026-02-13 (proposal) · Updated 2026-05-10 to reflect what shipped (renumbered from ADR-0006 to resolve a numbering collision with `0006-pure-go-mcp-first.md`)

## Status

Accepted and implemented. The validate → format → splice pipeline lives in `internal/writeback/` and is on by default on every write through the mount and the `write_file` MCP tool. See [ADR-0009](0009-ast-aware-write-pipeline.md) for the full write pipeline framing this is a part of.

## Context

When AI agents (or humans) refactor code via mache, they modify the virtual `source` files exposed by the filesystem. Mache splices these edits back into the original source code.

Agents routinely introduce syntax errors (missing braces, invalid keywords, half-applied refactors). Without a validation gate, the file on disk would become syntactically invalid; the user would only discover this when they ran the compiler later. For an autonomous agent loop, this delayed feedback is fatal — the agent thinks it succeeded ("I wrote the file") but actually broke the build.

The original framing of this ADR proposed a `--strict` flag on a FUSE-backed mount that would `EIO` on invalid writes. Since that proposal, two architectural shifts changed the surface:

1. **NFS replaced FUSE** ([ADR-0006: Pure Go, MCP-First](0006-pure-go-mcp-first.md), v0.7.0). The error path is no longer `fuse.EIO`; it's whatever NFS surfaces through `go-nfs` + `billy`.
1. **Validation became unconditional and non-blocking.** Rather than rejecting invalid writes at the filesystem boundary (which fights editors and tools that save intermediate broken states), mache accepts every write but routes invalid content into Draft state, surfaced via the `_diagnostics/` virtual directory.

## Decision

The actual implementation (in `internal/writeback/validate.go`, called from the NFS write path and the `write_file` MCP tool) is:

1. **Buffer the proposed content.** When a write closes on a `source` file (NFS Release, or MCP `write_file` call), mache holds the new bytes before touching the original source.
1. **Tree-sitter parse + error check.** `Validate` parses the buffer with the language-specific grammar (selected via `internal/lang/lang.go`) and walks the AST for `ERROR` / `MISSING` nodes.
1. **Format (where supported).** Valid content is then passed through the language-appropriate formatter — `gofumpt` for Go, `hclwrite` for HCL/Terraform. Other languages pass through unchanged.
1. **Gate:**
   - **If valid:** the write is spliced into the original source via `internal/writeback/splice.go` (byte-range replacement at the construct's origin), and the node's `Origins` are shifted; no re-ingest.
   - **If invalid:** the write is saved as a **Draft** at the same node path. The agent's next read of `source` returns the draft content (so its mental model isn't broken). The error surfaces via `<node>/_diagnostics/last-write-status`, which agents are expected to check after every write.

This is always on. There is no `--strict` flag — strict-by-default is the contract.

## Consequences

### Positive

- **Immediate feedback for agents.** `_diagnostics/last-write-status` reports validation outcomes; agents can self-correct in the next turn without waiting for a compile loop.
- **Repo integrity.** The original source file is never corrupted by invalid syntax — drafts live alongside, never replace.
- **Editor compatibility preserved.** Because writes are accepted (just routed to draft when invalid), editors that save intermediate states don't see hard "Unable to save" errors.
- **AST-aware Δ.** The splice happens at the node's exact byte range; the rest of the file is byte-identical. No re-ingest, no full-file rewrite.

### Negative

- **Per-write parsing cost.** Tree-sitter is fast (microseconds for typical edits) but it's still per-write work. Mitigated in practice by the small size of construct-level edits.
- **Drafts can accumulate.** If an agent writes invalid content and never circles back, the draft persists. The Arena's Level 5 exercise specifically tests that agents read `last-write-status` to catch this.
- **Per-language formatter coverage is uneven.** Validation works for all 28 registered languages; auto-formatting is currently Go + HCL/Terraform only. Other languages validate-then-pass-through. See `internal/writeback/format.go`.

## Implementation Reference

- `internal/writeback/validate.go` — tree-sitter parse + error walk.
- `internal/writeback/format.go` — gofumpt / hclwrite gating.
- `internal/writeback/splice.go` — byte-range source replacement + `ShiftOrigins`.
- `internal/nfsmount/graphfs.go::writeFile` — NFS write path, `written` flag distinguishes truncate-only from real-write close cycles.
- `cmd/serve_handlers.go::write_file` — MCP tool entry point.
- Diagnostics surface: `<node>/_diagnostics/last-write-status` virtual file.
- Arena Level 5 ([`scripts/arena.sh`](../../scripts/arena.sh)) is the end-to-end agent-facing test for this behavior.
