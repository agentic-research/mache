# 5. FCA-Based Schema Inference

Date: 2026-02-12
Status: Proposed
Depends-On: ADR-0002 (Declarative Topology Schema)

## Context

ADR-0002 requires a hand-authored `Topology` schema to map data into a filesystem
hierarchy. For new Venturi data sources this means manually inspecting records,
identifying sharding strategies, and writing JSON schemas before any data can be
mounted. This friction slows onboarding of new sources.

We need an inference mechanism that examines a data source and produces a valid
`Topology` automatically — zero-config for the common case, with the option to
hand-tune afterwards.

## Decision

We will implement schema inference using **Formal Concept Analysis (FCA)** via
Ganter's NextClosure algorithm on a reservoir sample of records.

### Why FCA

- **Canonical**: The concept lattice is the unique, mathematically complete hierarchy
  of all valid attribute groupings. No heuristic choices in the lattice itself.
- **Deterministic**: Same input always produces the same lattice.
- **Output-polynomial**: NextClosure runs in O(|concepts| × |M| × |G|), practical
  for sampled data (1000 objects × ~200 attributes).

### Pipeline

1. **Reservoir sample** up to 1000 records from the SQLite source (single streaming
   pass, constant memory).
2. **Conceptual scaling**: convert JSON fields to binary attributes — presence for
   paths, date scaling (year/month) for ISO timestamps, enum scaling for
   low-cardinality fields.
3. **NextClosure**: enumerate all formal concepts with a 10,000 concept safety cap.
4. **Projection**: walk the lattice to emit an `api.Topology`:
   - Identifier field = highest-cardinality universal string field
   - Shard levels = date-scaled attributes with 2–100 distinct groups
   - Leaf files = remaining universal scalar fields + `raw.json`

### Package Structure

`internal/lattice/` — separate from `graph` (runtime) and `ingest` (schema-driven).

| File | Purpose |
|------|---------|
| `context.go` | `FormalContext`, bitmap incidence table, derivation operators |
| `closure.go` | `NextClosure` algorithm, `Concept` type |
| `project.go` | Lattice → `api.Topology` projection |
| `infer.go` | `Inferrer` orchestrator, reservoir sampling |

### CLI Integration

```
mache --infer --data vulns.db /mnt/vulns
```

The `--infer` flag triggers schema inference before graph construction. The inferred
schema can optionally be written to `--schema` for inspection and hand-tuning.

## Consequences

- **Zero-config onboarding**: new Venturi sources work immediately without schema
  authoring.
- **Inferred schemas may be suboptimal**: hand-tuned schemas can use domain knowledge
  (e.g., custom `slice` expressions, meaningful file names) that inference cannot
  discover. The inferred schema is a starting point, not a final product.
- **Performance budget**: inference adds < 3s to mount time for 323K-record databases
  (dominated by reservoir sampling I/O).
- **Bitmap dependency**: uses `roaring` (already in go.mod) for efficient FCA
  operations on sampled data.
