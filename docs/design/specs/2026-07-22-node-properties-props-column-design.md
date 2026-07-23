# Node Properties — De-overload `record`, Add a Queryable `props` Column — Design

**Status:** Draft (brainstorming output), 2026-07-22. Bead `mache-90b89b`. Single-repo
(mache). On-disk format change with a deliberate hard cutover.

**Companion context:** `mache-b8fe72` (the `context` column — the precedent this follows),
`mache-d28eb1` (Properties preservation across the two-pass write), and the serve-time
Properties fix shipped in v0.19.0 (`nodes_table_reader.go` restoring Properties from
`record` for dir nodes — this spec replaces that mechanism).

______________________________________________________________________

## 1. Problem

`graph.Node.Properties` is `map[string][]byte`. Persisting it (`sqlite_writer.go:451`)
does `json.Marshal` on that map, and `encoding/json` renders `[]byte` as base64. For the
one key whose value is already JSON — `imports` — that means a **second** marshal wrapped
in base64.

Measured 2026-07-22:

```
imports map -> json.Marshal -> Properties["imports"]   {"fmt":"fmt","http":"net/http"}      31B
Properties  -> json.Marshal -> record column           {"imports":"eyJmbXQ...","lang":"Z28="} 72B
                                                                                          = 2.3x
```

Three consequences, in increasing order of importance:

1. **Double encode** — `imports` is marshaled twice.
1. **Base64 bloat** — 2.3x on the measured case; every value pays it, not just `imports`.
1. **Un-queryable** — `json_extract(record,'$.lang')` yields `Z28=`, not `go`. No SQL
   or smell rule can filter on `lang`/`pkg`/`imports`. This fights mache's core design of
   pushing extraction into SQLite via `json_extract`.

### 1.1 The deeper issue: `record` is triple-overloaded

`record` currently means one of three things depending on node kind and producer:

| Meaning                      | Written by                        | Read by                                                           |
| ---------------------------- | --------------------------------- | ----------------------------------------------------------------- |
| Source data record           | data-file ingest                  | `sqlite_graph_scan.go:142` via `json_extract(record,'$.<field>')` |
| Inline rendered file content | `sqlite_writer.go:449` (`n.Data`) | `NodesTableReader.ReadContent`                                    |
| Serialized Properties        | `sqlite_writer.go:451`            | `nodes_table_reader.go` (dir nodes only)                          |

The v0.19.0 serve-time fix had to write
`CASE WHEN kind = NodeKindDir THEN record ELSE NULL END` precisely to disambiguate the
second and third meanings. That workaround is the symptom; the overload is the cause.

### 1.2 Scope of the keys involved

Only six keys are ever set, all in `engine_walk.go`:

| Key             | Value shape                   | Set at               |
| --------------- | ----------------------------- | -------------------- |
| `lang`          | plain string                  | `engine_walk.go:257` |
| `pkg`           | plain string                  | `engine_walk.go:261` |
| `imports`       | **JSON object**               | `engine_walk.go:273` |
| `location`      | plain string `path:start:end` | `engine_walk.go:417` |
| `ast_source_id` | plain string                  | `engine_walk.go:450` |
| `ast_scope_id`  | plain string                  | `engine_walk.go:451` |

`imports` is the only pre-marshaled value, and therefore the only true double-encode.
The other five are plain strings that base64 gratuitously.

______________________________________________________________________

## 2. Decision

Add a **`props JSON`** column to `nodes`, and change `Node.Properties` to
**`map[string]json.RawMessage`**.

`record` drops back to two meanings (data record, or inline content). Properties get a
column of their own, following the `context` precedent exactly.

### 2.1 Schema

```sql
CREATE TABLE IF NOT EXISTS nodes (
    id         TEXT PRIMARY KEY,
    parent_id  TEXT,
    name       TEXT NOT NULL,
    kind       INTEGER NOT NULL,
    size       INTEGER DEFAULT 0,
    mtime      INTEGER NOT NULL,
    record_id  TEXT,
    record     JSON,    -- data record OR inline content. NO LONGER Properties.
    source_file TEXT,
    context    BLOB,    -- mache-b8fe72
    props      JSON     -- NEW: node Properties as real nested JSON
);
```

Stored form, for the measured example:

```json
{"lang":"go","imports":{"fmt":"fmt","http":"net/http"}}
```

`json_extract(props,'$.lang')` → `go`. `json_extract(props,'$.imports')` → a real JSON
object. That is the point of the change.

### 2.2 Type change

```go
- Properties map[string][]byte      // Metadata / extended attributes
+ Properties map[string]json.RawMessage
```

`json.RawMessage` is `[]byte` underneath, so the *marshal* side nests correctly with no
wrapper. The *read* side changes: values are now JSON-encoded, so a plain string key
arrives as `"go"` (with quotes), not `go`.

Consumers reading string-valued keys must unquote. The affected sites, all currently
doing `string(node.Properties[k])`:

- `internal/vfs/location.go` (3 sites — `location`)
- `internal/graph/memstore_callees.go` (`lang`, `ast_source_id`, `ast_scope_id`)
- `internal/graph/composite.go:502` (`lang`)
- `internal/graph/sqlite_graph_callees.go` (`lang`, `ast_*`)
- `internal/ingest/engine_walk.go:285` (`pkg`)
- `cmd/serve_handler_read_file.go`, `cmd/serve_handler_list_directory.go` (`location`)
- `cmd/serve_handler_find_callees.go` (presence check on `lang` only — unaffected)

A single helper keeps this from being error-prone at each site:

```go
// PropString returns a string-valued property, unquoted. Returns "" when the
// key is absent or the value is not a JSON string.
func PropString(n *Node, key string) string
```

Setting sites in `engine_walk.go` gain the symmetric `SetPropString` so the quoting
convention lives in one place rather than at eight call sites.

### 2.3 Compatibility — hard cutover, scoped by producer

**No legacy decode path is written.** The base64-map-in-`record` format is not read back
by any new code.

A literal "refuse anything without `props`" would break mache's primary input, because
**leyline-produced nodes tables have neither `context` nor `props`** — their schema stops
at `source_file` — and `leyline parse` has been the sole source parser since v0.18.0.

The discriminator already exists. mache's writer always has `context` (in `CREATE TABLE`,
plus the `ALTER` at `sqlite_writer.go:143`); leyline's never does.

| `context` | `props` | Producer         | Behavior                                           |
| --------- | ------- | ---------------- | -------------------------------------------------- |
| yes       | yes     | mache, current   | serve normally                                     |
| yes       | **no**  | mache, **stale** | **refuse** — error naming `mache build` as the fix |
| no        | no      | leyline          | serve; `Properties` nil — **unchanged from today** |
| no        | yes     | —                | not producible; treat as mache-current             |

The leyline row is not a degradation: leyline `.db` files carry no Properties in `record`
today either, so their `Properties` are already nil on the serve path.

The refusal must name the remedy. A bare "schema mismatch" sends the reader to the source;
the error says the `.db` predates the `props` column and to rebuild with `mache build`.

### 2.4 What is deliberately not done

- **No hot columns for `lang`/`pkg`.** Promoting them to indexed `TEXT` columns is the
  faster-query option, but commits to a key taxonomy that is still moving (`ast_source_id`
  / `ast_scope_id` arrived recently). `json_extract(props,...)` is queryable, which is the
  blocking property; indexing is a later optimization with a measurement behind it.
- **No dual-write transition.** Explicitly rejected: it would keep the 2.3x bloat for the
  duration and double the write path.
- **No change to the portable cache wire shape.** `mache push`/`pull` ship the `.db`; a
  cached pre-`props` mache `.db` hits the same refusal and is rebuilt.
- **No LLO change.** Asking leyline to emit `props` is a separate, cross-repo decision.

______________________________________________________________________

## 3. Verification

The reason for this change is queryability, so the load-bearing test is a **SQL** test,
not a Go round-trip. A round-trip test alone would pass even if the stored form were still
base64 — it would only prove the encoder and decoder agree with each other.

| #   | Test                             | Asserts                                                                                                                                                                              |
| --- | -------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| 1   | **SQL queryability** (the point) | Build a fixture `.db`; assert `json_extract(props,'$.lang') = 'go'` and that `json_type(props,'$.imports') = 'object'` — not `'text'`. Fails today.                                  |
| 2   | Round-trip                       | ingest → write → read; `Properties` identical, including `imports` nesting.                                                                                                          |
| 3   | Stale-mache refusal              | `.db` with `context` and no `props` → error mentioning `mache build`.                                                                                                                |
| 4   | Leyline shape                    | nodes table with neither column → serves, `Properties` nil, no error. The regression guard for §2.3.                                                                                 |
| 5   | No base64 in `props`             | For a node with `lang=go`, assert the raw `props` text contains `"lang":"go"` and does **not** contain `Z28=` (the base64 of `go`) — catches a silent revert to `map[string][]byte`. |
| 6   | `record` un-overloaded           | For a dir node, `record` is NULL (or data-record only); Properties live solely in `props`.                                                                                           |

Test 4 is the one that would catch the failure mode this design was refined to avoid, and
test 5 is the one that keeps the fix from silently regressing.

______________________________________________________________________

## 4. Consequences

- `record` carries two meanings instead of three; the `CASE WHEN kind = NodeKindDir`
  workaround in `nodes_table_reader.go` is deleted.
- Smell rules and any SQL consumer can filter on `lang`/`pkg`/`imports` for the first time.
- ~2.3x smaller storage for property-bearing dir nodes (measured on the example; the
  aggregate saving depends on how many dir nodes a projection has and is not claimed here).
- **Breaking:** mache-written `.db` files from before this change must be rebuilt. This is
  the accepted cost of the hard cutover.
- `Node.Properties`' element type changes, which is a breaking change to any external
  consumer of `internal/graph` — currently none, as `pkg/` is not yet public
  (`mache-734971`).
