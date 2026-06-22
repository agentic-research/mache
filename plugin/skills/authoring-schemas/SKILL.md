---
name: authoring-schemas
description: Author a mache projection schema (Topology→Node→Leaf) that maps structured data or source code into a filesystem/graph. Use when creating or debugging a *-schema.json for a new data source (SQLite/JSON) or a new source language. Covers both dialects — tree-sitter S-expression selectors for code, and JSONPath selectors for data — the gotchas (@scope on inner nodes, nested $[*], _parent traversal, the .db vs JSON-walker capability split), and how to validate the result.
allowed-tools: Read,Glob,Grep,Edit,Write,Bash(go test *),Bash(task *),Bash(jq *),mcp__mache__*
argument-hint: '<new-schema-name> [source: db|json|code] [example-to-copy]'
version: 0.1.0
author: ART Ecosystem (mache)
---

<!-- Provenance: mache beads mache-442fd9, mache-1094a2. Reference impl: examples/{go,nvd,kev}-schema.json. The @scope-inner gotcha is mache-33b53c. -->

# /mache:authoring-schemas — Write a mache projection schema

A mache schema (`api/schema.go`) projects data into a tree: **`Topology` → `Node` (dirs) → `Leaf` (files)**. There are **two dialects** and the first decision is which you're in:

| Source                        | Walker                  | Selector dialect                                               | Path                             |
| ----------------------------- | ----------------------- | -------------------------------------------------------------- | -------------------------------- |
| **source code** (.go, .py, …) | tree-sitter / ASTWalker | tree-sitter S-expressions: `(kind field: (child) @cap) @scope` | `examples/go-schema.json`        |
| **data** (.db, .json)         | JSONPath (ojg)          | JSONPath: `$`, `$[*]`, `$.item.field[*]`                       | `examples/{nvd,kev}-schema.json` |

Start by copying the closest example and editing — never write from scratch.

## Schema shape (the fields that matter)

```
Topology { version, table?, nodes[], file_sets?, diagrams? }
  Node   { name, selector?, refs?, language?, children[], files[], skip_self_match? }
  Leaf   { name, content_template, content_source?, attributes? }
```

- **`name`** is a Go text/template rendered against the match — it can be a literal (`functions`), a field (`{{.name}}`), or a computed expression (`{{replace .item.X ":" " "}}`). Computed names become directory names.
- **`selector`** picks matches. `$` is the wildcard "everything at this level" (used for grouping dirs like `functions/`, `types/`).
- **`content_template`** renders a leaf's bytes. The `source` leaf conventionally uses `{{.scope}}` — the matched construct's full source text.
- **`refs`** are template strings; each rendered token calls `AddRef(token, nodeID)` → powers `callers/`.

## Tree-sitter dialect (source code) — gotchas

- Pattern: `(outer_kind field: (child_kind) @capture) @scope`. The `@scope` node becomes the projected construct; `@capture`s become template values.
- **`@scope` can be an INNER node**, not the outer match: `(type_declaration (type_spec name: (type_identifier) @name) @scope)` binds scope to `type_spec`, so `{{.scope}}` is `Greeter struct{…}` *without* the `type ` keyword. (This was a real ASTWalker bug — mache-33b53c.) If your `source` leaf includes a leading keyword it shouldn't, your `@scope` is on the wrong node.
- **Ancestry = id-path depth.** A nested capture's depth must match the real named-node nesting; that's how ASTWalker resolves it (`matchAncestry`). Write the selector to mirror the actual tree-sitter named structure.
- Predicates: `#eq?`, `#match?`, `#not-match?` are supported by ASTWalker. `#not-eq?`, `#any-eq?`, `#is?` are NOT (fall back to CGO SitterWalker) — avoid them.

## JSONPath dialect (data) — the full toolbox

The data dialect is richer than the examples suggest (see the venturi trivy schema for a worked maximal example):

- **Nested array iteration**: `$[*]`, then deeper `$.item.X.FixedIn[*]`.
- **Computed directory names** via template funcs: `{{replace .item.Namespace ":" " "}}`.
- **`_parent` ancestor traversal** in templates: `{{._parent._parent.item.Name}}` — pull a value from a grandparent match (e.g. a leaf name from two levels up).
- **Inline JSON content**: `{{dict "Field" .Version "Status" 0 | json}}` builds a JSON object as the file body.
- **Multi-root / multi-view**: multiple top-level `nodes[]` project the *same* data different ways (one tree by namespace, another flat by name).

**Template funcs available**: `json, first, slice, dig, dict, lookup, default, replace, lower, upper, title, split, join, unquote, hasPrefix, hasSuffix, trimPrefix, trimSuffix`.

## ⚠ The capability split (.db fast path vs JSON-walker)

`.db` sources go through **SQLiteGraph** (zero-copy, `json_extract` in SQL) which **cannot iterate nested arrays** (`$.item.remotes[*]`). The richer JSONPath features (nested `$[*]`, deep `_parent`) work on the **JSON-walker/MemoryStore** path (ingesting JSON), not the `.db` fast path. **Rule of thumb: keep `.db` schemas flat; reserve nested iteration / `_parent` for JSON ingestion.** If a `.db` schema silently produces empty dirs, you've hit this — flatten it or pre-flatten the data (see `tools/mcp-fetch` for the normalize-then-flat pattern).

## Procedure

1. **Pick the dialect** (code vs data) and copy the nearest `examples/*.json`.
1. **Sketch the tree** top-down: root grouping nodes (`$` selectors) → construct nodes (real selectors) → `source`/detail leaves.
1. **For data on `.db`:** verify every selector is flat (no nested `[*]`). For deep structure, ingest JSON instead.
1. **Validate** — this is non-negotiable:
   - source schema → `task parity` (the ASTWalker↔SitterWalker byte gate) and mount + `mcp__mache__list_directory` to eyeball the tree.
   - data schema → `task validate` (SQLite fixtures) or mount the real `.db` and inspect.
1. **Check `callers/`** if you added `refs` — mount and read a `callers/` dir to confirm tokens resolve.

## Red flags

- `source` leaf shows a leading keyword (`type `, `func `) it shouldn't → `@scope` is on the wrong (outer) node.
- `.db` schema yields empty dirs → nested-array selector on the SQLiteGraph path; flatten.
- A capture never resolves → ancestry depth in the selector doesn't match the real named-node nesting.
- Using `#not-eq?`/`#is?` → unsupported by ASTWalker; rewrite with `#eq?`/`#match?`.

## Definition of done

The schema mounts, the projected tree matches intent under `list_directory`, `task parity`/`task validate` is green, and (if `refs` were used) `callers/` resolves.
