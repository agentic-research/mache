---
status: current
covers-version: v0.19.0
last-verified: 2026-07-22
sources-of-truth:
  - examples/go-schema.json
  - internal/ingest/engine_walk.go
  - internal/writeback/splice.go
audience: [agents, contributors]
supersedes: [docs/go-parsing-status.md]
---

# Go Schema Reference

Reference for the Go example schema (`examples/go-schema.json`) — what it captures, its limitations, and the contracts the write-back path ([ADR-0009](../adr/0009-ast-aware-write-pipeline.md), [ADR-0015](../adr/0015-syntax-aware-write-protection.md)) must respect.

This is **one schema among many**; see [`internal/lang/lang.go`](../../internal/lang/lang.go) for the full registry of 28 supported languages. The behaviors documented below are Go-schema-specific; analogous reference docs for other schemas can live alongside this file as they're written.

## The Normalization Contract

Mache projects `const`, `var`, and `type` specifications **without their declaration keywords**.
This normalizes the interface for Agents regardless of whether the original source used
single or grouped declarations.

Whether the source is `const X = 1` or `const ( X = 1 )`, the Agent sees and writes `X = 1`.

**The Agent MUST NOT emit the keyword.** If it does, the write-back will produce syntax errors:

```go
// Agent reads:  X = 1
// Agent writes: const X = 1    <-- WRONG
// Result:       const const X = 1   (in grouped block) or const const X = 1 (single)
```

This contract applies to:

- `constants/{name}/source` — no `const` keyword
- `variables/{name}/source` — no `var` keyword
- `types/{name}/source` — no `type` keyword

Functions and methods are **not normalized** — their source includes the full declaration
(`func`, receiver, name, params, body) because these are always standalone declarations
in Go with no grouped form.

## Construct Status

| Construct           | Status         | Isolated | Keyword in source   | Refactoring ready | Notes                                                                                                                                                                    |
| ------------------- | -------------- | -------- | ------------------- | ----------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Functions           | complete       | yes      | yes (`func`)        | yes               | Atomic, standalone                                                                                                                                                       |
| Methods (pointer)   | complete       | yes      | yes (`func (r *T)`) | yes               | Receiver captured without `*`                                                                                                                                            |
| Methods (value)     | complete       | yes      | yes (`func (r T)`)  | yes               | Receiver captured as type name                                                                                                                                           |
| Types (single)      | complete       | yes      | no (normalized)     | yes               | `type` stripped                                                                                                                                                          |
| Types (grouped)     | complete       | yes      | no (normalized)     | yes               | Each type_spec isolated                                                                                                                                                  |
| Constants (single)  | complete       | yes      | no (normalized)     | yes               | `const` stripped                                                                                                                                                         |
| Constants (grouped) | complete       | yes      | no (normalized)     | yes               | Each const_spec isolated                                                                                                                                                 |
| Variables (single)  | complete       | yes      | no (normalized)     | yes               | `var` stripped                                                                                                                                                           |
| Variables (grouped) | complete       | yes      | no (normalized)     | yes               | Each var_spec isolated                                                                                                                                                   |
| `init()` functions  | complete       | yes      | yes                 | yes               | Engine dedups colliding names by appending `.from_<sourcefile>` (`engine_walk.go::dedupSuffix`). Multiple `init()` in the same package each get a stable, isolated path. |
| Generic functions   | complete       | yes      | yes                 | partial           | Source includes type params. Directory name lacks them (`Foo` not `Foo[T any]`). Acceptable for navigation.                                                              |
| Generic types       | complete       | yes      | no (normalized)     | partial           | Same as generic functions.                                                                                                                                               |
| Imports             | complete       | yes      | n/a                 | yes               | Captured under `{package}/imports/` per the Go schema; agents can list, read, add, and remove import entries.                                                            |
| Struct fields       | not decomposed | n/a      | n/a                 | n/a               | Fields live in the type source but aren't individually addressable. By design — coarse granularity is sufficient for current refactoring use cases.                      |
| Interface methods   | not decomposed | n/a      | n/a                 | n/a               | Method signatures live in the type source but aren't individually addressable. Same rationale as struct fields.                                                          |

## Filesystem Layout

```
{package}/
  functions/{name}/           source  hover  diagnostics  definitions  references
  methods/{Receiver}.{Name}/  source  hover  diagnostics  definitions  references
  types/{Name}/               source  hover  diagnostics  definitions  references
  constants/{name}/           source
  variables/{name}/           source
  imports/{path}/             source
```

The names after each directory are the files inside it, all siblings.

`source` is the only leaf declared inline. The four sibling files on
`functions/`, `methods/`, and `types/` come from the schema's `lsp` **file set** —
declared once under `file_sets` and pulled in per-node via `"include": ["lsp"]`,
which `Topology.ResolveIncludes` ([`api/schema.go`](../../api/schema.go)) expands
into each node's `Files` at parse time.

These four are only populated when the `.db` carries the `_lsp*` tables that
ley-line-open's LSP pass produces. On a plain `leyline parse` projection they
resolve empty — the paths exist, the content doesn't.

Constants, variables, and imports do **not** include the set, so they expose
`source` alone. That asymmetry is a property of the schema as written, not a
documented limitation of the LSP pass — gopls reports hover for constants and
variables too, so the set could likely be extended to them.

## Not Blocking

### Missing keyword in normalized constructs

By design. The normalization contract ensures consistent Agent behavior
across single and grouped declarations. The directory path
(`constants/`, `variables/`, `types/`) provides the semantic context.

### Generic type parameters not in directory name

`Foo[T any]` appears as `types/Foo/source`. The full generic syntax is
in the source content. Using `Foo` as the directory name is correct for
navigation and avoids filesystem-unfriendly characters (`[`, `]`).

### Struct fields / interface methods not decomposed

The type source contains the full definition. Individual field/method
addressing could be added as children of the type node if needed for
fine-grained refactoring.
