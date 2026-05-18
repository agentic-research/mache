# ADR-0017: Matrix-Coverage Test Harness — Invariants for "What 100% Means"

Date: 2026-05-18
Status: Proposed
Bead: `mache-6682ec`
Campaign: `feat/evolve-coverage-trunk` (PR #385) — drive `internal/*` toward
100% per-package coverage.

## Context

Mache's tests today are layered but not coordinated. A coverage gate that
just asks "did some test exercise this line?" answers a weak question.
Three concrete weaknesses surfaced during the post-T8 / evolve sweep:

1. **No matrix accounting.** The MCP surface is now 18 tools
   (`cmd/serve_handlers.go:41-203` enumerates `list_directory`, `read_file`,
   `find_callers`, `find_callees`, `search`, `semantic_search`,
   `get_communities`, `find_definition`, `get_type_info`, `get_diagnostics`,
   `get_overview`, `get_sheaf_status`, `get_impact`, `get_architecture`,
   `get_diagram`, `write_file`, `resolve_ref`, `find_smells`). The graph
   backends number at least four (`MemoryStore`, `SQLiteGraph`,
   `WritableGraph`, `CompositeGraph` — plus the GraphFS NFS projection).
   Nothing today asserts the Cartesian product is covered. `TestE2E_AllMCPTools`
   in `cmd/all_tools_e2e_test.go` ticks all tools against **one** backend
   (MemoryStore via `buildMaybeMultiGraph` with `MACHE_NO_LEYLINE=1`),
   and `write_file` is explicitly skipped — so we have N×1×1 of the
   N×M×K matrix.

1. **No fixture taxonomy.** The e2e harness uses a 4-package toy Go
   project hand-rolled in `writeE2EFixture` (`cmd/all_tools_e2e_test.go:451-539`).
   Heterogeneous (cross-language), large-corpus, and mache-on-mache
   fixtures exist as open beads (`mache-d332b5`, `mache-be8090`) but
   nothing routes them through the same tool surface. Smell rules and
   graph algorithms get one fixture shape and we extrapolate.

1. **No invariant statement.** "We have tests" is not the same as "we
   know what the tests claim." When `internal/control` coverage drops to
   37% (`mache-51a0fb`), or when a `SocketClient.Close()` race only
   surfaces under `-race` on macOS-latest (`mache-4a827c`), the question
   "what invariant did that violation expose?" has no document to point
   at.

This ADR defines the **invariants** the evolve campaign is ratcheting
toward, **enumerates the existing test surface** mapped to each, and
**decomposes the gaps** into per-package sub-beads ready for dispatch.

This is the compass for the campaign. The next dispatch wave files the
sub-beads in the appendix as a batch; subsequent waves close cells.

## Decision

Six invariants. Each is a falsifiable claim about a specific cell of a
specific matrix. Each has an enforcement mechanism (a test file, a
generator, a CI gate, or a sub-bead committing to one). No aspirational
items.

### Invariant I1 — Tool × Backend matrix

**Claim.** For every (tool, backend) pair where the tool semantically
applies, at least one test invokes the tool against the backend on a
realistic fixture and asserts a non-error result shape.

**Tools (18):** `list_directory`, `read_file`, `find_callers`,
`find_callees`, `search`, `semantic_search`, `get_communities`,
`find_definition`, `get_type_info`, `get_diagnostics`, `get_overview`,
`get_sheaf_status`, `get_impact`, `get_architecture`, `get_diagram`,
`write_file`, `resolve_ref`, `find_smells`.

**Backends (5):** `MemoryStore`, `SQLiteGraph`, `WritableGraph`,
`CompositeGraph`, `GraphFS` (NFS projection over any of the above).

**Inapplicable cells must be declared.** `get_type_info` and
`get_diagnostics` require `_lsp*` tables — on MemoryStore those cells
**are** in the matrix; the assertion is `IsError == true` with the
documented "backend does not support" reason
(`cmd/serve_handlers.go:634, 678, 862`). Inapplicability is a property of
the cell, not an excuse to skip it.

**Enforcement.** A table-driven generator extending
`TestE2E_AllMCPTools` to iterate backends, with the schema-projected
fixture rebuilt per backend (or projected from a shared corpus).
Failure = a cell whose status is neither `ok` nor a known inapplicable
`IsError` reason.

### Invariant I2 — Fixture coverage matrix

**Claim.** Every tool runs against every fixture category at least
once. For language-specific tools (`find_callers`, `find_callees`,
`find_smells`, `get_diagnostics`, `find_definition`), the harness
asserts coverage of each language the tool claims to support.

**Fixture categories (4):**

| Category                       | Status    | Source                                                          |
| ------------------------------ | --------- | --------------------------------------------------------------- |
| `toy` (≤10 files, hand-rolled) | exists    | `writeE2EFixture` (`cmd/all_tools_e2e_test.go:451`)             |
| `synthetic-medium`             | open bead | `mache-be8090` (tunable size, generated)                        |
| `mache-on-mache` (self-ingest) | open bead | `mache-be8090` (`task test-go-schema` proxies today)            |
| `heterogeneous-multi-lang`     | open bead | `mache-d332b5` (`testdata/hetero/{go,rust,python,typescript}/`) |

**Enforcement.** Build-tagged fixture loaders. CI matrix job iterates
fixtures × tools (subset where applicable). Failure = a cell with no
recorded invocation.

### Invariant I3 — Public API invariant

**Claim.** Every exported symbol in `api/`, every exported method on a
`graph.Graph` implementation, and every MCP tool handler factory in
`cmd/serve_handlers.go` has at least one **direct** test
(not transitive). "Direct" means a test that names the symbol in its
body or imports the package as a non-blank import for the purpose of
calling that symbol.

**Enforcement.** The `untested_function` rule in `find_smells`
(introduced via `mache-vssv`, closed; subsequently refined for
type/method handling per `mache-k5aq`) is the static analysis input.
A CI invocation of `mache find_smells --rule untested_function`
against the mache repo itself, with the result diffed against a
checked-in allowlist (`coverage-thresholds.yaml`-adjacent), gates the
invariant. Failure = a new untested exported symbol with no allowlist
entry justifying why (e.g. "above the fidelity ceiling per ADR-0013").

### Invariant I4 — Schema engine invariant

**Claim.** Every schema in `examples/*-schema.json` parses, ingests a
representative fixture, and produces the expected projected shape.
This pins the declarative-topology contract that ADR-0002 promised.

**Enforcement.** `examples/examples_test.go` already does this for
the parse step (`TestSchemasParse`) and for 4 schemas via
`TestMCPSchemaIngest` (mcp) and `TestTreeSitterExamples` (go, python,
sql). The gap is the remaining 14 schemas: each needs (a) a sample
fixture and (b) an expected-projected-shape assertion. Pattern is
established; rollout is per-schema.

Failure = a schema present in `examples/` without a paired
`TestIngest_<schema>` function.

### Invariant I5 — Daemon-paired invariant (mock + live)

**Claim.** Every code path that calls into the ley-line-open daemon
over UDS has **two** tests:

1. A **mock-daemon unit test** that pins the wire encoding and the
   client's reaction to each documented response shape (including
   error, EOF, partial reads, version skew).
1. A **live-daemon e2e test** that exercises the same path against a
   real `leyline` subprocess, skipping when `leyline` is not on PATH.

**Pattern reference.** `internal/leyline/sheaf_e2e_test.go:41`
(`TestE2E_SheafCascade_AgainstLiveDaemon`) is the live-daemon model:
spawns daemon, uses a short `/tmp` path to dodge macOS sun_path limit,
captures daemon stderr on failure, drives only through the public UDS
surface.

**Note re bead body.** The bead references
`internal/leyline/sheaf_subscriber_e2e_test.go` as the model. That file
does not exist on `feat/evolve-coverage-trunk` (the SheafSubscriber
work lives on `claude/sheaf-subscribe-c14c43`). When SheafSubscriber
merges into the trunk, this invariant's model citation updates to
that file; in the meantime `sheaf_e2e_test.go` carries the pattern.

**Enforcement.** A discovery rule: for every file in `internal/leyline/`
matching `*.go` (not `*_test.go`) that calls `sendRequest`, `Subscribe`,
or `connect`, assert the existence of both a `*_test.go` sibling and an
`*_e2e_test.go` sibling (or that the function is on an allowlist of
internal helpers). Failure = a new UDS-touching function without paired
coverage.

### Invariant I6 — Perf regression gate

**Claim.** Tracked benchmarks do not regress beyond a configured
per-benchmark threshold (default ±15%) versus a committed baseline.

**Tracked benchmarks (initial set, 12):**

| Path                                              | Benchmark                     |
| ------------------------------------------------- | ----------------------------- |
| `cmd/serve_find_callers_bench_test.go`            | `BenchmarkFindCallers_*`      |
| `cmd/serve_find_definition_bench_test.go`         | `BenchmarkFindDefinition_*`   |
| `cmd/serve_find_smells_bench_test.go`             | `BenchmarkFindSmells_*`       |
| `cmd/serve_get_communities_bench_test.go`         | `BenchmarkGetCommunities_*`   |
| `cmd/serve_get_impact_bench_test.go`              | `BenchmarkGetImpact_*`        |
| `cmd/serve_listdir_bench_test.go`                 | `BenchmarkListDir_*`          |
| `cmd/serve_read_file_bench_test.go`               | `BenchmarkReadFile_*`         |
| `cmd/serve_search_bench_test.go`                  | `BenchmarkSearch_*`           |
| `internal/ingest/ast_walker_bench_test.go`        | `BenchmarkASTWalker_*`        |
| `internal/ingest/benchmark_test.go`               | misc ingestion benches        |
| `internal/ingest/ingest_throughput_bench_test.go` | `BenchmarkIngestThroughput_*` |
| `internal/leyline/socket_bench_test.go`           | `BenchmarkSocket_*`           |

**Enforcement.** `benchmarks/baselines.json` (NEW) records the
canonical numbers. A nightly or pre-merge CI job runs `task bench`,
compares via `benchstat`, and fails on regression beyond threshold.
Coordinates with `mache-15f15a` (operational perf bench) which
supplies the corpora and the wall-clock numbers.

## Existing test surface — categorization

The following table maps every test file in the repo (excluding
vendored, tooling, and tools/) to the invariant it primarily satisfies.
"Uncat" means the file does not cleanly satisfy any invariant — it
exists for its own correctness story, which may be valuable but is not
ratchet-able.

### `cmd/` (mostly MCP surface)

| File                                            | Invariant                                           | Notes                             |
| ----------------------------------------------- | --------------------------------------------------- | --------------------------------- |
| `cmd/all_tools_e2e_test.go`                     | I1 (partial)                                        | N×1 — only MemoryStore            |
| `cmd/auto_leyline_test.go`                      | I5 (mock side)                                      |                                   |
| `cmd/build_leyline_test.go`                     | I5 (mock side)                                      |                                   |
| `cmd/build_meta_test.go`                        | I4                                                  | builder for schema-driven outputs |
| `cmd/call_extractor_ast_test.go`                | I3                                                  | AST call extractor public surface |
| `cmd/cli_test.go`                               | I3                                                  | CLI argument parsing surface      |
| `cmd/config_test.go`                            | I3                                                  |                                   |
| `cmd/dead_code_skip_list_retreat_test.go`       | I1 (`find_smells` cell) + ADR-0013 falsifiability A |                                   |
| `cmd/escape_test.go`                            | uncat                                               | path escape edge cases            |
| `cmd/falsifiability_lsp_projection_test.go`     | ADR-0013 experiment B                               |                                   |
| `cmd/falsifiability_skip_list_ablation_test.go` | ADR-0013 experiment A                               |                                   |
| `cmd/find_smells_cli_test.go`                   | I1 (`find_smells` cell)                             |                                   |
| `cmd/infer_test.go`                             | I4 (FCA inference)                                  |                                   |
| `cmd/init_test.go`                              | I3                                                  |                                   |
| `cmd/leyline_callees_test.go`                   | I5 (mock)                                           |                                   |
| `cmd/leyline_test.go`                           | I5 (mock)                                           | _lsp_\* schema fixtures           |
| `cmd/out_db_test.go`                            | I3                                                  |                                   |
| `cmd/preset_fixture_test.go`                    | I4                                                  | preset schema regressions         |
| `cmd/serve_callees_def_mount_test.go`           | I1 (`find_callees` × NFS)                           |                                   |
| `cmd/serve_find_callers_bench_test.go`          | I6                                                  |                                   |
| `cmd/serve_find_callers_mount_test.go`          | I1 (`find_callers` × NFS)                           |                                   |
| `cmd/serve_find_definition_bench_test.go`       | I6                                                  |                                   |
| `cmd/serve_find_smells_bench_test.go`           | I6                                                  |                                   |
| `cmd/serve_find_smells_load_test.go`            | I1 + I2 (load shape)                                |                                   |
| `cmd/serve_find_smells_test.go`                 | I1 (`find_smells`)                                  |                                   |
| `cmd/serve_find_smells_views_test.go`           | ADR-0013 step 4                                     | v_refs/v_defs                     |
| `cmd/serve_get_communities_bench_test.go`       | I6                                                  |                                   |
| `cmd/serve_get_impact_bench_test.go`            | I6                                                  |                                   |
| `cmd/serve_hosted_test.go`                      | I3 (hosted-mode session registry)                   |                                   |
| `cmd/serve_listdir_bench_test.go`               | I6                                                  |                                   |
| `cmd/serve_lsp_step5_test.go`                   | ADR-0013 step 5                                     |                                   |
| `cmd/serve_mount_test.go`                       | I3 (mount wiring)                                   |                                   |
| `cmd/serve_read_file_bench_test.go`             | I6                                                  |                                   |
| `cmd/serve_repo_test.go`                        | I3 (`--repo` hosted mode)                           |                                   |
| `cmd/serve_resolve_ref_test.go`                 | I1 (`resolve_ref` cell)                             |                                   |
| `cmd/serve_search_bench_test.go`                | I6                                                  |                                   |
| `cmd/serve_test.go`                             | I3                                                  |                                   |
| `cmd/serve_write_test.go`                       | I1 (`write_file` cell)                              |                                   |
| `cmd/sheaf_wire_test.go`                        | I5 (mock)                                           | wire codec                        |
| `cmd/snapshot_test.go`                          | uncat                                               | snapshot roundtrip                |
| `cmd/uds_graph_test.go`                         | I5 (mock)                                           |                                   |
| `cmd/unmount_test.go`                           | I3                                                  |                                   |

### `internal/graph/` (the backend matrix proper)

| File                             | Invariant                                                       | Notes   |
| -------------------------------- | --------------------------------------------------------------- | ------- |
| `arena_test.go`                  | I3 (arena reader)                                               |         |
| `arena_writer_test.go`           | I3 (arena writer)                                               |         |
| `batch_test.go`                  | I3                                                              |         |
| `community_test.go`              | I1 (`get_communities` backend)                                  |         |
| `composite_test.go`              | I3 + I1 (`CompositeGraph` backend)                              |         |
| `composite_callees_test.go`      | I3                                                              |         |
| `composite_defs_test.go`         | I3                                                              |         |
| `delete_file_nodes_perf_test.go` | I6                                                              |         |
| `graph_suite_test.go`            | I3 (shared Graph-interface contract)                            | gold    |
| `graph_test.go`                  | I3 (MemoryStore core)                                           |         |
| `hotswap_test.go`                | I3                                                              |         |
| `invariant_test.go`              | I3 (sort/dedupe invariants)                                     |         |
| `live_graph_test.go`             | I3                                                              |         |
| `quotient_test.go`               | I3                                                              |         |
| `sheaf_invalidate_test.go`       | I5 (mock SheafBackend) + I3                                     |         |
| `shared_test.go`                 | I3 (helpers)                                                    |         |
| `sliceutil_test.go`              | uncat                                                           | utility |
| `snapshot_cache_test.go`         | I3                                                              |         |
| `sqlite_graph_test.go`           | I3 (SQLiteGraph backend)                                        |         |
| `sqlite_graph_lookupdef_test.go` | I3 + ADR-0013                                                   |         |
| `sqlite_resolver_test.go`        | I3                                                              |         |
| `store_local_test.go`            | I3                                                              |         |
| `vdirpath_test.go`               | I3 (virtual directory paths)                                    |         |
| `walkschema_test.go`             | I4                                                              |         |
| `writable_graph_test.go`         | I3 (`WritableGraph` backend, comment cites bead `mache-02f774`) | gold    |

### `internal/ingest/`

| File                              | Invariant                           | Notes              |
| --------------------------------- | ----------------------------------- | ------------------ |
| `address_refs_test.go`            | ADR-0013                            |                    |
| `ast_parity_test.go`              | I3 + I2 (AST vs sitter parity)      |                    |
| `ast_walker_bench_test.go`        | I6                                  |                    |
| `ast_walker_calls_test.go`        | I3                                  |                    |
| `ast_walker_test.go`              | I3                                  |                    |
| `benchmark_test.go`               | I6                                  |                    |
| `binary_filter_test.go`           | I3                                  |                    |
| `context_dedup_test.go`           | I3                                  |                    |
| `diagram_test.go`                 | uncat                               | diagram generation |
| `dig_test.go`                     | I3 (template `dig` func)            |                    |
| `engine_integration_test.go`      | I4                                  |                    |
| `engine_modtime_test.go`          | I3                                  |                    |
| `engine_test.go`                  | I3 + I4 (core ingestion contract)   | gold               |
| `git_test.go`                     | I3                                  |                    |
| `gitignore_test.go`               | I3                                  |                    |
| `ingest_throughput_bench_test.go` | I6                                  |                    |
| `json_walker_test.go`             | I3 + I4                             |                    |
| `location_test.go`                | I3                                  |                    |
| `parent_context_test.go`          | I3                                  |                    |
| `raw_file_test.go`                | I3                                  |                    |
| `reingest_test.go`                | I3                                  |                    |
| `sitter_flatten_test.go`          | I3                                  |                    |
| `sitter_walker_test.go`           | I3                                  |                    |
| `sqlite_loader_test.go`           | I3                                  |                    |
| `sqlite_writer_test.go`           | I3                                  |                    |
| `template_funcs_test.go`          | I3                                  |                    |
| `watcher_test.go`                 | I3 + I5 (watcher → leyline trigger) |                    |

### `internal/leyline/`

| File                   | Invariant      | Notes         |
| ---------------------- | -------------- | ------------- |
| `semantic_test.go`     | I5 (mock)      |               |
| `sheaf_e2e_test.go`    | I5 (live)      | **the model** |
| `sheaf_test.go`        | I5 (mock)      |               |
| `socket_bench_test.go` | I6             |               |
| `socket_test.go`       | I5 (mock)      |               |
| `trigger_test.go`      | I5 (mock) + I3 |               |

### Other internal packages

| File                                            | Invariant                                            | Notes                             |
| ----------------------------------------------- | ---------------------------------------------------- | --------------------------------- |
| `internal/control/control_test.go`              | I3 (arena root/control accessors)                    | 37% coverage gap → `mache-51a0fb` |
| `internal/lang/lang_test.go`                    | I3 (28-language registry)                            | gold                              |
| `internal/lattice/closure_test.go`              | I3 (NextClosure / Ganter)                            |                                   |
| `internal/lattice/context_test.go`              | I3 (FCA FormalContext)                               |                                   |
| `internal/lattice/date_parsing_test.go`         | I3                                                   |                                   |
| `internal/lattice/fuzz_test.go`                 | I3 (fuzz)                                            |                                   |
| `internal/lattice/grammar_introspect_test.go`   | I3                                                   |                                   |
| `internal/lattice/greedy_test.go`               | I3 (greedy entropy ADR-0008)                         |                                   |
| `internal/lattice/infer_test.go`                | I4 (FCA inference end-to-end)                        |                                   |
| `internal/lattice/project_test.go`              | I4 (lattice → topology projection)                   |                                   |
| `internal/lattice/project_ast_test.go`          | I4                                                   |                                   |
| `internal/lsp/binding_log_test.go`              | I5 (mock)                                            |                                   |
| `internal/materialize/json_test.go`             | I3                                                   |                                   |
| `internal/materialize/materialize_test.go`      | I3 (zip + json output)                               |                                   |
| `internal/nfsmount/file_test.go`                | I1 (`read_file` × GraphFS)                           |                                   |
| `internal/nfsmount/graphfs_test.go`             | I1 (`list_directory` × GraphFS)                      |                                   |
| `internal/nfsmount/server_test.go`              | I3 (NFS RPC layer)                                   |                                   |
| `internal/template/render_test.go`              | I3 + I4 (template engine)                            | gold                              |
| `internal/vfs/handlers_test.go`                 | I3 (virtual dir handlers)                            |                                   |
| `internal/vfs/resolver_test.go`                 | I3                                                   |                                   |
| `internal/writeback/format_test.go`             | I3 (gofumpt/hclwrite)                                |                                   |
| `internal/writeback/splice_test.go`             | I3 (byte-range splice)                               |                                   |
| `internal/writeback/splice_inode_unix_test.go`  | I3 (inode preservation)                              |                                   |
| `internal/writeback/splice_inode_other_test.go` | I3 (cross-platform)                                  |                                   |
| `internal/writeback/splice_normalize_test.go`   | I3                                                   |                                   |
| `internal/writeback/validate_test.go`           | I3 (tree-sitter pre-write)                           |                                   |
| `api/schema_test.go`                            | I3 (Topology JSON contract)                          |                                   |
| `examples/examples_test.go`                     | I4 (parse all + ingest for mcp/go/python/sql)        | I4 partial — 14 schemas to add    |
| `graph/cache_test.go`                           | I3                                                   | outer `graph/` shim               |
| `graph/sqlite_test.go`                          | I3                                                   | outer shim                        |
| `tests/integration_test.go`                     | I3 (cross-package: ingest + graph + nfs + writeback) | gold                              |
| `validate/validate_test.go`                     | I3                                                   |                                   |

### Tests that were hard to categorize (flag for human review)

- `cmd/escape_test.go` — exercises path escape edges; not clearly any
  invariant. Either gold (security-adjacent input fuzzing worth
  pinning) or testing-the-wrong-thing (path handling should be a
  graph-backend method test). Recommend keeping; possibly fold into a
  future `internal/path/` package extraction.

- `cmd/snapshot_test.go` — snapshot roundtrip. Useful but the snapshot
  format is internal; if/when snapshots get a public versioned
  schema, this folds into I4.

- `internal/graph/sliceutil_test.go` — utility test. Low-value
  ratchet but harmless; recommend leaving.

- `internal/ingest/diagram_test.go` — diagram generation tested
  in-package. The diagram output is consumed by the `get_diagram` MCP
  tool, so logically this is I1 — but the test stops at the
  in-package boundary. Either escalate to I1 (test through the tool
  surface) or leave as I3 and rely on `cmd/all_tools_e2e_test.go` for
  I1 coverage. Recommend: leave + extend the e2e harness.

## Gap matrix

### Tool × Backend (I1) — open cells

`✓` = covered; `~` = partial (e.g., one fixture only); `□` = open;
`N/A` = inapplicable by design.

| Tool               |    MemoryStore     | SQLiteGraph | WritableGraph | CompositeGraph | GraphFS |
| ------------------ | :----------------: | :---------: | :-----------: | :------------: | :-----: |
| `list_directory`   |         ✓          |      ~      |       □       |       ~        |    ✓    |
| `read_file`        |         ✓          |      ~      |       ~       |       □        |    ✓    |
| `find_callers`     |         ✓          |      ~      |       □       |       □        |    ~    |
| `find_callees`     |         ✓          |      ~      |       □       |       □        |    ~    |
| `search`           |         ✓          |      ~      |       □       |       □        |    □    |
| `semantic_search`  |         ~          |      □      |       □       |       □        |    □    |
| `get_communities`  |         ✓          |      ~      |       □       |       □        |    □    |
| `find_definition`  |         ✓          |      ~      |       □       |       □        |    □    |
| `get_type_info`    |   N/A (LSP req)    |      ~      |      N/A      |      N/A       |   N/A   |
| `get_diagnostics`  |   N/A (LSP req)    |      ~      |      N/A      |      N/A       |   N/A   |
| `get_overview`     |         ✓          |      ~      |       □       |       □        |    □    |
| `get_sheaf_status` |         ✓          |      ✓      |       ✓       |       ✓        |    ✓    |
| `get_impact`       |         ✓          |      ~      |       □       |       □        |    □    |
| `get_architecture` |         ✓          |      ~      |       □       |       □        |    □    |
| `get_diagram`      |         ✓          |      ~      |       □       |       □        |    □    |
| `write_file`       | □ (skipped in e2e) |      □      |       ~       |       □        |    ~    |
| `resolve_ref`      |         ✓          |      ~      |       □       |       □        |    □    |
| `find_smells`      |         ~          |      ✓      |       □       |       □        |    □    |

**Open cell count: ~60.** Most concentrated on `WritableGraph` and
`CompositeGraph` — neither is exercised through the MCP tool surface
end-to-end.

*Note: write_file matrix cells reflect the current test surface, not
the published API. `WritableGraph` and `GraphFS` both support writes
(per the production code and `internal/graph/writable_graph_test.go` /
`internal/nfsmount/graphfs_test.go` unit coverage), but the e2e harness
doesn't exercise those paths through the `write_file` MCP tool yet —
closing those cells is part of SB-02 / SB-03 / SB-16.*

### Fixture × Tool (I2) — open cells

| Tool         | toy | synthetic-medium |      mache-on-mache       | heterogeneous |
| ------------ | :-: | :--------------: | :-----------------------: | :-----------: |
| (every tool) |  ✓  |        □         | ~ (`task test-go-schema`) |       □       |

**Open cell count: ~36** (= 18 tools × 2 missing fixtures).
`synthetic-medium` and `heterogeneous` are both open beads
(`mache-be8090`, `mache-d332b5`) — landing them lights up the full row
once routed through the existing harness loop.

### Public API (I3) — gap surfaces

- `internal/control/` at 37.3% coverage (`mache-51a0fb`) — 6 arena/root
  accessors with 0% direct test coverage.
- `internal/refsvtab/` (364 LOC, **0 test files**) — `mache_refs` vtab
  module has no direct test; covered only transitively by SQLiteGraph
  consumers.
- `internal/linter/` (94 LOC, **0 test files**) — `Lint()` is exported
  and Go-specific but has no direct test.
- `pkg/` does not exist yet (per `mache-734971` API promotion); when
  it lands, I3 expands to it.

### Schema engine (I4) — gap surfaces

`examples/examples_test.go` covers parse for all 18 schemas and ingest
for 4: `mcp` (via `TestMCPSchemaIngest`), `go`, `python`, `sql` (via
`TestTreeSitterExamples`). **14** schemas remain without a paired
`TestIngest_<schema>` test:

`audit-schema.json`, `bdr-hierarchy-schema.json`, `bdr-schema.json`,
`cli-schema.json`, `kev-schema.json`,
`llm-conversations-schema.json`, `markdown-schema.json`,
`mcp-registry-schema.json`, `notion-repos-schema.json`,
`notion-schema.json`, `nvd-schema.json`,
`rust-schema.json`, `terraform-schema.json`,
`trivy-ghsa-schema.json`.

### Daemon-paired (I5) — gap surfaces

`internal/leyline/sheaf_e2e_test.go` covers the cascade path against
a live daemon. The other UDS-touching functions in `internal/leyline/`
(`semantic.go`, `trigger.go`, `socket.go` non-Subscribe paths) have
mock-side tests only. No live-daemon coverage for trigger/semantic
flows.

Also: the bead body cites `internal/leyline/sheaf_subscriber_e2e_test.go`
as the model — that file lives on a sibling branch
(`claude/sheaf-subscribe-c14c43`) and merges into trunk separately.
Until then, `sheaf_e2e_test.go` is the live model.

### Perf regression (I6) — gap surfaces

- `benchmarks/baselines.json` does not exist.
- No CI job runs benchmarks or compares against a baseline.
- 12 benchmark files exist (see I6 table); they run on demand only.

## Campaign milestones

Three-milestone roadmap. Each milestone has a closure criterion
expressible as a CI gate.

### M1 — Tool × Backend matrix for SQLiteGraph backend

**Scope.** Extend `cmd/all_tools_e2e_test.go` to iterate over both
`MemoryStore` and `SQLiteGraph` backends with the same fixture. Fill
every `~` in the SQLiteGraph column of the I1 table to `✓`.

**Closure criterion.** `task profile-tools` runs N×2 cells (every tool
× MemoryStore, every tool × SQLiteGraph) with at most the
documented-inapplicable IsError reasons. The `tool-profile.json`
manifest records backend per row.

**Dependent beads (proposed in appendix):** SB-01, SB-02, SB-03.

### M2 — Heterogeneous + synthetic-medium fixtures wired

**Scope.** Close `mache-d332b5` (heterogeneous) and `mache-be8090`
(rich fixtures). Route the resulting fixtures through the I1 harness
loop so the Fixture × Tool matrix fills.

**Closure criterion.** `task test-e2e-matrix` runs all four fixture
categories × applicable tools, manifest carries `fixture` column,
no open `□` cells in the I2 table.

**Dependent beads (proposed in appendix):** SB-04, SB-05, SB-06.

### M3 — Perf regression gate active

**Scope.** Commit `benchmarks/baselines.json` populated from a
clean-baseline `task bench` run. Add `task bench-check` that diffs
current → baseline via `benchstat` and exits non-zero on regression
beyond per-bench threshold. Wire into nightly CI (not per-PR, to
avoid flake on shared runners).

**Closure criterion.** A regressing PR fails the nightly job with a
named benchmark and a numeric delta. Baseline updates require an
explicit `task bench-baseline` step that commits the new file with
the PR.

**Dependent beads (proposed in appendix):** SB-07, SB-08, SB-09.

The remaining sub-beads (SB-10 through SB-17) are package-coverage
fillers that the evolve campaign picks up between milestones.

## Consequences

### Positive

- "100% coverage" gains a definition: the union of I1–I6 cells being
  filled or explicitly justified-inapplicable.
- The campaign has a backlog (the appendix), not a wishlist.
- The harness gains a single accounting structure (the matrix
  manifest) that survives across the campaign.

### Negative

- The matrix imposes structure on tests that currently grow
  organically — new tools/backends now require matrix updates, not
  just a passing PR.
- I6 nightly bench job needs a stable runner; shared GHA hosts will
  produce noise. Mitigation: per-bench thresholds and a 3-of-5 voting
  scheme before failure.
- I5's "live-daemon coverage for trigger/semantic" depends on
  ley-line-open daemon-side stability that we do not control.

### Reversibility

The invariants are documentation + sub-beads. Removing them is a
matter of closing the sub-beads as wontfix and deleting this ADR.
The tests that exist remain. Only the CI gate additions (untested
function check, perf regression check) require code removal to
revert.

## References

- Bead `mache-6682ec` — this ADR's source
- Bead `mache-6b6da6` — e2e MCP harness foundation (closed)
- Bead `mache-be8090` — rich fixtures (open, M2 input)
- Bead `mache-d332b5` — heterogeneous fixtures (open, M2 input)
- Bead `mache-vssv` — `untested_function` rule (closed; I3 input)
- Bead `mache-k5aq` — `untested_function` type/method fix (closed)
- Bead `mache-15f15a` — operational perf bench (open, I6 input)
- Bead `mache-4a827c` — `-race` discipline (open, I5 input)
- Bead `mache-51a0fb` — `internal/control` 37% coverage gap (open)
- Bead `mache-66d8df` — coverage gate per-package (sibling; consumes
  this ADR's invariants as the "what to ratchet" definition)
- ADR-0002 — declarative topology schema (I4 source)
- ADR-0006 — pure-Go MCP first (I1 backend list)
- ADR-0013 — refs/defs canonical schema (informs I1 `find_smells`)
- `cmd/all_tools_e2e_test.go` — I1 base harness
- `cmd/serve_handlers.go:41-203` — canonical MCP tool list
- `internal/leyline/sheaf_e2e_test.go` — I5 live-daemon model

______________________________________________________________________

## Appendix A — Sub-bead specs ready for dispatch

These are **not filed**; the next dispatcher batches them via
`rsry_bead_create`. Each carries the campaign tag `[mache-6682ec]` so
they trace back to this ADR.

### M1 — Tool × Backend (SQLiteGraph fill)

**SB-01 · e2e: parameterize TestE2E_AllMCPTools over (MemoryStore, SQLiteGraph)**

- files: `cmd/all_tools_e2e_test.go`
- test_files: `cmd/all_tools_e2e_test.go`
- priority: 1
- desc: Replace the single `buildMaybeMultiGraph` call with a
  table-driven loop over backend constructors. Emit `backend` column
  in `tool-profile.json`. Pin the schema-projected fixture so both
  backends consume identical input.

**SB-02 · graph: backend matrix test runner for tool × backend**

- files: `internal/graph/`, `cmd/`
- test_files: `cmd/all_tools_matrix_test.go` (NEW)
- priority: 1
- desc: New file extending SB-01 to also cover `WritableGraph` and
  `CompositeGraph`. Document inapplicable cells with explicit
  `requireIsError` assertions naming the reason.

**SB-03 · nfsmount: GraphFS column for tool × backend**

- files: `internal/nfsmount/`, `cmd/`
- test_files: `cmd/all_tools_nfs_test.go` (NEW)
- priority: 2
- desc: Wire GraphFS over MemoryStore + SQLiteGraph through the
  matrix runner. Drive via in-process NFS client (already used by
  `internal/nfsmount/server_test.go`).

### M2 — Fixture matrix

**SB-04 · fixtures: heterogeneous-multi-lang harness wiring**

- files: `testdata/hetero/`, `cmd/all_tools_e2e_test.go`
- test_files: `cmd/all_tools_hetero_test.go` (NEW)
- priority: 2
- desc: Depends on `mache-d332b5`. Wire the hetero fixtures into the
  matrix runner; language-specific tools assert per-language coverage.

**SB-05 · fixtures: synthetic-medium generator + matrix wiring**

- files: `testdata/synth/`, `cmd/all_tools_e2e_test.go`
- test_files: `cmd/all_tools_synth_test.go` (NEW)
- priority: 2
- desc: Depends on `mache-be8090`. Tunable-size synthetic Go corpus
  (default ~200 files, 50 packages); routed through matrix runner.

**SB-06 · fixtures: mache-on-mache invariant test**

- files: `cmd/all_tools_self_test.go` (NEW)
- test_files: `cmd/all_tools_self_test.go`
- priority: 3
- desc: Ingest `./` with `go-schema.json`, run every tool, assert
  no panic + non-empty result on tools that should hit
  (`find_callers Validate`, `find_definition NewEngine`, etc.).

### M3 — Perf regression gate

**SB-07 · bench: baselines.json initial commit**

- files: `benchmarks/baselines.json` (NEW), `Taskfile.yml`
- test_files: —
- priority: 2
- desc: Run `task bench` clean, capture output, format as benchstat
  baseline; commit.

**SB-08 · bench: task bench-check + benchstat regression gate**

- files: `Taskfile.yml`, `scripts/bench-check.sh` (NEW)
- test_files: —
- priority: 2
- desc: New task that runs benchmarks, diffs against baseline, exits
  non-zero on regression beyond per-bench threshold (default 15%).

**SB-09 · ci: nightly bench job + 3-of-5 vote**

- files: `.github/workflows/bench.yml` (NEW)
- test_files: —
- priority: 3
- desc: Nightly workflow runs `task bench-check` on a stable runner.
  Requires 3 of last 5 nightly runs to regress before failing the
  gate (anti-flake).

### Cross-cutting (interleaved with milestones)

**SB-10 · I3: untested_function CI gate against mache itself**

- files: `scripts/check-untested-functions.sh` (NEW),
  `coverage-thresholds.yaml` (extend with allowlist)
- test_files: —
- priority: 2
- desc: CI runs `mache find_smells --rule untested_function`, diffs
  against allowlist, fails on new untested exported symbol.

**SB-11 · I4: ingest test per example schema**

- files: `examples/examples_test.go`, `examples/testdata/`
- test_files: `examples/examples_test.go`
- priority: 2
- desc: One `TestIngest_<schema>` per schema (14 schemas remain after
  `mcp` + go/python/sql coverage via `TestTreeSitterExamples`).
  Pattern from existing `TestMCPSchemaIngest`. Sample fixtures land
  in `examples/testdata/`.

**SB-12 · I5: live-daemon test for semantic + trigger UDS paths**

- files: `internal/leyline/semantic_e2e_test.go` (NEW),
  `internal/leyline/trigger_e2e_test.go` (NEW)
- test_files: as above
- priority: 2
- desc: Pattern from `sheaf_e2e_test.go`. Skip when `leyline` not on
  PATH. Exercise the full UDS round-trip for each path.

**SB-13 · I3: internal/refsvtab unit tests**

- files: `internal/refsvtab/`
- test_files: `internal/refsvtab/refs_module_test.go` (NEW)
- priority: 2
- desc: 364 LOC with 0 test files. Cover register + scan + filter
  paths against a small SQLite fixture.

**SB-14 · I3: internal/linter unit test**

- files: `internal/linter/`
- test_files: `internal/linter/linter_test.go` (NEW)
- priority: 3
- desc: 94 LOC. Cover `Lint()` for the supported-language branch and
  the unsupported-language no-op branch.

**SB-15 · I3: internal/control coverage to 80%**

- files: `internal/control/`
- test_files: `internal/control/control_test.go`
- priority: 3
- desc: Pairs with existing `mache-51a0fb`. Cover the 6 arena/root
  accessors at 0% coverage. This ADR pins the invariant; the bead
  carries the work.

**SB-16 · I1: write_file happy path in matrix**

- files: `cmd/all_tools_e2e_test.go`, `cmd/serve_write_test.go`
- test_files: as above
- priority: 3
- desc: `write_file` is currently skipped in the e2e harness because
  it mutates fixture state. Split into a separate sub-harness with
  a writable fixture clone; assert post-write read-back round-trip
  per backend that supports it.

**SB-17 · I5: discovery-rule for UDS-touching functions**

- files: `cmd/serve_find_smells.go` (new rule)
- test_files: `cmd/serve_find_smells_test.go`
- priority: 3
- desc: New `find_smells` rule that flags `internal/leyline/` functions
  calling `sendRequest`/`Subscribe`/`connect` without a paired
  `*_test.go` + `*_e2e_test.go` (or allowlist entry). Enforces I5
  as a static gate.

______________________________________________________________________

End of ADR-0017.
