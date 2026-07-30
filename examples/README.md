# Mache Examples

This directory is a flat mix of four different kinds of artifact. Use
this map:

| You want to...                                                     | Go to                                                                  |
| ------------------------------------------------------------------ | ---------------------------------------------------------------------- |
| Project a data source or codebase as a filesystem (write a schema) | [Topology schemas](#topology-schemas) below                            |
| Look up what `{{...}}` can do in a schema                          | [Template functions](#template-functions) below                        |
| Adopt the structural smell gate / write custom smell rules         | [`smell-rules/`](smell-rules/README.md) — a self-contained starter kit |
| See the agent-audit projection pattern (markdown → JSON → mount)   | [Audit tooling](#audit-tooling)                                        |
| Find the sample inputs the schemas are tested against              | [Test fixtures](#test-fixtures)                                        |

**Start here** if you're new: read a small schema
([`kev-schema.json`](kev-schema.json) for JSON data,
[`go-schema.json`](go-schema.json) for source code) alongside its
section below — every schema is the same pattern: `nodes` describe
directories, `files`/`file_sets` describe the rendered leaf files, and
selectors (JSONPath for data, tree-sitter queries for code) pick what
lands where. Smell rules are a separate mechanism (SQL over the built
`.db`, not schemas) — see
[`smell-rules/README.md`](smell-rules/README.md).

## Topology schemas

Declarative JSON schemas that drive `mache build` / `mache serve` / the
root mount form (`mache <mountpoint>`), projecting a data source into a
navigable tree.

### Data sources (JSON / SQLite)

These schemas map structured JSON data into directory trees. They use
the `.item.` accessor because the primary data source is Venturi
SQLite databases where records are wrapped as
`{"schema":"...", "identifier":"...", "item":{...}}`.

#### NVD Schema

[`nvd-schema.json`](nvd-schema.json) — Maps the National Vulnerability Database (NVD) JSON feed into a hierarchy of `Year/Month/ID`.

- **Source:** [NVD JSON Feed](https://nvd.nist.gov/vuln/data-feeds) (via Venturi)
- **Structure:**
  - `/by-cve`
    - `/:year` (e.g., `2024`)
      - `/:month` (e.g., `01`)
        - `/:cve_id` (e.g., `CVE-2024-1234`)
          - `description`, `published`, `status`, `raw.json`

#### KEV Schema

[`kev-schema.json`](kev-schema.json) — Maps the CISA Known Exploited Vulnerabilities catalog.

- **Source:** [CISA KEV Catalog](https://www.cisa.gov/known-exploited-vulnerabilities-catalog) (via Venturi)
- **Structure:**
  - `/vulns`
    - `/:cve_id` (e.g., `CVE-2021-1234`)
      - `vendor`, `product`, `description`, `date-added`, `due-date`, `raw.json`

#### LLM Conversations

[`llm-conversations-schema.json`](llm-conversations-schema.json) — Projects LLM conversation exports into a structured archive.

- **Sample Data:** [`llm-conversations-sample.json`](llm-conversations-sample.json)
- **Structure:**
  - `/:provider` (e.g., `anthropic`)
    - `/:year-month` (e.g., `2025-06`)
      - `/:model` (e.g., `claude-sonnet-4`)
        - `/:conversation_id`
          - `title`, `transcript`, `system-prompt`, `token-usage`, `raw.json`

#### Other data schemas

- [`bdr-schema.json`](bdr-schema.json) / [`bdr-hierarchy-schema.json`](bdr-hierarchy-schema.json) — Project a beads issue database (`issues` table) as flat and dependency-hierarchy trees respectively (`title`, `description`, `status`, `priority` per bead).
- [`notion-schema.json`](notion-schema.json) / [`notion-repos-schema.json`](notion-repos-schema.json) — Project Notion page exports: a flat pages view, and a layered repo-catalog view (`/repos/:layer/:name` with `purpose`/`path` leaves).
- [`trivy-ghsa-schema.json`](trivy-ghsa-schema.json) — Projects Venturi GitHub Advisory data into the trivy-db BoltDB layout (`--format boltdb` output path).

### MCP (Model Context Protocol)

These schemas project MCP server capabilities into browsable filesystem trees. MCP is a JSON-RPC 2.0 protocol for connecting AI agents to external tools, resources, and prompts.

#### MCP Server Manifest

[`mcp-schema.json`](mcp-schema.json) — Projects a single MCP server's `tools/list` response into a navigable tree of tools, resources, and prompts.

- **Sample Data:** [`mcp-sample-manifest.json`](mcp-sample-manifest.json)
- **Structure:**
  - `/tools`
    - `/:tool_name` (e.g., `search-issues`)
      - `description`, `input-schema.json`, `raw.json`
  - `/resources`
    - `/:resource_name` (e.g., `repository-readme`)
      - `description`, `uri`, `mime-type`, `raw.json`
  - `/prompts`
    - `/:prompt_name` (e.g., `summarize-repo`)
      - `description`, `raw.json`

The `input-schema.json` file is the key artifact — it contains the JSON Schema for a tool's parameters, making the capability space greppable without loading everything into context.

#### MCP Registry

[`mcp-registry-schema.json`](mcp-registry-schema.json) — Projects the MCP server registry (thousands of servers) into a namespace/server hierarchy.

- **Source:** MCP registry SQLite database (fetched via `tools/mcp-fetch`)
- **Structure:**
  - `/servers`
    - `/:namespace` (e.g., `anthropic`)
      - `/:server_name` (e.g., `claude-code`)
        - `description`, `version`, `status`, `repository`, `raw.json`

This schema uses `Refs` cross-references on server names, enabling the `callers/` virtual directory to answer "which namespaces provide servers with similar names."

### Source code (tree-sitter)

These schemas use tree-sitter S-expression queries to project source
code ASTs into logical views.

#### Go Schema

[`go-schema.json`](go-schema.json) — Projects Go source into package-organized views.

- **Source:** `.go` files
- **Structure:**
  - `/:package_name`
    - `imports/`, `functions/`, `methods/`, `types/`, `constants/`, `variables/`
- **Sample Data:** [`testdata/go_sample.go`](testdata/go_sample.go)

#### Python Schema

[`python-schema.json`](python-schema.json) — Projects Python source into classes, functions, and imports.

- **Source:** `.py` files
- **Structure:**
  - `imports/` — import statements
  - `classes/` — class definitions with nested `methods/`
  - `functions/` — top-level functions
- **Sample Data:** [`testdata/python_sample.py`](testdata/python_sample.py)

#### SQL Schema

[`sql-schema.json`](sql-schema.json) — Projects SQL DDL into tables and views.

- **Source:** `.sql` files
- **Structure:**
  - `tables/` — `CREATE TABLE` statements
  - `views/` — `CREATE VIEW` statements
- **Sample Data:** [`testdata/sql_sample.sql`](testdata/sql_sample.sql)

#### Cobra CLI Schema

[`cli-schema.json`](cli-schema.json) — Extracts CLI command structure from Go code using the [Cobra](https://github.com/spf13/cobra) library.

- **Source:** `.go` files using Cobra
- **Structure:**
  - `/:package_name`
    - `commands/` — Cobra command definitions with `Use`, `Short` fields
    - `flags/` — flag definitions with `info` details
- **Sample Data:** [`testdata/cli_sample.go`](testdata/cli_sample.go)

#### Other source schemas

- [`rust-schema.json`](rust-schema.json) — Rust functions/types with LSP-backed leaves (`hover`, `diagnostics`, `definitions`, `references` from the `_lsp*` tables a ley-line build produces).
- [`terraform-schema.json`](terraform-schema.json) — Terraform/HCL resources with the same LSP file set.
- [`markdown-schema.json`](markdown-schema.json) — Markdown sections by heading (tree-sitter `markdown` grammar).

## Template functions

`name` and `content_template` are Go templates. Alongside the Go builtins
(`printf`, `index`, `range`, …), mache registers these — the set is
`Funcs` in [`internal/template/render.go`](../internal/template/render.go),
which is the source of truth if this table ever falls behind it.

| Function  | Signature                   | Example                                                                             |
| --------- | --------------------------- | ----------------------------------------------------------------------------------- |
| `default` | `default val fallback`      | `{{default .name "unknown"}}`                                                       |
| `dict`    | `dict k v …`                | `{{dict "PkgName" .name "Severity" 4 \| json}}` → `{"PkgName":"curl","Severity":4}` |
| `dig`     | `dig path obj`              | `{{dig "item.affected.0.package.ecosystem" .}}`                                     |
| `first`   | `first slice`               | `{{first .parts}}` — nil when empty                                                 |
| `join`    | `join sep parts`            | `{{split .s ":" \| join ", "}}`                                                     |
| `json`    | `json val`                  | `{{. \| json}}`                                                                     |
| `lookup`  | `lookup val k v … fallback` | `{{lookup .Severity "Critical" 4 "High" 3 0}}`                                      |
| `lower`   | `lower s`                   | `{{lower .name}}` → `rhel`                                                          |
| `replace` | `replace s old new`         | `{{replace .name ":" " "}}` → `alpine 3.18`                                         |
| `slice`   | `slice s start end`         | `{{slice .item.cve.id 4 8}}` → `2024`                                               |
| `split`   | `split s sep`               | `{{index (split .id ":") 0}}` → `alpine`                                            |
| `title`   | `title s`                   | `{{title .name}}` → `Amazon Linux`                                                  |
| `unquote` | `unquote s`                 | `{{unquote .path}}` → `cobra` from `"cobra"`                                        |
| `upper`   | `upper s`                   | `{{upper .name}}` → `DEBIAN`                                                        |

### Two that are silently wrong when misused

Neither errors — both return something plausible — so a schema written on
the wrong assumption renders empty or unchanged files rather than failing.

**`default` takes the value FIRST.** `{{default .name "unknown"}}`.
Reversed, as `{{default "" .name}}`, the first argument is always empty, so
it always returns the second and the guard does nothing — a nil still
renders as Go's literal `<no value>`, which is the case it was added for.

The order is invisible in an example where both arguments are meaningful,
such as `{{default (dig "a.b" .) (dig "c.d" .)}}` for a source with two
possible shapes. That form is idiomatic; it just does not teach the order.

**`lookup` is a value-mapping switch, not a lookup into a collection.** It
compares `val` against literal key/value pairs and returns the match, or the
trailing odd argument as a default. It cannot find a record in a slice by
id. There is no function that can — `dig` reaches into maps and indexes
slices by position, so a schema that needs to resolve an id to its record
has to be shaped so the record is already an ancestor.

`dig` returns `""` when any part of the path is missing, so wrapping it in
`default` is redundant.

## Reaching a parent: `_parent`

A child node's templates can read the parent match's values through
`_parent`, and it nests: `{{._parent._parent.item.id}}` reaches two levels
up. `dig` works through it too, which is usually more readable for deep
paths: `{{dig "_parent._parent.item.id" .}}`.

`_parent` is a reserved key. A data field of that name is shadowed —
underscore-prefixed keys belong to the engine
([`internal/ingest/parent_match.go`](../internal/ingest/parent_match.go)).

This is what lets a leaf carry context it does not itself contain. In the
trivy-db projection the FILE NAME comes from a grandparent, because the
layout that consumer expects keys entries by a vulnerability id that lives
two levels above the version rows being written.

## Smell rules

[`smell-rules/`](smell-rules/README.md) is a **copyable starter kit**
for the structural smell gate: example custom rules (the JSON DSL the
built-in `cmd/rules/*.json` rules also use), plus a README that walks
a brand-new repo through the whole surface — rule shape, how custom
rules overlay the built-ins (`MACHE_SMELL_RULES_DIR`), bootstrapping a
baseline, wiring the
[`find-smells` composite action](../.github/actions/find-smells/README.md)
in CI, and the ratchet model. Unlike everything else in `examples/`,
smell rules are not topology schemas — they're SQL queries over the
`.db` that `mache build` produces.

## Audit tooling

A worked example of projecting agent-coordination markdown as a
queryable filesystem:

- [`audit-indexer.py`](audit-indexer.py) — Parses an `.audit/`
  directory of coordination files (`STATUS.md`, `MASTER-TRIAGE.md`,
  `SCORECARD.md`, `BEAD-PLAN.md`, per-dimension audits) into a single
  `.index.json`.
- [`audit-schema.json`](audit-schema.json) — The topology schema that
  mounts that index (`findings/` with `title`/`severity`/`status`/`dimension`
  leaves).
- [`test-audit/`](test-audit) — Sample `.audit/` input files for
  trying the indexer end-to-end:

```bash
python examples/audit-indexer.py examples/test-audit/

# Serve it as MCP tools…
mache serve --schema examples/audit-schema.json examples/test-audit/.index.json

# …or mount it. Mounting is the ROOT command (`mache [mountpoint]`), so the
# source goes in --data and the mountpoint is the single positional.
mache --schema examples/audit-schema.json --data examples/test-audit/.index.json /tmp/audit
```

## Test fixtures

- [`testdata/`](testdata) — Sample source files (`go_sample.go`,
  `python_sample.py`, `sql_sample.sql`, `cli_sample.go`) the
  tree-sitter schemas are validated against.
- [`mcp-sample-manifest.json`](mcp-sample-manifest.json),
  [`llm-conversations-sample.json`](llm-conversations-sample.json) —
  Sample JSON inputs for the MCP and LLM-conversation schemas.

## Testing

Tree-sitter examples are validated by
[`examples_test.go`](examples_test.go) using the sample data in
`testdata/`. JSON/SQLite schemas are tested by the integration tests
in `internal/ingest/`. The example smell rules are load-tested by
`cmd/serve_find_smells_load_test.go`.

```bash
task test -- -run TestTreeSitterExamples ./examples/...
task test -- -run TestSchemasParse ./examples/...
go test ./cmd/ -run TestLoadExternalSmellRules_ShippedExamplesLoadCleanly
```
