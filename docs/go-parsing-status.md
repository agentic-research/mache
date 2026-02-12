# Go Parsing Status

Tracks what the Go schema (`examples/go-schema.json`) captures, its limitations,
and the contracts that the write-back path (ADR-004) must respect.

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

| Construct | Status | Isolated | Keyword in source | Refactoring ready | Notes |
|-----------|--------|----------|-------------------|-------------------|-------|
| Functions | complete | yes | yes (`func`) | yes | Atomic, standalone |
| Methods (pointer) | complete | yes | yes (`func (r *T)`) | yes | Receiver captured without `*` |
| Methods (value) | complete | yes | yes (`func (r T)`) | yes | Receiver captured as type name |
| Types (single) | complete | yes | no (normalized) | yes | `type` stripped |
| Types (grouped) | complete | yes | no (normalized) | yes | Each type_spec isolated |
| Constants (single) | complete | yes | no (normalized) | yes | `const` stripped |
| Constants (grouped) | complete | yes | no (normalized) | yes | Each const_spec isolated |
| Variables (single) | complete | yes | no (normalized) | yes | `var` stripped |
| Variables (grouped) | complete | yes | no (normalized) | yes | Each var_spec isolated |
| `init()` functions | partial | **no** | yes | **no** | Multiple init() in same package collide (last-writer-wins). Needs engine-level dedup. |
| Generic functions | complete | yes | yes | partial | Source includes type params. Directory name lacks them (`Foo` not `Foo[T any]`). Acceptable for navigation. |
| Generic types | complete | yes | no (normalized) | partial | Same as generic functions. |
| Imports | **not captured** | n/a | n/a | **no** | Cannot add/remove imports during refactoring. Needs `/imports/` directory or dedicated mechanism. |
| Struct fields | **not decomposed** | n/a | n/a | n/a | Fields are in the type source but not individually addressable. |
| Interface methods | **not decomposed** | n/a | n/a | n/a | Method signatures are in the type source but not individually addressable. |

## Filesystem Layout

```
{package}/
  functions/{name}/source
  methods/{Receiver}.{Name}/source
  types/{Name}/source
  constants/{name}/source
  variables/{name}/source
```

## Blocking Issues for Refactoring

### 1. `init()` collision (engine-level)

Multiple `init()` functions in the same package produce the same path
(`{pkg}/functions/init/source`). The last file processed wins, silently
dropping earlier init functions. Fix requires engine-level same-name dedup
(e.g., counter suffix, filename suffix).

### 2. Imports not captured

An Agent that renames `http.Get` to `client.Get` cannot add the new import.
Options:
- Add `{package}/imports/` virtual directory listing each import
- Add a writable `{package}/imports` file mapping to the import block
- Rely on external tooling (`goimports`) as a post-write hook

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
