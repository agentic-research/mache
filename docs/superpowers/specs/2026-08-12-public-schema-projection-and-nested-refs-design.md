# Public Schema Projection and Nested Address Refs

**Beads:** `mache-734971`, `mache-498bc3`

## Problem

PR #621 exposes the low-level ingestion pieces, but a library caller still has
to duplicate the CLI's schema-build recipe: resolve a schema, run the pinned
leyline parser into a temporary database, tune the read connection, check
grammar coverage, wire `Engine` to `ASTWalker` and `SQLiteWriter`, ingest, and
close every resource in the correct order. That duplication lets library and
CLI behavior drift.

The current recipe also loses typed address references for nested files.
Leyline correctly records root-relative `_source.id` values such as
`sub/nested.go`, and `Engine.sourceIDFor` computes the same value, but the
whole-file address-ref call passes a filesystem path to
`ExtractAddressRefs`, which reduces it to `nested.go`. The lookup therefore
misses the nested file's AST rows. This affects every registered address-ref
scheme, including `gomod:`, Terraform `mod:`, and `env:`.

## Ownership

This is Mache work. Leyline already preserves root-relative source identity,
parses the relevant Go and Terraform syntax, and exposes the parse operation
Mache needs without CGO. No ley-line-open change or bead is required.

## Public API

Introduce a top-level `schema` package as the single owner of bundled presets
and schema-reference resolution:

```go
package schema

type Resolution struct {
    Topology  *api.Topology
    Languages []string
}

func ParseTopology(data []byte) (*api.Topology, error)
func Resolve(ref, baseDir string) (*Resolution, error)
func LoadPreset(name string) (*Resolution, error)
func AvailablePresets() []string
```

`Resolve` accepts a built-in preset name, an absolute path, or a relative path
contained by `baseDir`. `Resolution.Languages` carries the preset language
identity used by the missing-grammar guard; file schemas derive languages from
their `Node.Language` fields. The preset JSON files move under this package so
there is one embedded source of truth. The CLI delegates to this package.

The longer names distinguish topology decoding from `build.Parse` and avoid a
second `PresetNames` definition in the CLI facade; the repository smell gate
surfaced both ambiguities during implementation.

Extend the public `build` package with two levels of entry point:

```go
func ParseWithSchema(source, output string, topology *api.Topology) error
func ParseWithSchemaRef(source, output, ref, baseDir string) error
```

`ParseWithSchema` serves callers that construct or parse a topology
themselves. `ParseWithSchemaRef` serves callers that want a bundled preset or
portable file reference and retains preset language metadata. Both use the
pinned leyline executable and remain CGO-free.

The public build path owns temporary parse creation and cleanup, SQLite read
tuning, grammar-coverage validation, writer lifecycle, and projection. A
schema selected through either public function is explicit, so a grammar gap
is an error rather than a hollow build. The library does not stamp CLI binary
provenance or emit CLI warnings; the CLI keeps those responsibilities after
the shared build call succeeds.

## CLI Delegation

`mache build --schema REF` resolves `REF` through the public schema package and
calls the public projection implementation. Existing metadata, empty-build
warnings, overwrite behavior, and error wording remain covered by CLI tests.
Private copies of preset loading and the projection recipe are removed.

Mount and serve schema resolution also delegate to the public schema package,
so every entry point sees the same presets and path-containment behavior.

## Nested Source Identity

`ASTWalker.ExtractAddressRefs` accepts a leyline source ID, not an arbitrary
filesystem path. It must query that ID unchanged. `Engine` passes the
root-relative `sourceID` it already computed for context, imports, file-level
refs, and AST projection. This removes the basename collision and aligns the
whole-file and scoped address-ref APIs.

## Verification

Tests start at the public or CLI production boundary, not only at the unit
walker:

- A public preset-resolution test proves `go` resolves and unknown or escaping
  references fail.
- A `build.ParseWithSchema` external-package test proves a caller-supplied
  topology produces a queryable projected database.
- A `build.ParseWithSchemaRef(..., "go", ...)` regression builds root and
  nested Go files and asserts both files have content plus both `gomod:` tokens
  in `node_refs`.
- A Terraform preset regression builds a nested module file and asserts its
  `mod:` token, demonstrating the fix applies to the registry rather than a
  Go-specific branch.
- A CLI regression exercises `mache build --schema go` and makes the same
  nested-token assertion, proving delegation is wired.
- Existing schema-resolution, coverage, ingestion, and CLI suites remain
  green, followed by `task check`, `task build`, and `task install`.

## Documentation

Update the README, architecture documentation, and changelog to describe the
public schema projection API, preset resolution, pinned leyline boundary, and
root-relative address-ref invariant. Clean up the inaccurate comments exposed
by PR #621 (`NewASTWalker` receives an open DB; it does not open one).
