# Findings and Verification

This document records the results of a "honesty check" on the project documentation, verifying if the documented features align with the actual codebase implementation.

## 1. Does it work how it says?

**Yes.** The core architecture described in `ARCHITECTURE.md` is faithfully implemented in the code.

-   **Ingestion Engine**: The `Engine` (`internal/ingest/engine.go`) correctly dispatches to specific walkers based on file extension.
-   **Walkers**:
    -   `SitterWalker` (`internal/ingest/sitter_walker.go`) handles source code using Tree-sitter.
    -   `JsonWalker` (`internal/ingest/json_walker.go`) handles JSON data using JSONPath.
    -   `SQLiteGraph` (`internal/graph/sqlite_graph.go`) and `StreamSQLiteRaw` (`internal/ingest/sqlite_loader.go`) handle SQLite databases.
-   **Graph Interface**: The `Graph` interface abstraction holds true, with `MemoryStore` and `SQLiteGraph` implementations.
-   **Filesystem Interface**: Both NFS (`internal/nfsmount`) and FUSE (`internal/fs`) implementations exist.

## 2. Is it tested / proved / repeatable?

**Yes.**

-   **Integration Tests**: `tests/integration_test.go` verifies the end-to-end flow:
    -   Schema inference from source code.
    -   Ingestion into the graph.
    -   NFS write-back pipeline (Validate -> Format -> Splice -> Update).
    -   Diagnostics (syntax errors, linting).
    -   Context awareness (imports).
-   **Unit Tests**: There is coverage across packages (`internal/lattice`, `internal/writeback`, `internal/graph`).
-   **Repeatability**: The `scripts/arena.sh` script provides a reproducible benchmark environment ("Mache Arena") to test agent interactions.

## 3. Discrepancies and Clarifications

While the high-level description is accurate, a few minor implementation details differ or are worth noting:

-   **SQLite Streaming**: The documentation mentions `StreamSQLite`, while the code primarily uses `StreamSQLiteRaw` for parallel ingestion (`ingestSQLiteStreaming` in `engine.go`). This is an optimization detail and doesn't contradict the architecture.
-   **Supported Extensions**: The original documentation listed a subset of supported languages. We have updated `ARCHITECTURE.md` to explicitly list all currently supported formats found in `internal/ingest/engine.go`:
    -   Go (`.go`)
    -   Python (`.py`)
    -   JavaScript/TypeScript (`.js`, `.ts`, `.tsx`)
    -   Rust (`.rs`)
    -   SQL (`.sql`)
    -   Configuration (`.tf`, `.hcl`, `.yaml`, `.yml`)
    -   Data (`.json`, `.db`)
-   **FUSE Support**: The documentation mentions FUSE as the default for Linux. The codebase (`internal/fs`) contains the implementation using `cgofuse`, confirming this capability exists alongside the NFS implementation.

## Conclusion

The documentation is largely honest and accurate. The updates to `ARCHITECTURE.md` have addressed the missing language support details. The system's core promises (AST overlay, write-back pipeline, schema-driven ingestion) are backed by concrete code and tests.
