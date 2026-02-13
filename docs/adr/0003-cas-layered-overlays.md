# 3. Content-Addressed Storage & Layered Overlays

Date: 2026-02-12
Status: Proposed

## Context

Mache currently mounts views of data that are ephemeral or directly tied to a mutable backend (SQLite). To support advanced use cases like versioning, deduplication, and efficient reorganization of views, we need a storage model that separates **Data Identity** (the content) from **Data Organization** (the path).

## Decision

We will implement a **Content-Addressed Storage (CAS)** backing store with **Layered Overlays**.

### Core Mechanism

1.  **CAS Blob Store:**
    *   All file content is hashed (SHA-256).
    *   Content is stored in a flat address space: `/data/cas/sha256:<hash>`.
    *   This ensures zero duplication for identical files.

2.  **Topology Layers:**
    *   The "Directory Tree" presented to the user is a lightweight overlay.
    *   Files in the tree are effectively "hard links" or pointers to the CAS blobs.
    *   Reorganizing a directory (e.g., moving a file) is an O(1) metadata operation; the content never moves.

### Architecture

```
Physical Storage:
/var/lib/mache/blobs/
  sha256:abc1234... (The actual JSON/Byte content)
  sha256:def5678...

Logical Overlay (FUSE View):
/mnt/mache/
  by-date/
    2024/doc.json -> points to sha256:abc1234...
  by-author/
    alice/doc.json -> points to sha256:abc1234...
```

## Consequences

### Positive

1.  **Deduplication:** The same file appearing in multiple views (e.g., sorted by Date AND sorted by Author) takes up no extra storage.
2.  **Versioning:** Updating a file creates a new blob. Old views can point to the old blob (Snapshotting), new views point to the new blob.
3.  **Atomic Updates:** Switching a view from "Version A" to "Version B" is an atomic pointer swap.

### Negative

1.  **GC Complexity:** We need a Garbage Collector to clean up blobs that are no longer referenced by any topology layer.
2.  **Storage Overhead:** The CAS index requires maintenance.

## Implementation Plan

1.  **BlobStore Interface:** Define a generic interface for `Put(content) -> Hash` and `Get(Hash) -> content`.
2.  **Graph Update:** Update `internal/graph` to store `ContentHash` instead of raw `[]byte` or `dbID` where appropriate.
3.  **FUSE Resolve:** Update the FUSE `Read` handler to fetch from BlobStore using the node's hash.
