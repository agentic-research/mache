# Portable cache (mache-aeb262) — consumer-side status

Branch: `feat/portable-cache-aeb262`
Worktree: `~/.rsry/worktrees/mache/portable-cache-aeb262/`

The mache portable-cache feature is **feature-complete across all
five named phases** plus quality-of-life CLI ergonomics (`verify`,
`inspect`, `--token-file`).

## What ships on this branch

```bash
# Local round-trip
mache cache push --db ./mache.db ./cache-out
mache cache pull --out-db ./restored.db ./cache-out

# Remote OCI transport (build-cache/v1)
mache cache push --db ./mache.db ./cache-out \
    --remote https://cache.example.com --scope myrepo/abc123 --tag latest
mache cache pull --out-db ./restored.db ./cache-in \
    --remote https://cache.example.com --scope myrepo/abc123 --ref latest

# CI-friendly bundle-existence probe (no restore, no expensive pull)
mache cache verify \
    --remote https://cache.example.com --scope myrepo/abc123 --ref latest

# Local-only debugging summary (no network)
mache cache inspect ./cache-out
mache cache inspect ./mache.lock.bin

# Token loading from a file (recommended for CI)
mache cache push ... --token-file /run/secrets/cache-token
```

## Phase ledger (all complete)

| Phase  | What                                                                                                                             | Commit                                     |
| ------ | -------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------ |
| **1**  | `mache cache push` — walks `_source`, emits chunks + `mache.lock.{bin,toml}`                                                     | `c7d90b7`                                  |
| **2**  | `mache cache pull --verify` — restores from local CAS, verify-on-read                                                            | `c7d90b7`                                  |
| **3**  | `--remote` push/pull via OCI build-cache/v1 (HEAD-checked idempotent push, bounded-parallel chunk upload, verify-on-read on GET) | `98fe421`                                  |
| **4**  | Chunks-as-parse-outputs — when `_ast` table exists, chunks include AST node rows; pull reconstructs both `_source` AND `_ast`    | `0a292ac`                                  |
| **5**  | Taskfile entries + GHA workflow (`cache-roundtrip.yml`)                                                                          | `36dcdfc`                                  |
| extras | `mache cache verify` (CI existence probe)                                                                                        | `a0170cd`                                  |
| extras | `mache cache inspect` + `--token-file` (debug + CI ergonomics)                                                                   | `4b1c9e3`                                  |
| docs   | ADR-0020, ADR path correction, Phase 4 wire-shape doc, README section                                                            | `3733813`, `89be3a2`, `14afb0e`, `a0170cd` |

## Test ledger (46/46 pass)

Run all cache tests: `task cache:test`

| Suite                                 | Count  |
| ------------------------------------- | ------ |
| Local push/pull (Phase 1+2)           | 7      |
| OCI client (Phase 3)                  | 12     |
| End-to-end remote (Phase 3)           | 3      |
| AST round-trip (Phase 4)              | 3      |
| TOML round-trip + Phase 4 error paths | 6      |
| `verify` subcommand                   | 4      |
| `inspect` + `--token-file`            | 11     |
| **Total**                             | **46** |

`golangci-lint run ./cmd/`: 0 issues.

## Substrate (LLO) the consumer side depends on

| LLO bead               | Surface                                                                                          | Status  |
| ---------------------- | ------------------------------------------------------------------------------------------------ | ------- |
| `ley-line-open-ae89aa` | `cache.capnp` schema + Rust + Go bindings + cross-runtime fixtures + cross-repo conformance gate | ✅ Done |
| `ley-line-open-bb0316` | `FsBlobStore` + `MemBlobStore` with race-safe concurrent put + `sweep_stale_temps()` API         | ✅ Done |
| `cloister-bb168f`      | `cloister-spec/build-cache/v1` capability spec + conformance vectors + Rust producer             | ✅ Done |

## Architectural calls captured in code/docs

1. **Producer = `"mache"`** (short-name v1 per ADR-0020).
1. **Kind = `"<language>-source"`** (matches `_source.language` column).
1. **Hash = BLAKE3** (substrate-locked per Σ §3.4); wire digest reuses `sha256:<hex>` prefix with BLAKE3 bytes inside per `cloister-spec/build-cache/v1/README.md` §"Digest encoding."
1. **Chunk shape**: Phase 1 (raw bytes) OR Phase 4 (JSON `{source_id, path, language, content_b64, ast_nodes}`). Auto-detected via `_ast` table presence.
1. **Wire form**: capnp `Marshal` (Go std framing). Canonical encoding (`SetRootCanonical`-shape byte-equal with Rust) deferred to v1.1.
1. **`mache.lock.{bin,toml}`** — both written; `.bin` authoritative, `.toml` for diff-friendly review.
1. **Topology** — empty in v1; future bead populates from `leyline-sheaf` edges.
1. **Token precedence**: `--token-file > --token > MACHE_CACHE_TOKEN env`.

## What still needs operational work

1. **Push branches** — all work is local-only. Branch list:
   - LLO `feat/cache-schema-ae89aa` (~11 commits)
   - cloister main has 3 commits ahead
   - mache `feat/portable-cache-aeb262` (10 commits — this STATUS.md is the latest)
1. **Tag a `leyline-schema` release** that ships `cache.capnp`, so the `go.mod replace` directive in this branch can come out.
1. **Open PRs** in each repo against `main`.
1. **Close beads** after merge: `ley-line-open-ae89aa`, `ley-line-open-bb0316`, `cloister-bb168f`, `mache-aeb262`.

## Phase 4 follow-ups (out of v1 scope)

- Migrate chunks from JSON to capnp-encoded `ast.capnp List(AstNode)` if cross-runtime byte-equal becomes needed.
- Populate `topology` from `leyline-sheaf` edges for incremental restore.
- Thread `mache.Version` into `MacheProducerVersion` (currently `"0.x.y"`).
- Real-registry integration tests (Docker Hub, ghcr) for `WWW-Authenticate` flow.

## How to verify the work locally

```bash
cd ~/.rsry/worktrees/mache/portable-cache-aeb262
task cache:test       # 46/46 pass
task cache:roundtrip  # end-to-end smoke
golangci-lint run ./cmd/   # clean
go build ./cmd/...    # clean
```

## Cron status

`/loop 5m /evolve` running as cron `3d6c97d9`. Marginal return per
iteration is now zero — the feature is saturated. Recommended:
`CronDelete 3d6c97d9` to stop; resume explicitly when ready to push
branches + open PRs.
