@0xb0c0debaadc0deb0;
using Go = import "/go.capnp";
$Go.package("bindings");
$Go.import("github.com/agentic-research/mache/internal/lsp/bindings");
# Σ Merkle-CAS substrate — common primitives.
#
# T8 (decade ley-line-open-9d30ac, thread T8/capnp-as-protocol).
#
# Ordinals are stable. Adding fields: append at next ordinal with
# default. Renaming or removing: never — leave a hole. See
# docs/adr/0014-capnp-as-protocol.md (T8.6) for the schema-evolution
# contract.

# Source position. `byte` is the canonical ordering field; `line`/`column`
# are derivable but stored for query speed.
struct Position {
  line @0 :UInt32;
  column @1 :UInt32;
  byte @2 :UInt64;
}

# Inclusive-start, exclusive-end byte range — matches tree-sitter's
# convention.
struct Range {
  start @0 :Position;
  end @1 :Position;
}

# 32-byte BLAKE3 hash. Locked to BLAKE3 per Σ §3.4 (decade write-up
# 2026-merkle-cas-substrate.md). Not a placeholder for arbitrary digest.
struct Hash {
  bytes @0 :Data;  # MUST be exactly 32 bytes
}

# A reference *into* a parsed source tree — used to identify the site
# of a definition, reference, or AST node from outside the producer.
struct NodeRef {
  sourceId @0 :Text;   # _source.id (relative path, stable per repo)
  nodeId @1 :Text;     # _ast.node_id (stable per parse run)
  range @2 :Range;     # for lookup-only refs whose nodeId is unresolved
}
