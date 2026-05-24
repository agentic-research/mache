# ADR-0020 — Mache adopts LLO's CacheLockfile schema for portable-cache (consumer-side ADR)

- **Status:** Proposed (2026-05-21)
- **Tracking bead:** `mache-aeb262`
- **Branch:** `feat/portable-cache-aeb262`
- **Pairs with:**
  - **ley-line-open ADR-0021** — Cache lockfile schema as substrate primitive (the schema design lives there)
  - ADR-0014 (mache-in-constellation — capnp as the protocol IDL across LLO/mache/cloister)
  - ADR-0013 (refs-defs canonical schema — prior art for capnp-anchored, IDL-first table shape)
  - mache-aeb262 (portable mache db — the consumer this ADR scopes)

## Context

mache-aeb262 ("portable mache db, uv.lock shape") needs a lockfile mapping `(source_hash, parser_version) → cache_chunk_hash` plus a sheaf-topology slice, then a `mache push` / `mache pull` CLI on top.

Initial draft of this ADR landed the schema design itself in mache. User correction 2026-05-21: LLO owns the cache substrate (BlobStore, sheaf, daemon protocol); the schema is substrate-shaped and other consumers (me-bundle, agent-corpus) want the same shape. **Moved the schema design to LLO ADR-0021. This ADR is now the consumer-side adoption note for mache.**

## Decision

**Mache adopts LLO's `CacheLockfile` capnp schema (LLO ADR-0021 / `rs/ll-core/schema-capnp/schemas/cache.capnp`) for the portable-cache feature. On-disk: TOML at `mache.lock.toml` AND canonical capnp at `mache.lock.bin` (both committed). Wire: OCI artifacts pushed to a `build-cache/v1` provider (Phase 3 of mache-aeb262).**

> **Path correction note** (2026-05-22): an earlier draft referenced `rs/ll-core/public-schema/capnp/cache.capnp`. The schema actually lives at `rs/ll-core/schema-capnp/schemas/cache.capnp` alongside `common.capnp` / `ast.capnp` / etc. — schema-capnp is the structural substrate; public-schema is for protocol (daemon RPC). The corrected location above is what `cmd/cache.go` actually imports.

This ADR records the **mache-specific** decisions on top of LLO's substrate schema. The schema design itself, the rendering paths (capnp↔TOML↔OCI), and the consequences for substrate consumers all live in LLO ADR-0021.

### Mache-specific calls

**1. `producer` string.** mache writes `producer = "mache"`, `producer_version = "<mache version>"` in `meta`. Reverse-DNS reserved for v2 if/when collision happens.

**2. `kind` vocabulary.** mache uses:

- `"go-source"`, `"rust-source"`, `"hcl-source"`, etc. — one per supported language
- Maps directly to mache's existing `_source.language` column

**3. Topology semantics.** mache populates `[[topology]]` from leyline-sheaf edges (per LLO ADR-0021's "Topology semantics per consumer" note). `from` and `to_source` are repo-relative source paths matching `[[sources]].path`.

**4. Where the lockfile lives.** `mache.lock.toml` at repo root, **committed**. Same convention as Cargo.lock + uv.lock for binaries/CLIs (mache is server-shaped; consumers want determinism). Document this in `mache push` output and in `GETTING-STARTED.md` (mache-d5b869).

**5. `input_hash` source.** BLAKE3 of the raw source file bytes — pre-parser, post-LFS-checkout. No normalization (CRLF, trailing-newline). The lockfile records bytes-on-disk; if a developer's git settings rewrite EOLs, the lockfile mismatches and `mache pull` falls back to re-parse + verify chunk-hash match (per LLO ADR-0021's restore semantics).

**6. Verification posture.** `mache pull` verifies the lockfile end-to-end:

- For each `[[sources]]`: re-hash the local source → must match `input_hash` OR re-parse to produce a chunk and verify its hash matches `chunk_hash` (graceful divergence path).
- The assembled-db hash chains to `meta.root`. Mismatch is a hard fail.

### What this ADR does NOT decide

- Schema shape (LLO ADR-0021).
- TOML rendering rules (cloister ADR-0025 bidi pipeline + LLO ADR-0021).
- OCI wire shape (LLO ADR-0021).
- `build-cache/v1` capability transport — that's a cloister-side spec filed when Phase 3 starts.
- Multi-arch lockfiles — out of scope per LLO ADR-0021; mache v1 inherits the "one lockfile per (repo, processor versions)" constraint.

### Open question

**Cross-language sheaf composition.** When a repo has Go AND Rust, today mache emits separate parse results per language. Should `mache.lock.toml` be one file containing all languages, or one file per language (`mache.go.lock.toml`, `mache.rs.lock.toml`)? LLO ADR-0021 punts this to consumers; mache's call:

- Lean: one combined lockfile per repo. The schema's `kind` field disambiguates entries; cross-language topology can be expressed (Go imports a generated Rust binding, etc.); single-file diff in PRs.
- Counter-argument: splitting per-language is friendlier for repos that only build one language and don't want noise from another. Defer to a follow-on bead if it bites.

Decision: **one combined lockfile** for v1. Revisit at Phase 4 if cross-language friction shows up.

## Consequences

### Positive

- Mache consumes one ecosystem-wide schema; no fork.
- Drift with me-bundle, agent-corpus, future consumers is impossible at the schema layer.
- LLO's existing capnp pipeline + clients/go bindings deliver the Go consumer for free.
- The TOML diff-friendliness story is owned by cloister ADR-0025 — not mache's concern.

### Negative

- Mache's lockfile evolution is gated on LLO's schema versioning. Practically fine — LLO already coordinates capnp triplet bumps per ADR-0014 §3 — but mache cannot ship a schema change unilaterally.
- The `mache push` / `mache pull` CLI design (mache-aeb262 Phase 1) still has to land; this ADR alone doesn't deliver the user-facing surface.

### Neutral

- No new mache-side schema work — the `clients/go/leyline-schema/cache/` bindings come from LLO's codegen pipeline once LLO ADR-0021 lands.
