# Smell rules — a copyable starter

This directory is a working starter kit for mache's structural smell
gate. If you have never seen mache before, read this file top to
bottom: it covers what a rule is, how custom rules compose with the
built-ins, how to bootstrap a baseline, how to wire the CI gate into
your own repo, and how the ratchet model keeps a codebase from getting
structurally worse.

Background in one paragraph: `mache build <src> <out.db>` parses a
source tree into a SQLite database of code structure — construct nodes
(`nodes`), definitions (`node_defs`), references (`node_refs`), and,
when the leyline backend is used, raw AST spans (`_ast`). A **smell
rule** is a SQL query over that database. `mache find-smells` runs
rules and reports each row they return as a *finding*: a (file, node,
span, metric) tuple describing one structural problem.

## 1. What a rule is

One JSON object per file. The example rules in this directory are
copy-paste templates:

| File                                                             | Demonstrates                                                                                          |
| ---------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------- |
| [`fatal_call_in_library.json`](fatal_call_in_library.json)       | Binary rule (`0 AS metric`), cross-reference tables only, `%%` escaping, the empty-`source_file` trap |
| [`long_test_function.json`](long_test_function.json)             | Metric rule with `DefaultMinMetric`, `_ast`-backed, auto-skips on backends without `_ast`             |
| [`long_unexported_function.json`](long_unexported_function.json) | Joining `_ast` spans to `nodes` for name-based filtering                                              |

The shape:

```json
{
  "ID": "long_test_function",
  "Description": "What it flags, why it matters, and known false positives.",
  "Requires": ["_ast"],
  "ScopeColumn": "fn.source_id",
  "DefaultMinMetric": 121,
  "Severity": "warn",
  "Tags": ["tests"],
  "Query": "SELECT fn.source_id, fn.node_id, fn.start_byte, fn.end_byte, fn.start_row, fn.start_col, (fn.end_row - fn.start_row) AS metric FROM _ast fn WHERE ... %s ORDER BY metric DESC"
}
```

| Field              | Required    | Notes                                                                                                                                                                              |
| ------------------ | ----------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `ID`               | yes         | Stable identifier. Must not collide with a built-in or another custom rule.                                                                                                        |
| `Description`      | recommended | Shown in the rule listing — agents and humans read this to decide whether to run the rule. Document known false positives here.                                                    |
| `Requires`         | recommended | SQL tables the query reads. Powers the pre-flight "this rule needs `_ast`, your backend doesn't have it" diagnostic (and the auto-skip under `--rule '*'`).                        |
| `ScopeColumn`      | optional    | SQL expression compared to the caller's `source_id` arg to scope a run to one file. Leave blank to disable scoping.                                                                |
| `Query`            | yes         | SQL with **exactly one `%s`** placeholder where the scope clause is spliced in.                                                                                                    |
| `DefaultMinMetric` | optional    | Threshold applied when the caller omits `min_metric`. Use for metric rules whose natural output has a long tail of low-metric noise. An explicit `min_metric` (even 0) overrides.  |
| `Severity`         | optional    | `off` / `warn` / `error` (ESLint precedent, ADR-0018). Empty means `warn`. The gate decision is made at invocation via `--fail-on`; the rule only states how serious a finding is. |
| `Tags`             | optional    | Free-form labels for `--tags=a,b` selection (union semantics). Keep it to 3–5 per rule.                                                                                            |

### The query contract

Every query MUST select these seven columns in this order:

```
source_id, node_id, start_byte, end_byte, start_row, start_col, metric
```

- `source_id` — the source file path.
- `node_id` — the construct dir or AST node being flagged.
- `start_byte`, `end_byte` — byte range of the offending span (0 if unknown).
- `start_row`, `start_col` — 0-based line/column (the handler converts to 1-based).
- `metric` — integer score, descending sort recommended. Use `0 AS metric` for binary rules.

A rule that doesn't match this column shape produces a SQL error at run
time.

## 2. How built-ins and custom rules compose

Mache ships built-in rules embedded in the binary (`cmd/rules/*.json`
in this repo — same JSON format, same validation). Run
`mache find-smells` with no `--rule` to list everything that's
registered.

Custom rules live in a directory of `*.json` files — like this one —
and are **merged with the built-ins on every invocation**. The
directory is resolved with this precedence:

1. `--rules-dir <dir>` flag
1. `MACHE_SMELL_RULES_DIR` environment variable
1. `smellRulesDir` in the project's `.mache.json` (relative paths are
   project-relative and containment-checked)
1. none — built-ins only

```bash
# Point mache at this directory and list the merged rule set:
mache find-smells --rules-dir examples/smell-rules

# Or via the environment (also honored by `mache serve` / MCP):
export MACHE_SMELL_RULES_DIR="$PWD/examples/smell-rules"
mache find-smells --db repo.db --rule long_test_function
```

Once loaded, custom rules are indistinguishable from built-ins: they
appear in the listing, run under `--rule '*'` globs and `--tags`
filters, participate in baselines, and hit the same pre-flight table
check. The directory is re-scanned per invocation (and per MCP
request), so dropping in a new file needs no restart. Loading is
fail-fast: one malformed rule aborts the whole custom set — the CLI
surfaces the error; `mache serve` logs it and falls back to built-ins.

## 3. Bootstrapping a baseline

An existing codebase will have findings on day one. You don't fix them
all first — you **grandfather** them into a committed baseline and gate
only on *new* debt:

```bash
# 1. Index your repo (downloads/uses leyline automatically when available):
mache build . smells.db

# 2. Record the current findings as the floor and commit the file:
mache find-smells --db smells.db --rule '*' --limit 100000 \
  --baseline-root "$PWD" --write-baseline docs/smell-baseline.json
git add docs/smell-baseline.json
```

Notes:

- `docs/smell-baseline.json` is the conventional default path — the
  composite action below defaults to it. Any path works as long as the
  action's `baseline` input agrees.
- `--baseline-root "$PWD"` relativizes recorded file paths so the
  baseline is portable across machines and CI.
- `--rule '*'` runs every registered rule and skips rules whose
  `Requires` tables aren't in the .db without failing (a `skipping rule ...` notice goes to stderr) — so the same command works on any
  backend.
- Regenerate the baseline with the same mache version (and the same
  custom-rules dir) your CI uses, or counts can differ.

## 4. Wiring the CI gate in your repo

Mache ships a composite GitHub Action
([`.github/actions/find-smells`](../../.github/actions/find-smells/README.md))
that downloads a pinned mache release, indexes the checkout, gates
against your committed baseline, and uploads SARIF to the
code-scanning tab. Complete minimal workflow:

```yaml
# .github/workflows/smells.yml
name: smells

on:
  pull_request:

permissions:
  contents: read
  security-events: write   # for the SARIF upload; drop with upload-sarif: false

jobs:
  smells:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: agentic-research/mache/.github/actions/find-smells@main   # pin a SHA in production
        with:
          baseline: docs/smell-baseline.json   # the default; shown for clarity
```

To include the custom rules from a directory in your repo, export the
env var for the job:

```yaml
    env:
      MACHE_SMELL_RULES_DIR: ${{ github.workspace }}/smell-rules
```

(Custom rules change the finding set — regenerate the baseline with the
same env var set.)

## 5. The ratchet model

The gate is a **ratchet**, not a linter with a fixed zero-findings bar:

- The committed baseline records, per (rule, file), how many findings
  existed when it was written.
- `find-smells --baseline <path>` exits non-zero **only when a
  (rule, file) count exceeds its baseline entry** — brand-new findings
  in new files, or extra findings in known files, fail; grandfathered
  debt passes.
- Fixing debt makes the next `--write-baseline` strictly smaller — the
  ratchet only tightens.
- When a PR deliberately adds a finding (rare, but legitimate),
  regenerate and commit the baseline in the same PR so the review sees
  the decision.

Mache dogfoods this exact loop on itself: the `smells` target in
[`Taskfile.yml`](../../Taskfile.yml) runs the same
`find-smells --rule '*' --limit 100000 --baseline-root --baseline`
invocation the composite action uses (a Go test,
`TestFindSmellsAction_TaskfileParity`, keeps the two in sync), and
`task smells:baseline` regenerates the floor.

______________________________________________________________________

## Appendix: authoring caveats

### Escape `%` chars

The query is `fmt.Sprintf`'d at run time to splice in the optional
scope clause. Any `%` that isn't the single `%s` placeholder must be
escaped as `%%` — SQL `LIKE '%foo%'` becomes `LIKE '%%foo%%'`. The
loader rejects rules with unescaped `%` at startup.

### Which tables exist depends on the build backend

`mache build` dispatches to leyline when available (backend `auto`),
else the in-process tree-sitter backend:

- **Both backends**: `nodes`, `node_defs`, `node_refs` (plus the
  `v_defs` / `v_refs` canonical views).
- **leyline only**: `_ast`, `_source`, `_imports`, `_lsp*`.

Declare every table your query reads in `Requires` so the pre-flight
check reports "rule X needs table Y" instead of a raw
`no such table: _ast` — and so `--rule '*'` can skip the rule cleanly.
To see which backend produced a `.db`:

```bash
sqlite3 my.db "SELECT key, value FROM _mache_meta"
```

### `nodes.source_file` is empty for construct dirs

The schema engine attaches `source_file` to leaf rendered files
(`source`, `ast.json`, `doc`) but **not** to the wrapping construct
directory. So:

```sql
SELECT id FROM nodes WHERE source_file NOT LIKE '%test.go'
```

does not exclude construct dirs — their `source_file` is `''`, and
`'' NOT LIKE '%test.go'` is true, so they all pass the predicate. Add a
non-empty guard (see `fatal_call_in_library.json`), or resolve the
file via child leaves with the COALESCE idiom the built-in `dead_code`
rule uses:

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
not empty strings.

### Joining `node_defs` to an `_ast` row

`node_defs.node_id` is the construct dir (`functions/Foo`) while the
matching `_ast` row's `node_id` is the AST path under the source. A
`=` join returns nothing; use a prefix `LIKE` join:

```sql
SELECT ... FROM node_defs d
JOIN _ast a ON a.node_id LIKE d.node_id || '/%%'
```

### Validation

The loader validates every rule at load time: non-empty `ID` with no
collisions, non-empty `Query` with exactly one `%s`, no unescaped `%`,
and a whitelist check on `ScopeColumn` characters. The rules in this
directory are load-tested by `cmd/serve_find_smells_load_test.go`
(`TestLoadExternalSmellRules_ShippedExamplesLoadCleanly`), so they
can't rot silently.
