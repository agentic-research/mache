# 4. Wait-Free MVCC Memory Ledger for POSIX/SQL Graph Projection

Date: 2026-02-11
Status: Proposed
Depends-On: ADR-0001 (FUSE bridge), ADR-0002 (Declarative Topology)
Enables: ADR-0003 (CAS & Layered Overlays)

## Context

ADR-0002 defines a Declarative Topology Schema that maps data to filesystem hierarchy. That ADR deliberately defers _how_ the data is held in memory -- it only defines the _shape_. We now need a runtime data layer that:

1. **Parses massive, chaotic JSON feeds** (e.g., grype-db vulnerability data) into a semantic graph.
2. **Serves concurrent FUSE reads and SQLite queries** without blocking.
3. **Supports bulk-ingest updates** (new feed versions) without stalling readers.
4. **Scales to 10M+ entities** without GC pauses or pointer-chasing.

### Why not the obvious approaches?

| Alternative | Why Rejected |
|---|---|
| **Go `map` + `sync.RWMutex`** | GC pressure at 10M+ heap-allocated nodes. Write-lock blocks all readers during ingest. Acceptable for prototyping (see Phased Approach below), not for production. |
| **Embedded KV (Badger/Pebble)** | KV stores _can_ do graph traversals (Dgraph proves this on Badger). However, every read requires deserialization from `[]byte`, and every write requires serialization. For our hot path (FUSE `Readdir` iterating thousands of entries), the encode/decode overhead dominates. The real argument is eliminating serialization, not asymptotic complexity. |
| **Neo4j / JanusGraph** | JVM process, network protocol, serialization. Correct for multi-service architectures, overkill for a single-process FUSE mount. |
| **Standard FUSE in-process state** | No inherent problem with in-process state. The question is _which_ in-process data structure. |

## Decision

We will implement the runtime data layer as a **pointerless Entity Component System (ECS)** using dense arrays backed by `mmap`, with an **RCU-style epoch model** for concurrent access.

### Architecture Overview

```
                    ┌─────────────────────────┐
                    │   Source Data (JSON)     │
                    └───────────┬─────────────┘
                                │ parse + ingest
                    ┌───────────▼─────────────┐
                    │   Writer Goroutine       │
                    │   (single, serialized)   │
                    │   Builds Epoch N+1       │
                    └───────────┬─────────────┘
                                │ atomic pointer swap
                    ┌───────────▼─────────────┐
                    │   Epoch Registry         │
                    │   current: *Epoch        │
                    │   (128-byte aligned)     │
                    └──┬──────────────────┬────┘
                       │                  │
              ┌────────▼───────┐ ┌────────▼───────┐
              │ FUSE Readers   │ │ SQLite VTable   │
              │ (wait-free)    │ │ (wait-free)     │
              │ Acquire epoch, │ │ Acquire epoch   │
              │ read dense     │ │ at tx start,    │
              │ arrays, release│ │ hold for query  │
              └────────────────┘ └─────────────────┘
```

### 1. Entity Component System (ECS) with Dense Arrays

Entities (CVEs, packages, advisories) are integer IDs. Components (severity, description, edges) are stored in **type-homogeneous dense arrays** indexed by entity ID via sparse sets.

```
Sparse Set (per component type):
  sparse[entity_id] → dense_index    (or INVALID)
  dense[dense_index] → component_data

Lookup: O(1) — two array accesses.
Iteration: O(n) over dense array — sequential, cache-friendly.
```

**Why "pointerless":** Dense arrays contain only fixed-size value types and integer offsets — no Go heap pointers. This means:
- The Go GC does not scan these regions (no GC pressure).
- No pointer-chasing during traversal (cache-line-friendly sequential access).
- All "references" between entities are integer IDs resolved through sparse set lookups.

**The cost:** Every entity-to-entity reference requires an explicit sparse set lookup (4-5 operations) instead of a pointer dereference (1 operation). This is the fundamental trade-off: GC immunity and cache locality vs. per-access overhead.

### 2. Concurrency Model: RCU-Style Epochs

We use a Read-Copy-Update (RCU) pattern, **not** a general MVCC system.

**Precise guarantees:**
- **Read path: Wait-free.** Readers perform one atomic load to acquire the current epoch pointer, then read exclusively from immutable data. No CAS loops, no retries, no locks. Every reader completes in bounded steps.
- **Write path: Serialized.** A single writer goroutine builds each new epoch sequentially. Writes are not concurrent. This eliminates write-write contention and cache-line false sharing on the write side.
- **Epoch swap: Lock-free.** The writer performs a single `atomic.StorePointer` to publish a new epoch. Readers that have already acquired epoch N continue on N; new readers see N+1.

**What this is NOT:**
- It is not "wait-free" in the unqualified sense (writes are serialized).
- It is not MVCC in the database sense (no transaction IDs, no read-your-writes).
- It is not a general concurrent data structure (single writer is a hard constraint).

### 3. Epoch Reclamation Protocol

This is the hardest problem in the design. When can the writer reclaim epoch N's memory?

**Protocol: Epoch-Based Reclamation (Fraser, 2004)**

```
Global:
  current_epoch: uint64 (atomically updated by writer)
  retired_epochs: list of (epoch_ptr, epoch_number)

Per-reader (goroutine-local):
  active_epoch: uint64 (set on entry, cleared on exit)

Reader entry:
  reader.active_epoch = atomic.LoadUint64(&current_epoch)
  // memory barrier implicit in atomic load

Reader exit:
  reader.active_epoch = 0

Writer reclamation:
  min_active = min(r.active_epoch for r in readers where r.active_epoch != 0)
  for each retired_epoch where epoch_number < min_active:
    munmap(retired_epoch)
```

**Critical constraint:** A FUSE `Readdir` iterating 100K entries, or a SQLite query joining virtual tables, holds an epoch reference for the entire operation. The writer MUST NOT reclaim any epoch with active readers. This means:

- Under sustained read load, up to 3 epochs may coexist (N-1 draining, N current, N+1 building).
- Pathological slow readers can prevent reclamation. **Mitigation:** epoch hold timeout (configurable, default 30s) after which the reader is forcibly detached and must re-acquire.
- Memory budget must account for 3x epoch size, not 2x.

### 4. Memory-Mapped Backing Store

Dense arrays are allocated via `mmap(MAP_ANONYMOUS | MAP_PRIVATE)` for the hot path. Optionally file-backed for persistence (see Crash Semantics).

**Binary Layout (per epoch):**

```
Epoch File/Region Layout:
┌──────────────────────────────────┐
│ Header (4 KB, page-aligned)      │
│   magic: [8]byte "MACHE\x00\x01"│
│   version: uint32                │
│   epoch_number: uint64           │
│   entity_count: uint64           │
│   component_table_offset: uint64 │
│   string_table_offset: uint64    │
│   checksum: [32]byte (SHA-256)   │
├──────────────────────────────────┤
│ Component Table (index)          │
│   For each component type:       │
│     type_id: uint32              │
│     sparse_offset: uint64        │
│     dense_offset: uint64         │
│     element_size: uint32         │
│     count: uint64                │
├──────────────────────────────────┤
│ Sparse Arrays                    │
│   (page-aligned per component)   │
├──────────────────────────────────┤
│ Dense Arrays                     │
│   (page-aligned per component)   │
│   Fixed-size components inline   │
│   Variable-length: uint32 offset │
│     into string table            │
├──────────────────────────────────┤
│ String Table                     │
│   Length-prefixed byte sequences  │
│   (uint32 len + []byte data)     │
│   Append-only within an epoch    │
└──────────────────────────────────┘
```

**Variable-length data** (CVE descriptions, package names): stored in the String Table as length-prefixed byte sequences. Dense array slots contain a `uint32` offset into the String Table. This keeps dense arrays fixed-stride for cache-friendly iteration.

**Byte order:** Little-endian (Apple Silicon native). Not portable across architectures — acceptable for a single-machine FUSE mount.

**Alignment:** All arrays page-aligned (16 KB on Apple Silicon). Component elements aligned to their natural alignment within dense arrays.

### 5. Apple Silicon Considerations

**Cache line geometry:** M1/M2/M3 use 128-byte cache line pairs (two 64-byte lines sharing coherence state in the MESI protocol). The epoch pointer and any writer-mutated metadata MUST be padded to 128 bytes:

```go
type EpochRegistry struct {
    current unsafe.Pointer // *Epoch
    _pad    [120]byte      // Pad to 128 bytes for M-series cache line pair
}
```

**Page size:** Apple Silicon uses 16 KB pages (not 4 KB). mmap allocations and alignment must respect this. `madvise` with `MADV_WILLNEED` for read-ahead on cold start.

**TLB budget:** M-series L1 dTLB has ~160 entries, L2 TLB ~2048 entries. At 16 KB pages, 2048 TLB entries cover 32 MB. A 640 MB component array will experience TLB misses on random access. **Mitigation:** Entity allocation should favor spatial locality (entities likely to be traversed together should have adjacent IDs). This is a soft optimization, not a hard requirement.

### 6. Crash Semantics

**Decision: Ephemeral by default, snapshotable on demand.**

The mmap'd ECS is the **derived** store, not the source of truth. The source JSON feeds are the authoritative data. Therefore:

- **Normal operation:** `MAP_ANONYMOUS`. Process crash = full rebuild from source data on restart.
- **Rebuild time budget:** At 10M entities, target < 30 seconds cold rebuild from JSON. If this is exceeded, optimize the parser before adding persistence complexity.
- **Optional snapshots:** On clean shutdown (or periodic timer), write the current epoch to a file-backed mmap with `msync(MS_SYNC)`. On restart, load the snapshot and verify its checksum. If valid, serve immediately while rebuilding in background. If invalid, discard and rebuild from source.
- **No WAL, no journaling.** The source feeds are the journal. Adding write-ahead logging for a derived, read-only projection is unjustified complexity.

This means:
- Crash during epoch compilation: epoch N-1 (if snapshotted) or full rebuild.
- No torn-write concerns for the hot path (anonymous mmap is process-private).
- Snapshot writes use atomic rename (`write to .tmp`, `fsync`, `rename`) for crash safety.

### 7. SQLite Virtual Table Integration

SQLite queries acquire an epoch reference at virtual table `xBegin` and release at `xEnd`.

```
SQLite query lifecycle:
  xBegin:  epoch_ref = acquire_epoch()
  xFilter: iterate dense arrays using epoch_ref
  xNext:   continue iteration
  xEnd:    release_epoch(epoch_ref)
```

**Risk:** Long-running SQL queries hold epoch references, blocking reclamation. Same mitigation as FUSE: configurable timeout with forced detach. SQLite queries that exceed the timeout receive `SQLITE_BUSY` and must retry.

### String Allocation at the SQLite Boundary

When SQLite's `xColumn` requests a variable-length string (e.g., a CVE description) from the mmap'd String Table, we hit a memory safety fork:

- **Option A (Zero-Copy):** Pass the `[]byte` slice backed by the mmap region directly to SQLite. Risk: If the SQLite driver (`modernc.org/sqlite` or CGo bridge) delays copying the buffer or holds the reference beyond our epoch timeout, epoch reclamation causes a `SIGBUS` on unmapped memory.
- **Option B (Deep Copy):** Allocate a new Go `string` on the heap and copy bytes into it before handing to `xColumn`. Risk: Re-introduces GC pressure for text-heavy queries.

**Decision: Option B (Deep Copy at the Boundary) is mandatory.**

We pay the GC tax at the absolute edge of the system to guarantee that `unsafe` memory references never escape the epoch lifecycle. The boundary rule is simple: **no `unsafe.Pointer` or mmap-backed `[]byte` may be returned to any caller outside the ECS package.** All data crossing the package boundary must be copied to GC-managed memory.

This applies equally to FUSE `Read` callbacks — the buffer passed to the FUSE layer must be a copy, not a slice of the mmap region. The performance cost is bounded: copies only occur at system boundaries (FUSE responses, SQLite results), not during internal traversals.

### 8. FUSE Read Path

A FUSE `Readdir` on `/vulns/` translates to:

```
1. Acquire current epoch (atomic load)
2. Resolve "/vulns/" through the schema topology (ADR-0002)
3. Schema says: Selector="vulns.#", Name="{{.id}}"
4. Iterate the "vulnerability" component dense array
5. For each entity: fill dirent with templated name
6. Release epoch
```

A FUSE `Read` on `/vulns/CVE-2024-1234/severity` translates to:

```
1. Acquire current epoch
2. Resolve entity ID for "CVE-2024-1234" via name→ID index
3. Sparse set lookup for "severity" component
4. Return component data (or string table dereference)
5. Release epoch
```

**Latency reality:** The ECS lookup is nanoseconds (cache-warm). The end-to-end FUSE path through fuse-t's NFS bridge adds 10-100 microseconds of overhead per operation. Honest target: **microsecond-scale FUSE reads**, not nanosecond.

### 9. SIGBUS Handling

mmap'd regions can raise `SIGBUS` on access if the backing file is truncated or the mapping becomes invalid. Since we primarily use `MAP_ANONYMOUS`, SIGBUS from backing file issues is not a concern for the hot path.

For file-backed snapshots, SIGBUS is possible if:
- The snapshot file is truncated while mapped.
- Disk is full during msync.

**Strategy:** Register a SIGBUS handler via `os/signal.Notify` that logs the fault address and triggers a graceful fallback to full rebuild. Do NOT attempt to recover in-place — the mapping is compromised.

**Go runtime interaction:** Go installs its own signal handlers. We use `sigaction` with `SA_ONSTACK` and chain to Go's handler for non-mmap faults. This requires careful testing on each Go version upgrade. Pin the minimum Go version in `go.mod`.

## Memory Budget

At target scale (10M entities, 5 component types, 64 bytes average per component):

| Component | Size |
|---|---|
| Dense arrays (10M x 5 x 64B) | 3.2 GB |
| Sparse arrays (10M x 5 x 4B) | 200 MB |
| String table (est. 128B avg x 10M) | 1.3 GB |
| Per-epoch total | ~4.7 GB |
| Peak (3 concurrent epochs) | ~14.1 GB |

**Minimum hardware:** 16 GB unified memory (M1 Pro / M2 Pro or higher). M1 base (8 GB) is insufficient at full scale.

**Backpressure:** If available memory < 2x current epoch size, the writer refuses to begin a new epoch and logs a warning. Readers continue on the current epoch.

## `unsafe` Discipline

Using Go's `unsafe` package with mmap is the highest-risk aspect of this design. Mandatory rules:

1. **Every function touching mmap'd memory MUST call `runtime.KeepAlive(mmapSlice)` before returning.** Missing one call site is a latent use-after-free.
2. **`unsafe.Pointer` ↔ `uintptr` conversion MUST happen in a single expression.** Never store a `uintptr` across a potential GC safepoint.
3. **Zero Go heap pointers in mmap'd regions.** Enforced by code review and a CI check that greps for pointer-typed fields in component structs.
4. **All unsafe operations wrapped in named functions** (e.g., `readComponent[T](epoch, entityID)`) with bounds checks on the offset before the unsafe cast.
5. **Fuzz testing** of the unsafe boundary with `go test -fuzz` targeting malformed entity IDs, out-of-bounds offsets, and concurrent access patterns.

## Consequences

### Positive

- **Wait-free reads** for FUSE and SQLite. Readers never block on writers.
- **Zero GC pressure** from the data store. Dense arrays are invisible to Go's collector.
- **Cache-friendly iteration.** Sequential dense array scans for `Readdir` and bulk queries.
- **Legacy tool compatibility.** `grep`, `find`, `ls` work natively via FUSE.
- **No external dependencies.** No JVM, no network protocol, no separate process.

### Negative

- **Manual memory safety liability.** `unsafe` + mmap is the most hazardous pattern in Go systems programming. Bugs manifest as silent data corruption, not panics.
- **3x memory overhead** during epoch transitions (not 2x — three epochs may coexist).
- **Single-writer bottleneck.** Ingest throughput is limited to one goroutine's parsing speed.
- **Platform-specific.** Little-endian binary layout, Apple Silicon cache line assumptions, macOS mmap semantics. Not portable without additional work.
- **Testing difficulty.** The race detector cannot see raw memory accesses. Custom signal handlers interact with Go's runtime in version-dependent ways.
- **Maintainability risk.** Every entity reference is an integer + sparse set lookup. No type safety at the mmap boundary.

### Composition with Other ADRs

- **ADR-0002 (Schema):** The schema engine drives _what_ gets projected. This ADR provides _where_ the data lives at runtime. The schema's eager-loading walk (ADR-0002) populates the ECS dense arrays at mount time.
- **ADR-0003 (CAS):** The layered filesystem reorganizes directory views. With this ADR, reorganization means updating the name→entity mapping. The underlying CAS data (ADR-0003's hard links) maps to entity IDs in the ECS.

## Phased Approach

This design is complex. We will build incrementally and let profiling data justify each escalation:

### Phase 0: Baseline (build first, measure)
- Implement ADR-0002's schema engine with standard Go structs and `sync.RWMutex`.
- Mount real vulnerability data (grype-db) through FUSE.
- Profile: GC pauses, lock contention, `Readdir` latency at 100K / 1M / 10M entities.
- **Gate:** Only proceed to Phase 1 if measured performance is insufficient.

### Phase 1: ECS without mmap
- Replace Go structs with dense arrays (still heap-allocated, still GC-visible).
- Measure improvement in iteration speed and GC pause reduction.
- **Gate:** Only proceed to Phase 2 if GC is still the bottleneck.

### Phase 2: mmap backing
- Move dense arrays to mmap'd anonymous regions.
- Implement epoch-based reclamation.
- Measure: GC eliminated from data path? TLB miss rates acceptable?

### Phase 3: Full RCU + snapshots
- Add file-backed snapshots for fast restart.
- Add SQLite virtual table with epoch-aware transactions.
- Add SIGBUS handling for snapshot mmaps.

## Testing Strategy

- **Unit tests:** ECS operations (add/remove/lookup) with standard Go testing.
- **Fuzz tests:** `go test -fuzz` on unsafe boundary (malformed IDs, offsets, concurrent readers).
- **Stress tests:** 10M entity load test with concurrent FUSE reads and epoch swaps.
- **SIGBUS test:** Intentionally truncate a snapshot file and verify graceful fallback.
- **Leak detector:** CI job that monitors epoch count and memory growth over sustained operation.
- **Go version gate:** Signal handler tests run on each Go minor version before upgrade.

## References

- Fraser, K. (2004). _Practical lock-freedom._ Epoch-based reclamation.
- Herlihy, M. (1991). _Wait-free synchronization._ Concurrency classification hierarchy.
- Michael, M. (2004). _Hazard pointers._ Safe memory reclamation for lock-free objects.
- ADR-0001: User-Space FUSE Bridge (transport layer)
- ADR-0002: Declarative Topology Schema (shape of projection)
- ADR-0003: CAS & Layered Overlays
