# schemas/

Cap'n Proto schemas vendored from
[ley-line-open/rs/ll-core/schema-capnp/schemas](https://github.com/agentic-research/ley-line-open/tree/main/rs/ll-core/schema-capnp/schemas).

These are the typed cross-runtime contract for Σ events
(decade `ley-line-open-9d30ac`, thread `T8/capnp-as-protocol`). LLO is
the canonical source; mache vendors the files because consumer-side
codegen wants them locally and a submodule would couple build sequencing.

## Files

| File            | Authoritative source             | Purpose                                                                    |
| --------------- | -------------------------------- | -------------------------------------------------------------------------- |
| `common.capnp`  | LLO file ID `0xb0c0debaadc0deb0` | `Position`, `Range`, `Hash`, `NodeRef`                                     |
| `binding.capnp` | LLO file ID `0x9c0c8cd3c5b1329a` | `BindingRecord` — LSP refs with both `constructNodeId` and `refSiteNodeId` |

## Sync rule

Struct bodies + `@N` field ordinals MUST stay byte-identical to the
LLO source of truth. The only allowed mache-side delta is the
three-line Go annotation block at the top:

```
using Go = import "/go.capnp";
$Go.package("bindings");
$Go.import("github.com/agentic-research/mache/internal/lsp/bindings");
```

These are consumer-specific metadata (Rust consumers don't see them),
not schema content. When LLO updates a schema, copy the new file in,
re-add the three Go annotation lines, then run `task gen-bindings`.

## Regenerating bindings

```
task gen-bindings
```

writes Go code to `internal/lsp/bindings/`. The task installs
`capnpc-go` if it's missing. Generated files are checked in so
contributors don't need the capnp toolchain to build.

Build prereq for regen: `capnp` binary (Cap'n Proto 1.3.0+) on PATH.
On macOS: `brew install capnp`.

## Why vendor instead of submodule

A submodule would couple `go build` to `git submodule update`, which
bites every fresh clone and CI runner. The schemas change at LLO's
release pace, so a manual sync-with-PR is fine. CI doesn't regenerate
on its own — divergence requires human review.
