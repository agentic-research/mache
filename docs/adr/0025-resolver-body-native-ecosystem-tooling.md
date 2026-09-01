---
title: "ADR-0025: Resolver Bodies Use Each Ecosystem's Own Tooling, Not Hand-Rolled Manifest Parsing"
status: Accepted
date: 2026-08-04
tags: [architecture, cross-language, references, resolver, dependencies]
---

**Relates to:**

- ADR-0016 (Cross-Language Reference Resolver — defines the `Resolver`
  interface, `Registry`, and the open scheme registry this ADR's pattern
  fills in)
- Bead `mache-q43l` (cross-language reference graph epic)
- Bead `mache-bd97d9` (the `Resolver`/`Registry` package this ADR's
  `GoModResolver` is the second implementation of)
- Bead `mache-e6d582` (this ADR's implementation)
- Bead `mache-04972b` (`graph.Open` / `mache-3edd21` `graph.Build` — the
  public library facades this ADR's resolver body is built on)

## Context

ADR-0016 defines *what* a resolver is (a Kleisli arrow from a locator to a
graph, keyed by scheme in an open registry) but deliberately leaves *how*
a resolver's body is implemented as a per-scheme decision. Its own
`Address.Scheme` enum already names `gomod` alongside `mod`, `npm`, `oci`,
`openapi`, `git`, `http` — none of the ecosystem-manifest schemes
(`gomod`, and by the same argument `npm`, `cargo`, `maven`, `pip`) had an
implementation until this ADR.

This gap surfaced from a concrete external case, not a hypothetical: a
personal tool (built independently of mache, by mache's own maintainer)
does cross-cutting dependency/architecture tracing using several
separate, hand-rolled per-ecosystem integrations —
`golang.org/x/mod/modfile` for Go module resolution,
`hashicorp/hcl/v2` for Terraform "deploy references code" tracing,
a protobuf parser for event-schema tracing, and mache itself for
in-language call-site tracing. The question this ADR answers: when mache
grows a `gomod:` resolver (and later `npm:`, `cargo:`, ...), should it
parse `go.mod`/`go.sum` itself, or something else?

Two of the four sources in that motivating tool are not actually a gap.
Protobuf and HCL/Terraform are both registered LLO tree-sitter grammars,
and mache already layers semantic construct extraction on top via
declarative preset schemas (`cmd/schemas/protobuf.json` distinguishes
`messages`/`services`/`rpcs`/`enums`, not just generic AST tokens;
`cmd/schemas/terraform.json` is the equivalent for HCL). Those are
**source-shaped specs** — another language for LLO's grammar +
mache's schema-selector layer to project, no new mechanism needed.

`go.mod` is not source-shaped in the same sense. It is a **build/dependency
manifest** — its meaning is defined by Go's own module-resolution
algorithm (semver selection, `replace` directives, workspace boundaries,
the module cache layout), not by a grammar. The same is true of
`Cargo.toml`+`Cargo.lock`, `package.json`+lockfiles, `pom.xml`, and
`pyproject.toml`: each encodes a *resolution algorithm* specific to its
ecosystem, not just a syntax tree. Parsing the manifest's syntax (which
tree-sitter can do) answers a different question than resolving what an
import path actually points to.

LLO's own architecture doesn't propose solving this either. Its ADR-0023
("agent-first language facts," Proposed, unimplemented, 2026-06-24)
covers a different problem (tier-1 semantic facts — types, cross-file
bindings) and, tellingly, assigns each ecosystem's tier-1 integration to
bespoke code in that ecosystem's native runtime: *"Phase 2 — Terraform
(tier 1, mache-side)... `github.com/hashicorp/hcl/v2`...
Implementation home: mache (Go). HCL is Go-native."* That is the same
principle this ADR applies to manifest resolution, independently arrived
at on the LLO side. Neither LLO nor ADR-0023 mentions module/dependency
resolution (`go.mod`, `Cargo.toml`, `package.json`) at all — this ADR
does not duplicate anything either project has built or planned.

## Decision

**A resolver's body shells out to the ecosystem's own first-party
resolution tool and consumes its structured output — it does not
reimplement that ecosystem's manifest/lockfile resolution semantics.**

Concretely, for `gomod:`:

- `GoModResolver.Resolve` runs `go list -json <import-path>` from a
  caller-supplied `WorkDir`. `go list` is Go's own module-resolution
  tool — it already gets `replace` directives, minimum-version selection,
  and the module cache right; a `golang.org/x/mod/modfile`-based
  reimplementation would either duplicate that logic or drift from it the
  first time `go list`'s behavior changes.
- The relevant slice of `go list -json`'s output (here, just the
  resolved package's `Dir`) is decoded with a narrow, purpose-built
  struct (`goListPackage`) — not modelled exhaustively. A resolver that
  doesn't need a field shouldn't carry it.
- Once `go list` names the resolved directory, that directory is a
  **source-shaped** thing again — an ordinary Go package on disk — so it
  re-enters the path this session already built:
  `graph.Build(dir, tmpDB)` (leyline parse, the public library facade
  from `mache-3edd21`) followed by `graph.Open(tmpDB)` (`mache-04972b`).
  The resolver returns a real `graph.Graph` — `LookupDef`/`QueryRefs`/
  `GetCallers` work on it immediately, same as any other mache-produced
  `.db`, because it *is* one.

This generalizes directly to the schemes ADR-0016 leaves open, because
the pattern — not the Go specifics — is what repeats:

| Scheme  | Native resolution tool                                                                                  | Structured output |
| ------- | ------------------------------------------------------------------------------------------------------- | ----------------- |
| `gomod` | `go list -json <path>`                                                                                  | JSON (one object) |
| `npm`   | `npm ls --all --json`, or read `package.json`/`package-lock.json` directly (already JSON)               | JSON              |
| `cargo` | `cargo metadata --format-version=1`                                                                     | JSON              |
| `maven` | `mvn dependency:tree -DoutputType=json` (or `mvn -q dependency:build-classpath`)                        | JSON or text      |
| `pip`   | `pip show <pkg>` / `python -m pip show`, or parse `pyproject.toml` (TOML, not resolution-bearing alone) | key-value / JSON  |

Most of these already speak JSON on request — which is why the earlier
framing of this problem (session notes, 2026-08-04) reached for
`ingest.JsonWalker`. That instinct was half right and half a category
error, corrected here: `JsonWalker` is for **projecting a JSON
document's own contents as browsable graph nodes** (JSONPath-driven
schema ingestion — see its use in `examples/` JSON-source schemas). A
`go list`/`cargo metadata` response is not content to browse; it is
**metadata used once, to find a directory**, after which the actual
content-to-browse is the resolved source tree, ingested through the
normal `graph.Build` source-parsing path. Using `JsonWalker` here would
have been ingesting the wrong artifact. `encoding/json.Unmarshal` into a
narrow struct is the right-sized tool for metadata; `JsonWalker` remains
the right tool on the day mache wants to expose "browse my resolved
dependency graph as a directory tree" as its own view — a different,
later feature, not required for resolution to work.

## Consequences

### Positive

- **Correctness tracks the ecosystem, not mache.** `go list`'s semver
  and `replace`-directive behavior is Go's committed public contract; a
  hand-rolled reimplementation would need to track every change to it.
  Delegating means mache's `gomod:` resolution is exactly as correct as
  the Go toolchain installed wherever mache runs — including changes
  neither this ADR nor its author has to track.
- **The resolved graph is a first-class mache graph, not a shadow
  structure.** Because resolution ends in `graph.Build`+`graph.Open`
  (not a bespoke in-memory representation), every existing MCP tool
  (`find_definition`, `find_callers`, `get_overview`, ...) works on a
  resolved dependency exactly as it does on the primary source tree —
  no parallel API surface to maintain.
- **The pattern is the reusable unit, not the code.** Adding `npm:` or
  `cargo:` is "find that ecosystem's JSON-emitting tool, decode the one
  field needed, call `graph.Build`+`graph.Open`" — an afternoon per
  scheme, not a new subsystem per scheme.

### Negative

- **A working ecosystem toolchain is now a runtime dependency for that
  scheme.** `GoModResolver` requires `go` on `PATH`; an `npm:` resolver
  will require `node`/`npm`. This is a different failure mode than
  today's leyline-only dependency and needs its own clear error (this
  implementation returns `ErrNotResolvable`, not a bare exec error, when
  `go list` fails — but a missing toolchain and an unresolvable import
  path are still collapsed into one error class; distinguishing them is
  left to a follow-up if a consumer needs to).
- **Subprocess cost per resolution.** `go list` plus a full `leyline parse` of the resolved package is not free. `GoModResolver` caches by
  import path and coalesces concurrent resolutions with `singleflight`
  (matching the pattern `LocalPathResolver`, ADR-0016's sibling bead
  `mache-bdcd2b`, already commits to), but the first resolution of any
  given import path pays full cost. No eviction yet — same V1 scope
  `bdcd2b` accepts for its own cache.
- **No sandboxing of the invoked tool.** `go list` (and any future
  `npm`/`cargo` equivalents) runs with mache's own process privileges
  against a caller-supplied `WorkDir`. This is the same trust boundary
  `leyline parse` already crosses (mache already shells out to a pinned
  binary); it is not a new class of exposure, but it is not reduced
  either.

### Reversibility

Fully additive. Each scheme's resolver is independent — `gomod:` shipping
does not commit `npm:`/`cargo:` to this same pattern, though nothing here
suggests they should differ. If a future scheme's native tool has no
usable structured-output mode, that scheme's resolver falls outside this
ADR's pattern and needs its own decision; the pattern is a strong default,
not a universal law.

## Implementation

- `internal/resolve/resolver.go` — `Resolver`/`Registry` (`mache-bd97d9`,
  unmodified by this ADR — this ADR is about resolver bodies, not the
  contract).
- `internal/resolve/gomod_resolver.go` — `GoModResolver`, this ADR's
  worked example (`mache-e6d582`).
- Wiring `gomod:` into `resolve_ref`/`CompositeGraph` mounting
  (ADR-0016's `mache-be0b9f`) is out of scope here — this ADR and its
  bead cover the resolver in isolation, testable without the MCP layer.

## References

1. ADR-0016 — Cross-Language Reference Resolver (the `Resolver` contract
   this ADR's pattern implements against).
1. ADR-0023 (LLO, `~/remotes/art/ley-line-open/docs/adr/0023-agent-first-language-facts.md`)
   — Proposed/unimplemented; independently assigns ecosystem-native
   tier-1 integrations to bespoke per-ecosystem code, the same principle
   this ADR applies to manifest resolution.
1. `go help list` — `-json` output format.
1. `cargo metadata` — https://doc.rust-lang.org/cargo/commands/cargo-metadata.html
1. `npm ls` — https://docs.npmjs.com/cli/v10/commands/npm-ls
