# 9. AST-Aware Write Pipeline

Date: 2026-02-14

## Status

Accepted

## Context

Mache's write-back pipeline is byte-wise: splice content at `[StartByte:EndByte]`, re-ingest, done. This has two failure modes:

1. **Stale offsets**: After splicing node A, all sibling nodes in the same file have wrong `StartByte/EndByte`. A second write before re-ingest completes corrupts the source. The re-ingest fixes offsets but is slow (full re-parse). We need instant arithmetic offset shifting.

2. **No validation gate**: An agent can write broken syntax and mache splices it blindly. The source file becomes invalid. The agent doesn't know until compilation fails later.

Both problems have high-leverage fixes. The infrastructure (roaring bitmaps, tree-sitter) already exists in the codebase.

Depends on: ADR-0004 (serialized writes), ADR-0006 (syntax-aware protection).

## Decision

### Part 1: File→Nodes Roaring Index

Add a roaring bitmap index to MemoryStore mapping source file paths to the set of node IDs originating from each file. This replaces the O(N) full scan in `DeleteFileNodes` with O(k) bitmap iteration (where k = nodes in the file).

Fields added to MemoryStore:
- `fileToNodes map[string]*roaring.Bitmap` — FilePath → bitmap of internal node IDs
- `nodeIntID map[string]uint32` — Node.ID → internal bitmap uint32 ID
- `intToNodeID []string` — reverse: uint32 → Node.ID
- `nextIntID uint32` — monotonic counter

Wired into `AddNode()` (assign ID, set bit) and `DeleteFileNodes()` (iterate bitmap instead of full scan).

### Part 2: ShiftOrigins

New method on MemoryStore:
```go
func (s *MemoryStore) ShiftOrigins(filePath string, afterByte uint32, delta int32)
```

After a splice, called from the write-back callback BEFORE re-ingest. Iterates the file's bitmap, adjusts `StartByte`/`EndByte` for all nodes whose origin starts after the splice point. This makes sibling origins correct immediately, so a rapid second write uses correct offsets even if re-ingest hasn't finished.

### Part 3: Tree-sitter Validation Gate

New function in `internal/writeback/validate.go`:
```go
func Validate(content []byte, filePath string) error
```

Maps file extension to tree-sitter language, parses content, walks the AST root for `HasError()`. Returns a structured error with line/column if validation fails. Files with no known language pass through (no validation).

Inserted in the write-back callback BEFORE splice. On failure, returns error (NFS returns EIO, FUSE returns -EIO). Source file is untouched.

### Part 4: `_diagnostics/` Virtual Directory

For each node directory that has writable children, expose a virtual `_diagnostics/` subdirectory:
- `_diagnostics/last-write-status` — "ok\n" or last error message
- `_diagnostics/ast-errors` — tree-sitter ERROR node locations (if any)

Implementation: intercept in `ReadDir`/`Stat`/`Open` — when path contains `_diagnostics/`, generate content dynamically. Store last-write status in a `sync.Map` keyed by node path.

## Consequences

### Positive
- **Immediate offset correction**: ShiftOrigins makes rapid sequential writes safe without waiting for re-ingest.
- **Syntax safety**: Broken code is rejected at the filesystem boundary; source files remain valid.
- **Agent feedback loop**: Agents get immediate EIO on bad writes and can read `_diagnostics/` to understand why.
- **Performance**: Roaring bitmap index makes DeleteFileNodes O(k) instead of O(N).

### Negative
- **Memory overhead**: Roaring bitmaps and reverse-ID maps add memory proportional to node count.
- **Validation latency**: Tree-sitter parse on every write adds ~1-5ms per file.
- **Complexity**: The `_diagnostics/` virtual directory adds intercept logic to both FUSE and NFS paths.
