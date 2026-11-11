---
status: current
covers-version: v0.22.0
last-verified: 2026-11-11
sources-of-truth:
  - api/schema.go
  - internal/ingest/engine_walk.go
  - internal/ingest/json_walker.go
  - internal/ingest/ast_walker.go
  - internal/ingest/ast_walker_selector.go
  - internal/template/render.go
  - graph/sqlite_graph_scan.go
  - schema/schema.go
  - internal/leylinegraph/leyline.go
audience: [users, agents, contributors]
supersedes: []
---

# Schema reference

A mache schema is a JSON file that describes a directory tree. mache reads a
source — a JSON document, a SQLite table of JSON records, or a source-code
tree parsed by leyline — and lays it out as that tree, for `mache serve`,
`mache build` and the mount. The Go type the file decodes into is
`api.Topology` in [`api/schema.go`](../../api/schema.go).

Every statement here was checked against the code named in the front matter,
and every schema snippet is projected over the sample next to it by
[`schema_test.go`](schema_test.go): the tree shown under a snippet is the tree
the test observed, path for path. The test also fails when `api/schema.go`
gains a field this page does not describe.

## The three source kinds

The same schema file drives three different engines, and they do not behave
identically. The rest of this page says "data (engine)", "data (direct)" or
"source" when a rule is specific to one of them.

| Kind              | Source                                                             | What runs it                                                                                                                  | Reached by                                                                      |
| ----------------- | ------------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------- |
| **data (engine)** | a `.json` file, or a directory of `.json` files                    | `ingest.Engine` with `JsonWalker` ([`engine_walk.go`](../../internal/ingest/engine_walk.go))                                  | `mache serve --schema S x.json`, `mache --schema S --data x.json <mountpoint>`  |
| **data (direct)** | a `.db` whose `results(id, record)` table holds one JSON per row   | `graph.SQLiteGraph` ([`sqlite_graph_scan.go`](../../graph/sqlite_graph_scan.go)) — no ingestion, the db is read on demand     | `mache serve --schema S x.db`, `mache --schema S --data x.db <mountpoint>`      |
| **source**        | a directory of source code, parsed by the pinned leyline into `_ast` | `ingest.Engine` with `ASTWalker` ([`ast_walker.go`](../../internal/ingest/ast_walker.go)); no tree-sitter runs inside mache | `mache build --schema S src out.db`, `mache serve <dir>`, `mache <mountpoint>` |

There is a fourth path, **data (engine) over a `.db`**: `mache --data x.db
--out` streams the `results` table through the engine
([`engine_sqlite.go`](../../internal/ingest/engine_sqlite.go)). It is called
out below where it differs from the direct path.

A SQLite source must be a table with `id` and `record` columns, and `record`
must be JSON. Venturi and `tools/mcp-fetch` write that shape, wrapping each
record as `{"schema": ..., "identifier": ..., "item": {...}}`, which is why
the data schemas in [`examples/`](../../examples/README.md) read `.item.…`. A
beads `issues` table, or any other table, does not project.

## Topology

The root object.

### `version`

The schema version string. `api.SchemaVersion` is `"v1"` and every shipped
schema says so, but nothing reads the field: a missing, empty or different
value changes nothing. The examples test asserts it is `"v1"` so the files
stay uniform. Tracked as `mache-56b800`.

### `table`

The table to read from a SQLite source. Default `results`. The table must
have `id` and `record` columns.

Applies to data (direct) only, where `SQLiteGraph`, `WritableGraph` and
`NodesTableReader` all honour it. The engine's `.db` streaming path
(`mache --data x.db --out`) queries `results` regardless
([`sqlite_loader.go`](../../internal/ingest/sqlite_loader.go)). Tracked as
`mache-56d7dc`.

```json schema:db
{
  "version": "v1",
  "table": "advisories",
  "nodes": [
    {
      "name": "vulns",
      "children": [
        {
          "name": "{{.item.cveID}}",
          "files": [{"name": "vendor", "content_template": "{{.item.vendorProject}}"}]
        }
      ]
    }
  ]
}
```

```text tree
vulns
vulns/CVE-2021-44228
vulns/CVE-2021-44228/vendor = Apache
vulns/CVE-2023-4966
vulns/CVE-2023-4966/vendor = Citrix
```

### `diagrams`

Named diagram views, `name → {"layout": …}`. A content template renders one
with `{{diagram "name"}}`, and the `get_diagram` MCP tool accepts the same
names. The diagram is a mermaid graph of the communities detected over the
refs index (`callers/`), so it is empty for a projection with no refs (the
function returns `%% diagram: no communities detected`).

Rules, from [`engine_diagram.go`](../../internal/ingest/engine_diagram.go):

- With no `diagrams` key, any name renders with layout `TD`.
- With a `diagrams` key, a name it lists renders with that layout; `system`
  always renders (default `TD`); any other name renders the literal
  `%% diagram "name" not defined`.
- Communities and refs are computed once, on the first `{{diagram}}` render
  of the ingest, from the refs indexed at that moment.

Applies to data (engine) and source, where content is rendered by the engine.
On data (direct), `{{diagram}}` is not a defined function: the leaf fails to
render and reads as an error.

```json schema:data
{
  "version": "v1",
  "diagrams": {"deps": {"layout": "LR"}},
  "nodes": [
    {
      "name": "servers",
      "selector": "$",
      "children": [
        {
          "name": "{{.name}}",
          "selector": "$.servers[*]",
          "refs": ["{{.name}}"],
          "files": [{"name": "deps.mmd", "content_template": "{{diagram \"deps\"}}"}]
        }
      ]
    }
  ]
}
```

```text tree
servers
servers/notes
servers/notes/deps.mmd = %% diagram: no communities detected
servers/weather
servers/weather/deps.mmd = %% diagram: no communities detected
```

Two refs that share no token form no community, so the leaf holds the
placeholder.

### `file_sets`

Named lists of leaves, `name → [Leaf, …]`, for a node to pull in with
[`include`](#include). Defined once, used from many nodes. See
[File sets and include](#file-sets-and-include).

### `nodes`

The root directories. Each is a [Node](#node). Order matters: siblings are
tried in order (see [`selector`](#selector), ordered choice) and the direct
path lists roots in this order.

## Node

A directory. One schema node produces one directory per match of its
selector, named by rendering `name` against the match.

### `name`

The directory name. A Go `text/template` rendered against the match (see
[Template context](#template-context)). A name with no `{{` is static.

- data (engine), source: a root node's name is rendered too, but a root is
  matched once, so a static name is the norm.
- data (direct): roots must be static. A root with `{{` in its name is not
  listed and cannot be looked up (`compileLevels` marks only static names as
  roots).
- data (engine) over a `.db`: the root name is used verbatim, unrendered.

A rendered name that contains `/` or `%` is percent-encoded in the node ID
(`/` → `%2F`, `%` → `%25`) so it stays one path segment; `search` and the def
index still see the rendered text ([`segmentEscape`](../../internal/ingest/engine_walk.go)).

### `selector`

The query that picks this node's matches out of its parent's context. The
syntax depends on the source kind; see [Selectors](#selectors).

- data (engine): required to match anything. An empty selector is valid
  JSONPath to ojg and matches nothing, so the node and everything under it
  is silently absent.
- source: an empty selector is rejected (`parse selector: empty selector`).
- data (direct): **ignored**. Every record produces one path down every chain
  of child nodes; the selector key is accepted and never read.
- data (engine) over a `.db`: the root node's selector is ignored (the root
  is created unconditionally); each child selector runs against a one-element
  array holding the record, so `$[*]` yields the record.

Sibling nodes are an **ordered choice** on the source path: the first sibling
whose selector matches a given construct owns it, and later siblings skip it.
That is what lets the Go preset list four receiver shapes under `methods/`
without projecting a method four times. `$` containers are exempt.

### `refs`

Templates rendered against the match; each non-empty result is added to the
refs index as a token pointing at this directory. The token joins the same
index the source path fills from call extraction, so `callers/<token>` and
`find_callers` list the directory, and `{{diagram}}` clusters on it.

Applies to data (engine), including the `.db` streaming path. Ignored on data
(direct). On the source path the engine renders `refs` too, but the index is
already filled from the AST, so presets do not use it.

```json schema:data
{
  "version": "v1",
  "nodes": [
    {
      "name": "tools",
      "selector": "$",
      "children": [
        {
          "name": "{{.name}}",
          "selector": "$.servers[*].tools[*]",
          "refs": ["{{.name}}"],
          "files": [{"name": "description", "content_template": "{{.description}}"}]
        }
      ]
    }
  ]
}
```

```text tree
tools
tools/alerts
tools/alerts/description
tools/forecast
tools/forecast/description = Seven-day forecast
tools/search
tools/search/description
```

After this projection `find_callers("forecast")` returns `tools/forecast`.

### `skip_self_match`

When true, a match whose scope is the parent's own scope is skipped. Source
path only: the comparison is between `_ast` node ids, which JSON matches do
not carry, so on data paths the flag is accepted and never acts. Schema
inference (`--infer`, [`internal/lattice`](../../internal/lattice/project_ast.go))
sets it on the recursive schemas it generates, where a container's selector
would otherwise match the container itself at every level.

### `language`

Restricts a root node to one language on the source path. For each source
file, a root node is used when its `language` equals the file's language
(from [`internal/lang`](../../internal/lang/lang.go)), or its `name` equals
the language, or `language` is empty
([`filterNodesByLanguage`](../../internal/ingest/engine_walkers.go)). A file
no root node applies to lands under `_project_files/`. Only root nodes are
filtered. The bundled language presets leave it empty; inferred multi-language
schemas set it. Ignored on data paths.

### `include`

Names of `file_sets` whose leaves are appended to this node's `files`. See
[File sets and include](#file-sets-and-include).

### `children`

Nested nodes. Each child's selector runs against this match's context (the
matched JSON value; the `@scope` node on the source path), and its templates
see this match's values as `_parent`.

### `files`

The leaves rendered in this directory, one per [Leaf](#leaf) per match. A
node with `files` is a *construct*: it is registered in the def index under
its rendered name (so `find_definition` and `search` find it) and it claims
its ID (see [Naming and dedup](#naming-and-dedup)). A node without `files` is
a container that collapses: every match of it with the same rendered name is
the same directory.

## Leaf

A file.

### `name`

The file name. A template on data (engine) and source. On data (direct) it is
used verbatim: the scan writes `dir + "/" + name` without rendering, and path
lookup matches only names with no `{{`, so a templated leaf name is not
reachable there. Tracked as `mache-56ff85`.

Two names are special on the source path: `source` is kept even when it
renders empty and is the leaf that carries the write-back origin; `doc` is
the conventional home of `{{.doc}}`.

### `content_template`

A template for the file's content, rendered against the match.

- data (engine), source: a leaf that renders empty is dropped unless it is
  named `source`, so an optional field produces no file rather than an empty
  one.
- data (engine) over a `.db`: empty leaves are kept. Content over 4096 bytes
  is not stored; it is re-rendered from the record on read.
- data (direct): content is rendered on first read and cached. Empty leaves
  are kept.

### `content_source`

Instead of a template, the name of an auxiliary table that supplies the
content, joined to construct directories by symbol name. Accepted values and
their tables ([`leylinegraph.go`](../../internal/leylinegraph/leyline.go)):

| Value             | Table           | Content                           |
| ----------------- | --------------- | --------------------------------- |
| `lsp_hover`       | `_lsp_hover`    | `hover_text`                      |
| `lsp_diagnostics` | `_lsp`          | `diagnostics`, non-empty rows     |
| `lsp_defs`        | `_lsp_defs`     | JSON array of definitions         |
| `lsp_refs`        | `_lsp_refs`     | JSON array of references          |

Any other value is an error at materialization. The tables come from
leyline's LSP pass (`leyline lsp --merge-db`); when a table is absent the leaf
is skipped silently. A row's `node_id` is `symbols/<path>`, and its last
segment is matched against the last segment of every directory in the
projection, so the file lands under every construct of that name.

The leaf is written into the `.db`'s nodes table, not rendered by the engine:
`mache build` (via leyline) and `mache serve x.db` materialize it; the engine
itself sees an empty template, renders `""`, and drops the leaf. It is
therefore source-only in practice, and the Go preset declares all four in a
file set named `lsp`. `content_template` and `content_source` are exclusive
by convention; if both are set, the engine renders the template and the
materializer also writes its file only where no node of that path exists.

### `attributes`

An [Attributes](#attributes-1) object. Parsed and never read: every projected
directory is mode `0555` and every leaf `0444` (`0755`/`0644` on a writable
graph) regardless of what is set. Tracked as `mache-5689b3`.

## DiagramDef

The value of one `diagrams` entry.

### `layout`

The mermaid direction: `TD`, `LR`, `BT` or `RL`. Passed to the renderer
unchecked on the template path; `get_diagram` rejects anything else.

## Attributes

### `mode`

A file mode such as `420` (`0644`). Not consumed; see
[`attributes`](#attributes).

## Selectors

### JSONPath — data (engine)

Selectors are JSONPath, parsed by [ojg](https://github.com/ohler55/ojg)
(`jp.ParseString`). A root selector runs against the whole document; a
child selector runs against the parent's matched value.

- `$` matches the context itself, once. At the root that is the whole
  document; under a node it is that node's match.
- `$[*]` iterates an array; `$.servers[*]` a field holding an array;
  `$.servers[*].tools[*]` flattens two levels; `$..name` is recursive descent.
- A match that is a JSON object exposes its fields as template values. A
  match that is anything else — an array, a string, a number — exposes it as
  `{{.value}}`.

```json schema:data
{
  "version": "v1",
  "nodes": [
    {
      "name": "{{.name}}",
      "selector": "$.servers[*]",
      "files": [{"name": "version", "content_template": "{{.version}}"}],
      "children": [
        {
          "name": "tools",
          "selector": "$",
          "children": [
            {
              "name": "{{.name}}",
              "selector": "$.tools[*]",
              "files": [{"name": "description", "content_template": "{{.description}}"}]
            }
          ]
        }
      ]
    }
  ]
}
```

```text tree
notes
notes/tools
notes/tools/search
notes/tools/search/description = Full-text search over notes
notes/version = 0.3.1
weather
weather/tools
weather/tools/alerts
weather/tools/alerts/description
weather/tools/forecast
weather/tools/forecast/description
weather/version
```

Here the root node's selector iterates the array, so the roots are dynamic.
The `tools` container selects `$` — the same server object — and its child
iterates `$.tools[*]` within it.

### None — data (direct)

The direct path does not evaluate selectors. For each row it renders the name
template of every child chain under a root and writes the resulting paths; a
node with `files` at the end of a chain is bound to that row's `id`, and the
leaf content is rendered from the full record when read. Consequences:

- A static-named node under a root is one directory shared by every record.
- A record whose name templates reference a field that is NULL in the record
  is skipped for that root (the scan counts it as "null-skipped" in the log).
  The field is fetched with `json_extract(record, '$.<path>')`, so the path
  must exist in the JSON, and only literal field references are seen: a name
  template that reaches a field through `dig` or `index` does not fetch it.
- Name-template values arrive as strings, whatever their JSON type.
- `refs`, `selector`, `skip_self_match`, `language` and templated leaf names
  are all ignored.

### Tree-sitter S-expressions — source

A selector that starts with `(` is a tree-sitter query pattern, evaluated by
the `ASTWalker` over leyline's `_ast` rows for one file
([`ast_walker_selector.go`](../../internal/ingest/ast_walker_selector.go)).
The presence of any such selector in a schema makes the whole schema a source
schema: `.json` and `.db` inputs are then not parsed.

- `$` is the grouping selector: one match, the parent's own scope, with no
  captures. It is how `functions/`, `types/` and the other container
  directories are made. A root `$` node matches the whole file.
- `(kind …) @scope` matches every node of `kind` under the parent scope.
  `@scope` names the construct: its source text is `{{.scope}}`, its byte
  range is the write-back origin, and nested children query inside it. When
  `@scope` sits on an inner node — `(type_declaration (type_spec …) @scope)`
  — each inner node is its own match, so a grouped `type ( A …; B … )`
  yields two.
- `(kind field: (child_kind) @cap)` captures the text of the child under
  `field` as `{{.cap}}`. An unlabelled child pattern matches a child of that
  kind under any field. `(_)` matches a node of any kind, except as the
  outermost node.
- A capture whose name does not start with `_` is required: a node lacking
  it is not a match. A `_`-prefixed capture is optional and exists for
  predicates.
- Predicates: `(#eq? @cap "literal")`, `(#match? @cap "regex")`,
  `(#not-match? @cap "regex")`. `#not-eq?`, `#any-eq?`, `#is?` and `#is-not?`
  are rejected.
- The outermost node is looked up by kind over the file, then each match is
  checked against the pattern, so a pattern's outer kind cannot be `_`.

The node kinds and field names are the grammar's. For Go,
[`docs/schemas/go.md`](../schemas/go.md) walks the preset; the presets in
[`schema/presets/`](../../schema/presets/) are the reference for the rest.

```json schema:go
{
  "version": "v1",
  "nodes": [
    {
      "name": "{{.pkg}}",
      "selector": "(source_file (package_clause (package_identifier) @pkg)) @scope",
      "children": [
        {
          "name": "functions",
          "selector": "$",
          "children": [
            {
              "name": "{{.name}}",
              "selector": "(function_declaration name: (identifier) @name) @scope",
              "files": [
                {"name": "source", "content_template": "{{.scope}}"},
                {"name": "doc", "content_template": "{{default .doc \"\"}}"}
              ]
            }
          ]
        },
        {
          "name": "types",
          "selector": "$",
          "children": [
            {
              "name": "{{.name}}",
              "selector": "(type_declaration (type_spec name: (type_identifier) @name) @scope)",
              "files": [{"name": "source", "content_template": "{{.scope}}"}]
            }
          ]
        }
      ]
    }
  ]
}
```

```text tree
demo
demo/functions
demo/functions/Greet
demo/functions/Greet/doc = // Greet says hello.
demo/functions/Greet/source
demo/functions/init
demo/functions/init.from_greet_go
demo/functions/init.from_greet_go/source
demo/functions/init/source
demo/types
demo/types/Config
demo/types/Config/source = Config struct{ Name string }
```

`Greet/source` holds the comment and the function. The two `init` functions
have no comment, so their `doc` renders empty and is dropped; without the
`default` it would read `<no value>` (see [Template context](#template-context)).
`Config/source` has no `type` keyword: the `@scope` is the `type_spec` (the
contract [`docs/schemas/go.md`](../schemas/go.md) describes). The two `init`
directories are explained under [Naming and dedup](#naming-and-dedup).

## Template context

Names, content and refs are Go `text/template` strings. `.` is a
`map[string]any` built from the match, and differs per source kind.

**data (engine):** the fields of the matched JSON object, or `{"value": …}`
for a non-object. Nested fields chain: `{{.item.cve.id}}`. A field that is
absent renders `<no value>`; wrap optional fields in `default`
(`{{default .owner ""}}`) or use `dig`.

**data (direct):** a name template sees only the fields it mentions, as
strings, fetched by `json_extract`; a content template sees the whole record
as parsed JSON. For a Venturi db that is `.schema`, `.identifier` and
`.item`, so `{{. | json}}` reproduces the record.

**source:** the captures of the selector by name (`{{.name}}`,
`{{.receiver}}`), plus:

| Key      | Value                                                                                                                                     |
| -------- | ----------------------------------------------------------------------------------------------------------------------------------------- |
| `scope`  | the source text of the `@scope` node                                                                                                      |
| `doc`    | the comment block immediately above the scope, set for content templates only, not for names — and only when there is one: `{{.doc}}` on an undocumented construct renders `<no value>`, so write `{{default .doc ""}}` (`mache-5a1f21`) |

A capture's text is the node's `record` from `_ast` when leyline stored one,
else its byte span of the file. There is no `index`: the `{{.index}}` the
bash, css, dockerfile, markdown, sql and other presets use for unnamed
constructs is set by no walker, so those nodes render as `<no value>`
(`mache-fa8b02`).

**`_parent`** (data (engine) and source): a child node's templates see the
parent match's values under `_parent`, and it nests
(`{{._parent._parent.item.id}}`). `_parent` is a reserved key; a data field
of that name is shadowed. Root matches have no `_parent`. The direct path
does not inject it: a name template that mentions `_parent` asks
`json_extract` for a `$._parent…` path, finds NULL, and skips every record.

```json schema:data
{
  "version": "v1",
  "nodes": [
    {
      "name": "by-server",
      "selector": "$",
      "children": [
        {
          "name": "{{.name}}",
          "selector": "$.servers[*]",
          "children": [
            {
              "name": "{{.name}}",
              "selector": "$.tools[*]",
              "files": [
                {"name": "server", "content_template": "{{._parent.name}} {{._parent.version}}"},
                {"name": "ecosystem", "content_template": "{{default (dig \"_parent._parent.value\" .) \"document\"}}"}
              ]
            }
          ]
        }
      ]
    }
  ]
}
```

```text tree
by-server
by-server/notes
by-server/notes/search
by-server/notes/search/ecosystem
by-server/notes/search/server
by-server/weather
by-server/weather/alerts
by-server/weather/alerts/ecosystem
by-server/weather/alerts/server
by-server/weather/forecast
by-server/weather/forecast/ecosystem = document
by-server/weather/forecast/server = weather 1.2.0
```

The grandparent here is the `by-server` match of `$` over the document, an
object, so `_parent._parent.value` is absent and `ecosystem` falls back to
`document`. Note the argument order: `default` takes the value first and the
fallback second, so a piped value (`{{.x | default "y"}}`) lands in the
fallback slot and `y` is always returned.

**Functions.** The function map is `Funcs` in
[`internal/template/render.go`](../../internal/template/render.go): `json`,
`first`, `unquote`, `slice`, `replace`, `lower`, `upper`, `title`, `split`,
`join`, `hasPrefix`, `hasSuffix`, `trimPrefix`, `trimSuffix`, `dict`,
`lookup`, `default`, `dig`, plus Go's built-ins (`index`, `len`, `printf`,
`range`, `if`, `with`). Each is described with an example in
[examples/README.md § Template functions](../../examples/README.md#template-functions).
`diagram` is added by the engine only (see [`diagrams`](#diagrams)).

A template that fails to parse or execute does not fail the projection: on
the engine paths the name or file is skipped and a `[WARN]` line is logged;
on the direct path a name that fails is skipped silently and content that
fails is an error on read. A `refs` template that fails does fail the
record.

## Naming and dedup

A node's ID is its path: the parent's ID, `/`, the rendered name
(percent-encoded as described under [`name`](#name)). Two matches that render
the same name under the same parent want the same ID, and what happens
depends on whether the node has `files`.

- **Containers** (no `files`) merge. Every `.go` file in a package renders
  `{{.pkg}}` to the same name, so the package directory is one directory with
  everyone's children. Static-named containers under a root are the same.
- **Constructs** (with `files`) are never overwritten. The engine claims each
  construct ID once per ingest ([`claimConstructID`](../../internal/ingest/engine_walk.go)).
  When the ID is taken, the construct is renamed `<name>.from_<file>` where
  `<file>` is the source file's base name with `.` replaced by `_`
  (`init.from_greet_go`). When that is taken too — the same name twice in one
  file — a counter is appended: `<name>.from_<file>.2`, `.3`, … Each rename
  logs a `[WARN] duplicate construct name` line naming bead `mache-c777ef`.
  Files are projected in lexical order of their full path, so the bare name
  goes to the lexically first file.
- On the data (direct) path there is no claiming: a second record rendering
  the same path is deduplicated into the same directory, and the leaf content
  comes from whichever record the scan bound last.
- On the `.db` streaming path, directory nodes are created once and children
  are deduplicated by (parent, child) pair.

Constructs also register definition tokens: the rendered name, `pkg.name`
when the match carries a Go package, and the part after the last `.` of a
`Receiver.Method` name, so a call to `Method` resolves to `Receiver.Method`.

Source files the schema cannot place — no root node applies to their
language, the selector is invalid for the grammar, or nothing matched — are
copied whole under `_project_files/<relative path>`. Non-source files in a
source tree land there too. Binary files, files over the size limit
(`--max-file-size`, default 100 MB), hidden directories, `node_modules`,
`target`, `dist`, `build`, `vendor`, `__pycache__` and `.gitignore` matches
are skipped.

## File sets and include

`file_sets` declares named leaf lists at the top level; a node's `include`
names the sets to append to its own `files`. Expansion is
`Topology.ResolveIncludes()` ([`api/schema.go`](../../api/schema.go)): for
each node, in tree order, the leaves of each named set are appended after the
node's own `files`, in the order the names are listed. A name that is not in
`file_sets` is silently ignored. Expansion is not idempotent: a second call
appends the leaves again.

Who expands:

- The mount (`mache <mountpoint>`), `mache build --schema` via leyline, and
  `examples_test.go` call `ResolveIncludes` before projecting.
- `mache serve x.db` expands as a side effect of materializing
  `content_source` leaves.
- `mache serve --schema S x.json` does **not** expand: leaves that come from
  `include` are missing there (`mache-fe04ff`). The same bead covers serve
  resolving a relative `--schema` path under the data file rather than the
  working directory; pass an absolute path.

```json schema:data
{
  "version": "v1",
  "file_sets": {
    "ident": [
      {"name": "name", "content_template": "{{.name}}"},
      {"name": "raw.json", "content_template": "{{. | json}}"}
    ]
  },
  "nodes": [
    {
      "name": "servers",
      "selector": "$",
      "children": [
        {
          "name": "{{.name}}",
          "selector": "$.servers[*]",
          "include": ["ident"],
          "files": [{"name": "version", "content_template": "{{.version}}"}]
        }
      ]
    }
  ]
}
```

```text tree
servers
servers/notes
servers/notes/name = notes
servers/notes/raw.json
servers/notes/version = 0.3.1
servers/weather
servers/weather/name
servers/weather/raw.json
servers/weather/version
```

## Data is read-only, code is writable

A leaf carries a write-back origin — the file and byte range it was cut from
— only when it came from a source construct whose `@scope` resolved, or is a
raw copy under `_project_files/`. Leaves rendered from JSON or SQLite records
have no origin (`graph.Node.Origin` is nil), so a data projection is
read-only however it is mounted: there is no reverse template.

For source, `--writable` mounts route an edit to a `source` leaf through
validate → format → propose, and leyline writes the file. The pipeline is
described in [GETTING-STARTED.md § Write-back](../../GETTING-STARTED.md#write-back)
and designed in [ADR-0009](../adr/0009-ast-aware-write-pipeline.md).

## Known traps

Each of these is verified against the current tree and tracked.

- A SQLite source must be a `results(id, record)` table (or the `table` you
  name, on the direct path), with JSON in `record`.
- `mache serve --schema` with a JSON data file resolves a relative schema path
  under the data file and does not expand `include` (`mache-fe04ff`).
- `mache serve --schema <dir>` is ignored for a directory source; the
  leyline layout is served (`mache-eade2e`).
- `{{.index}}` in the presets is set by no walker (`mache-fa8b02`).
- `version` is not checked (`mache-56b800`); `attributes.mode` is not applied
  (`mache-5689b3`); `table` is ignored by the engine's `.db` path
  (`mache-56d7dc`); the direct path ignores `selector`, `refs` and templated
  leaf names and does not inject `_parent` (`mache-56ff85`).
- A schema with any `(`-selector is a source schema. Over a directory, its
  `.json` and `.db` files are copied raw under `_project_files/`, not
  parsed; JSONPath nodes in the same file never run.

## Samples

The snippets above are projected over these inputs by `schema_test.go`.

The JSON document (`schema:data`):

```json sample:data
{
  "servers": [
    {
      "name": "weather",
      "version": "1.2.0",
      "tools": [
        {"name": "forecast", "description": "Seven-day forecast"},
        {"name": "alerts", "description": "Active weather alerts"}
      ]
    },
    {
      "name": "notes",
      "version": "0.3.1",
      "tools": [
        {"name": "search", "description": "Full-text search over notes"}
      ]
    }
  ]
}
```

The Venturi records (`schema:db`), written one per row into the table the
snippet names, `id` from `identifier`:

```json sample:db
[
  {
    "schema": "kev",
    "identifier": "CVE-2021-44228",
    "item": {"cveID": "CVE-2021-44228", "vendorProject": "Apache", "product": "Log4j"}
  },
  {
    "schema": "kev",
    "identifier": "CVE-2023-4966",
    "item": {"cveID": "CVE-2023-4966", "vendorProject": "Citrix", "product": "NetScaler"}
  }
]
```

The Go package (`schema:go`), two files parsed by the pinned leyline:

```go sample:go config.go
package demo

type Config struct{ Name string }

func init() { defaults = Config{Name: "demo"} }

var defaults Config
```

```go sample:go greet.go
package demo

import "fmt"

// Greet says hello.
func Greet(name string) string { return fmt.Sprintf("hi %s", name) }

func init() { fmt.Println("greet loaded") }
```
