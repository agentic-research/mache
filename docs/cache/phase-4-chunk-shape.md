# Phase 4 chunk wire shape

When mache's source db has an `_ast` table, `mache cache push` emits
each chunk as a JSON document containing the source's content + its
AST node rows. `mache cache pull` decodes those chunks to reconstruct
`_source` + `_ast` byte-equal with the input.

When `_ast` is absent, the Phase 1 fallback applies (chunk = raw
source bytes). The two paths coexist; auto-detected at push time via
`dbHasASTTable()` and at pull time via `chunkBodyIsASTShape()`.

## Per-chunk JSON shape

```json
{
  "ast_nodes": [
    {
      "node_id": "src/auth.go/function_declaration",
      "node_kind": "function_declaration",
      "start_byte": 13,
      "end_byte": 55,
      "start_row": 1,
      "start_col": 0,
      "end_row": 1,
      "end_col": 42
    }
  ],
  "content_b64": "cGFja2FnZSBhdXRoCg==",
  "language": "go",
  "path": "src/auth.go",
  "source_id": "src/auth.go"
}
```

## Field semantics

| Field         | Source                                | Notes                                                           |
| ------------- | ------------------------------------- | --------------------------------------------------------------- |
| `ast_nodes`   | `_ast` rows whose `source_id` matches | Ordered by `node_id`. Empty array for sources without AST data. |
| `content_b64` | `_source.content` blob                | Base64-encoded; supports arbitrary bytes.                       |
| `language`    | `_source.language`                    | Optional.                                                       |
| `path`        | `_source.path`                        | Repo-relative.                                                  |
| `source_id`   | `_source.id`                          | Used by `_ast.source_id` joins on restore.                      |

## Determinism

Encoder uses `encoding/json` with struct-declaration field order
(alphabetical). Two pushes of identical inputs produce byte-equal
chunks. `chunk_hash = BLAKE3(JSON bytes)`.

## Why JSON, not capnp

ADR-0021 leaves chunk shape producer-defined. Mache picks JSON
because:

- Human-readable + diff-friendly when committed alongside source
- `encoding/json` is std-lib; no schema bump in cache.capnp
- Detection is trivial via top-level `source_id` key check
- Phase 1 fallback's raw bytes can't be confused for JSON-shape
  except by deliberate construction (near-zero false positive rate)

A future bead can migrate to capnp-encoded `ast.capnp List(AstNode)`
chunks if cross-runtime byte-equal becomes a requirement. The lockfile
schema doesn't change; only the chunk body, gated by a
`producer_version` bump.

## Phase 1 fallback

When `_ast` is absent on push:

```
chunk_hash = BLAKE3(source.content)
chunk body = source.content (raw bytes, no encoding)
```

Pull detects this case automatically: chunks that don't pass
`chunkBodyIsASTShape()` are treated as raw content for `_source`
only. The `_ast` table is NOT created — restoring an empty
`_ast` would lie about what was in the original db.

## Mixed-mode bundles

A mache.db SHOULD have either `_ast` for every source or none. If a
db has `_ast` for SOME sources, push uses Phase 4 for all (an empty
`ast_nodes` list is harmless). Pull's lazy table creation handles
this: the table appears the first time a Phase 4 chunk arrives.

## See also

- ADR-0020 (mache): consumer-side adoption of LLO ADR-0021 schema
- ADR-0021 (LLO): the `CacheLockfile` schema itself
- `cloister-spec/build-cache/v1/`: transport for shipping chunks to
  remote registries
- `cmd/cache_ast.go`: producer/consumer implementation
- `cmd/cache_ast_test.go`: round-trip tests
- `cmd/cache_toml_test.go`: error-path + TOML drift tests
