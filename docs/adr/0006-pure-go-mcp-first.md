# ADR-0006: Pure Go, MCP-First — Remove CGO and FUSE

## Status

Proposed

## Context

Mache currently has three CGO dependencies:

1. **Tree-sitter** (~1,500 lines) — parses source code at mount time via go-tree-sitter
1. **FUSE** (~1,200 lines) — filesystem mount via cgofuse/fuse-t
1. **Ley-line FFI** (~150 lines) — direct C bindings to Rust staticlib (behind build tag)

CGO complicates cross-compilation, CI, deployment, and contributor onboarding. Fuse-t is macOS-only and requires a separate install. The ley-line FFI is already optional.

Meanwhile, ley-line-open (LLO) now provides:

- `leyline parse` — tree-sitter parsing in Rust, produces `.db` with `_ast` tables
- `leyline serve` — NFS/FUSE mount in Rust
- `leyline daemon` — UDS socket server for coordination
- ASTWalker in mache already consumes `_ast` tables (pure Go, no CGO)

The MCP serve path (`mache serve`) is the primary agent interface. It requires no mount, no FUSE, no CGO. It works today as pure Go when consuming a pre-built `.db`.

## Decision

Remove all CGO from mache in a single versioned release (v0.7.0):

1. **Delete tree-sitter**: Remove SitterWalker, sitter_flatten, language bindings, treesitter/elixir. LLO's `leyline parse` replaces all parsing. Mache's ASTWalker becomes the only walker.

1. **Delete FUSE backend**: Remove internal/fs/ (root.go, cgo_darwin.go). The NFS backend (internal/nfsmount/, pure Go) remains as an optional mount path. LLO's Rust NFS/FUSE is the primary filesystem interface.

1. **Delete ley-line FFI**: Remove internal/leyline/client.go CGO bindings. The UDS socket client (socket.go, pure Go) is the only communication path.

1. **Build with CGO_ENABLED=0**: Cross-compile to any GOOS/GOARCH without a C toolchain.

## Threads

### Thread 1: Move tree-sitter parsing to LLO

- Ensure `leyline parse` populates all tables mache needs: nodes, \_ast, \_source, node_refs, node_defs
- ASTWalker: add #match? predicate support via \_source byte ranges
- ASTWalker: ExtractCalls/ExtractQualifiedCalls via \_ast queries
- ASTWalker: ExtractContext + ExtractGoImports via \_ast/\_source
- Validate that all 28 language schemas work with LLO-produced .db files

### Thread 2: Remove FUSE backend

- Delete internal/fs/root.go and internal/fs/cgo_darwin.go
- Update cmd/mount.go to remove --backend fuse option (NFS becomes only Go mount)
- Remove fuse-t from build dependencies and CI
- Update README and docs

### Thread 3: Bundle leyline in mache release

- mache CI builds LLO leyline binary (Rust cross-compile)
- Release tarball includes both mache (Go) and leyline (Rust)
- Auto-detect tiers: mache-only / mache+llo / mache+ll
- mache calls leyline parse under the hood when .db doesn't exist

### Thread 4: Remove ley-line CGO FFI

- Delete internal/leyline/client.go (CGO bindings)
- UDS socket (socket.go) is the only LL communication path
- Remove CGO build tags and flags from Taskfile/CI

### Thread 5: Final CGO removal

- Set CGO_ENABLED=0 in CI and release builds
- Remove CGO_CFLAGS/CGO_LDFLAGS from Taskfile.yml
- Update CLAUDE.md build instructions
- Tag v0.7.0

## Consequences

- Mache binary drops from ~45MB to ~25MB (no C libraries linked)
- Cross-compilation works for linux/darwin × amd64/arm64 without C toolchain
- Contributors don't need fuse-t, tree-sitter grammars, or Xcode command line tools
- Users who want filesystem browsing use LLO's mount or the NFS backend
- ~2,500 lines deleted from mache
- Breaking change: --backend fuse no longer available

## Alternatives Considered

- **Keep FUSE, gate behind build tag**: Adds complexity for a backend nobody uses except the author. NFS does the same thing without CGO.
- **Keep tree-sitter in mache, make ASTWalker optional**: Inverts the dependency — mache should consume, not parse. LLO is the parser.
- **Gradual removal across multiple releases**: Extends the maintenance burden. One clean cut is simpler.
