---
status: current
covers-version: v0.21.1
last-verified: 2026-09-10
sources-of-truth:
  - schema/presets/rust.json
  - internal/ingest/ast_walker_selector.go
  - internal/ingest/engine_walk.go
audience: [agents, contributors]
---

# Rust Schema Reference

Reference for the Rust preset (`schema/presets/rust.json`, mirrored by
`examples/rust-schema.json`) — what it projects and the naming rules an agent
can rely on. Companion to [`go.md`](go.md).

## What lands where

| Directory          | Construct                                                       | Node name                   | `source`                       |
| ------------------ | --------------------------------------------------------------- | --------------------------- | ------------------------------ |
| `functions/`       | `fn` at file level, inside a `mod`, or nested in a block        | `{name}`                    | the `fn` item                  |
| `methods/`         | `fn` inside an `impl` block or a `trait` (including defaults)   | `{Receiver}.{name}`         | that one `fn` — never the block |
| `implementations/` | each `impl` block, as an index by receiver                      | `{Receiver}`                | none (LSP files only)          |
| `structs/` `enums/` `traits/` `type_aliases/` `constants/` `statics/` `modules/` `macros/` `imports/` | the item | `{name}` | the item |

Names that collide across files get `.from_<file>` suffixes, as everywhere in
mache. `fn new` in twenty impls is twenty `methods/<Type>.new` nodes, not one
node plus nineteen suffixes (`mache-c777ef`).

## The receiver

`{Receiver}` is the **type identifier** of the impl's self type, with generic
arguments, references, and module paths stripped:

| Source                                        | `methods/`            | `implementations/` |
| --------------------------------------------- | --------------------- | ------------------ |
| `impl Grid`                                   | `Grid.new`            | `Grid`             |
| `impl<T: Clone> Cell<T>`                      | `Cell.new`            | `Cell`             |
| `impl geometry::Point`                        | `Point.new`           | `Point`            |
| `impl<'a> IntoIterator for &'a Grid`          | `Grid.into_iter`      | `Grid`             |
| `impl<'a, T> fmt::Debug for &'a Cell<T>`      | `Cell.fmt`            | `Cell`             |
| `impl<T> Default for geometry::Vec2<T>`       | `Vec2.default`        | `Vec2`             |
| `trait Hash32 { fn describe(&self) … }`       | `Hash32.describe`     | —                  |

When the self type has no type identifier — `impl Hash32 for [u8]`, `for u32`,
`for (u8, u8)` — the receiver is the type's own source text: `[u8].hash32`,
`u32.hash32`, `(u8, u8).hash32`.

The trait being implemented is not part of the name: `Cell.fmt` says nothing
about `fmt::Debug`. Two impls of different traits that share a method name on
the same receiver in one file collide and get a suffix.

## How the preset spells it

Each receiver shape is one selector under `methods/`, listed most-specific
first and ending in a catch-all:

```
(impl_item type: (type_identifier) @receiver                                  body: …)
(impl_item type: (_ type: (type_identifier) @receiver)                        body: …)
(impl_item type: (_ name: (type_identifier) @receiver)                        body: …)
(impl_item type: (_ type: (_ type: (type_identifier) @receiver))              body: …)
(impl_item type: (_ type: (_ name: (type_identifier) @receiver))              body: …)
(impl_item type: (_ type: (_ type: (_ name: (type_identifier) @receiver)))    body: …)
(impl_item type: (_) @receiver                                                body: …)
```

with `body: (declaration_list (function_item name: (identifier) @name) @scope)`.
Two selector-engine rules make this work, and apply to every source preset:

- **`(_)` is a wildcard step** — tree-sitter's "any named node". It still
  honours its field label, so `(_ type: …)` and `(_ name: …)` are different
  steps. The outer node of a selector cannot be a wildcard.
- **Sibling schema nodes are an ordered choice.** The first sibling (in schema
  order) to match a construct under a parent owns it; later siblings skip it.
  That is why the catch-all never re-projects a method a specific shape already
  named. `$` containers are not part of the choice.

A selector whose inner `@scope` path reaches nothing is **not a match** — a
`mod` with no functions does not project as `functions/<mod>`.

## Pinned by

- `internal/schemainfer/rust_preset_test.go` — the table above, on
  `internal/testutil/testdata/preset_fixtures/rust/`; and on the
  `medium-rust-rosary` corpus, every `function_item` ley-line parses is projected
  exactly once, as a function or a method.
- `internal/ingest/ast_walker_wildcard_test.go` — the two selector-engine rules
  and the no-fallback rule, each mutation-verified.
