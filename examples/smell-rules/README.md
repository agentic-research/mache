# External smell rules

Drop-in `*.json` files that define additional `find_smells` rules,
loaded at process start when `MACHE_SMELL_RULES_DIR` points at the
directory containing them.

```bash
export MACHE_SMELL_RULES_DIR=$(pwd)/examples/smell-rules
mache serve --http :7532
```

Each external rule shows up in `find_smells` discovery (the no-arg
listing) and as a runnable rule (`find_smells rule=<id>`) the same
way built-ins do.

## File layout

One rule per file. The filename has no semantic meaning beyond
sort order during load — pick something that matches the rule ID
for clarity.

## Rule shape

```json
{
  "ID": "long_unexported_function",
  "Description": "Unexported Go functions whose body exceeds 200 lines.",
  "Requires": ["_ast", "nodes"],
  "ScopeColumn": "fn.source_id",
  "Query": "SELECT fn.source_id, fn.node_id, fn.start_byte, fn.end_byte, fn.start_row, fn.start_col, (fn.end_row - fn.start_row) AS metric FROM _ast fn JOIN nodes n ON n.id = fn.node_id WHERE ... %s ORDER BY metric DESC"
}
```

| Field              | Required    | Notes                                                                                                                                                                                                                                                                                                                                                                                                 |
| ------------------ | ----------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `ID`               | yes         | Stable identifier. Must not collide with a built-in.                                                                                                                                                                                                                                                                                                                                                  |
| `Description`      | recommended | Shown in the rule listing — agents read this.                                                                                                                                                                                                                                                                                                                                                         |
| `Requires`         | recommended | SQL tables the query reads. Used for the pre-flight "this rule needs `_ast`, your backend doesn't have it" diagnostic.                                                                                                                                                                                                                                                                                |
| `ScopeColumn`      | optional    | SQL expression compared to `source_id` when callers pass a `source_id` arg. Leave blank to disable scoping.                                                                                                                                                                                                                                                                                           |
| `Query`            | yes         | SQL with **exactly one `%s`** placeholder for the scope clause.                                                                                                                                                                                                                                                                                                                                       |
| `DefaultMinMetric` | optional    | Integer threshold the handler applies when the caller omits `min_metric`. Use for metric-bearing rules where the natural query output has a long tail of low-metric noise (e.g. `long_function` defaults to 81 lines). An explicit `min_metric=0` from the caller still overrides — agents that want everything sorted can opt out. Omit the field to leave threshold control entirely to the caller. |

## The query contract

Every query MUST select these seven columns in this order:

```
source_id, node_id, start_byte, end_byte, start_row, start_col, metric
```

- `source_id` — the source file path, used for filtering.
- `node_id` — the construct dir or AST node ID being flagged.
- `start_byte`, `end_byte` — byte range of the offending span.
- `start_row`, `start_col` — 0-based line/column (the handler converts to 1-based for the response).
- `metric` — integer score. Use `0` for binary rules with no metric.

Any rule that doesn't match this column shape will produce a SQL
error at run time and the agent will get a tool error.

## SQL caveats

- **Escape `%` chars.** The query is `fmt.Sprintf`'d at run time
  to splice in the optional scope clause. Any `%` that isn't part
  of `%s` must be escaped as `%%`. The loader rejects rules with
  unescaped `%` at startup (regression: a stray `%` in a `LIKE`
  pattern would corrupt the query at the first call).
- **Table availability.** Built-in mache produces `nodes`,
  `node_refs`, `node_defs`. ley-line-parsed `.db` files
  additionally have `_ast`, `_source`, `_imports`, and `_lsp*`.
  Declare in `Requires` so the pre-flight check catches missing
  tables before the SQL runs.

## Common pitfalls

A few schema quirks bite first-time rule authors. The built-in rules
work around them, so consult `cmd/serve_find_smells.go` for prior art
before assuming a "natural" SQL query will do what you expect.

### `nodes.source_file` is empty for construct dirs

The schema engine attaches `source_file` to the leaf rendered files
(`source`, `ast.json`, `doc`) but **not** to the wrapping construct
directory itself. So a query like

```sql
SELECT id FROM nodes WHERE source_file NOT LIKE '%test.go'
```

won't filter construct dirs the way you'd expect. Every dir's
`source_file` is `''`, and `'' NOT LIKE '%test.go'` evaluates true,
so construct dirs are **included** in the result — *the predicate
doesn't filter them at all*. The "is this construct in a test file?"
question can only be answered by walking the dir's child rows (where
`source_file` is non-empty) and projecting the answer up.

Use the COALESCE-via-child idiom the built-in `dead_code` rule uses
(`cmd/serve_find_smells.go:241`):

```sql
WITH child_source AS (
  SELECT parent_id AS node_id, MIN(source_file) AS source_file
  FROM nodes
  WHERE source_file IS NOT NULL AND source_file != ''
  GROUP BY parent_id
)
SELECT COALESCE(NULLIF(n.source_file, ''), cs.source_file, '') AS source_id,
       ...
FROM nodes n
LEFT JOIN child_source cs ON cs.node_id = n.id
```

The `NULLIF(..., '')` is load-bearing — `COALESCE` only skips NULLs,
not empty strings, so without it the empty `n.source_file` shadows
the resolved child `source_file`.

### Joining `node_defs` to an `_ast` row

`node_defs.node_id` is the construct dir (`functions/Foo`), while the
matching `_ast` row's `node_id` is the AST path under the source
(`<type_decl>/type_spec` or similar). A `=` join

```sql
SELECT ... FROM node_defs d JOIN _ast a ON a.node_id = d.node_id
```

returns nothing because the two columns refer to different things.
Use a `LIKE` join with the construct-dir path as a prefix:

```sql
SELECT ... FROM node_defs d
JOIN _ast a ON a.node_id LIKE d.node_id || '/%%'
```

(Remember to escape the SQL `%` as `%%` — see SQL caveats below.)

### Backend produced different tables

`mache build` may dispatch to leyline or the in-process tree-sitter
backend depending on what's on PATH. Leyline produces `_ast`,
`_source`, `_imports`, `_lsp*` in addition to the shared
`nodes` / `node_defs` / `node_refs` tables; the in-process backend
produces `v_defs` / `v_refs` / `_index_coverage` and no `_ast`.

Declare every table your query reads in `Requires` so the pre-flight
check returns a friendly "rule X needs table Y" error instead of a
raw `no such table: _ast`. To check which backend produced an
existing `.db`, look at the `_mache_meta` table:

```bash
sqlite3 my.db "SELECT key, value FROM _mache_meta"
```

## Validation

The loader validates every rule at startup:

- Non-empty `ID`, no collision with a built-in or another external
- Non-empty `Query` with exactly one `%s` placeholder
- `fmt.Sprintf` over the query produces no `%!` error markers
  (catches unescaped `%` chars in `LIKE` patterns and similar)

A validation failure aborts the load and logs a warning — mache
starts without external rules rather than failing to boot. Tests
call `LoadExternalSmellRules` directly and assert errors loudly.

## Example rule

[`long_unexported_function.json`](long_unexported_function.json) is
an example rule that flags Go functions with lowercase names whose
body spans more than 200 source lines. Drop the file in your
`MACHE_SMELL_RULES_DIR` and call `find_smells rule=long_unexported_function`.
