# Mache Example Schemas

This directory contains example topologies (schemas) for Mache, demonstrating how to project various data sources into filesystem hierarchies.

## Table of Contents

- [Data Sources (JSON/SQLite)](#data-sources-jsonsqlite)
  - [NVD Schema (`nvd-schema.json`)](#nvd-schema)
  - [KEV Schema (`kev-schema.json`)](#kev-schema)
  - [LLM Conversations (`llm-conversations-schema.json`)](#llm-conversations)
- [Source Code (Tree-sitter)](#source-code-tree-sitter)
  - [Go Schema (`go-schema.json`)](#go-schema)
  - [Python Schema (`python-schema.json`)](#python-schema)
  - [SQL Schema (`sql-schema.json`)](#sql-schema)
  - [Cobra CLI Schema (`cli-schema.json`)](#cobra-cli-schema)
- [Testing](#testing)

## Data Sources (JSON/SQLite)

These schemas map structured JSON data into navigable directory trees. They use the `.item.` accessor because the primary data source is Venturi SQLite databases where records are wrapped as `{"schema":"...", "identifier":"...", "item":{...}}`.

### NVD Schema

[`nvd-schema.json`](nvd-schema.json) — Maps the National Vulnerability Database (NVD) JSON feed into a hierarchy of `Year/Month/ID`.

- **Source:** [NVD JSON Feed](https://nvd.nist.gov/vuln/data-feeds) (via Venturi)
- **Structure:**
  - `/by-cve`
    - `/:year` (e.g., `2024`)
      - `/:month` (e.g., `01`)
        - `/:cve_id` (e.g., `CVE-2024-1234`)
          - `description`, `published`, `status`, `raw.json`

### KEV Schema

[`kev-schema.json`](kev-schema.json) — Maps the CISA Known Exploited Vulnerabilities catalog.

- **Source:** [CISA KEV Catalog](https://www.cisa.gov/known-exploited-vulnerabilities-catalog) (via Venturi)
- **Structure:**
  - `/vulns`
    - `/:cve_id` (e.g., `CVE-2021-1234`)
      - `vendor`, `product`, `description`, `date-added`, `due-date`, `raw.json`

### LLM Conversations

[`llm-conversations-schema.json`](llm-conversations-schema.json) — Projects LLM conversation exports into a structured archive.

- **Sample Data:** [`llm-conversations-sample.json`](llm-conversations-sample.json)
- **Structure:**
  - `/:provider` (e.g., `anthropic`)
    - `/:year-month` (e.g., `2025-06`)
      - `/:model` (e.g., `claude-sonnet-4`)
        - `/:conversation_id`
          - `title`, `transcript`, `system-prompt`, `token-usage`, `raw.json`

## Source Code (Tree-sitter)

These schemas use Tree-sitter S-expression queries to project source code ASTs into logical views.

### Go Schema

[`go-schema.json`](go-schema.json) — Projects Go source into package-organized views.

- **Source:** `.go` files
- **Structure:**
  - `/:package_name`
    - `imports/`, `functions/`, `methods/`, `types/`, `constants/`, `variables/`
- **Sample Data:** [`testdata/go_sample.go`](testdata/go_sample.go)

### Python Schema

[`python-schema.json`](python-schema.json) — Projects Python source into classes, functions, and imports.

- **Source:** `.py` files
- **Structure:**
  - `imports/` — import statements
  - `classes/` — class definitions with nested `methods/`
  - `functions/` — top-level functions
- **Sample Data:** [`testdata/python_sample.py`](testdata/python_sample.py)

### SQL Schema

[`sql-schema.json`](sql-schema.json) — Projects SQL DDL into tables and views.

- **Source:** `.sql` files
- **Structure:**
  - `tables/` — `CREATE TABLE` statements
  - `views/` — `CREATE VIEW` statements
- **Sample Data:** [`testdata/sql_sample.sql`](testdata/sql_sample.sql)

### Cobra CLI Schema

[`cli-schema.json`](cli-schema.json) — Extracts CLI command structure from Go code using the [Cobra](https://github.com/spf13/cobra) library.

- **Source:** `.go` files using Cobra
- **Structure:**
  - `/:package_name`
    - `commands/` — Cobra command definitions with `Use`, `Short` fields
    - `flags/` — flag definitions with `info` details
- **Sample Data:** [`testdata/cli_sample.go`](testdata/cli_sample.go)

## Testing

Tree-sitter examples are validated by [`examples_test.go`](examples_test.go) using the sample data in `testdata/`. JSON/SQLite schemas are tested by the integration tests in `internal/ingest/`.

```bash
task test -- -run TestTreeSitterExamples ./examples/...
task test -- -run TestSchemasParse ./examples/...
```
