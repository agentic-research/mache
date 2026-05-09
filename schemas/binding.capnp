@0x9c0c8cd3c5b1329a;
using Go = import "/go.capnp";
$Go.package("bindings");
$Go.import("github.com/agentic-research/mache/internal/lsp/bindings");
# BindingRecord — a single LSP-derived reference projected into Σ.
#
# T8.2 (decade ley-line-open-9d30ac, thread T8/capnp-as-protocol).
# Producer: rs/ll-open/lsp/src/project.rs::project_references.
# Consumer: any process reading the per-db binding event log
#   (`${db_path}.bindings.capnp`); initially LLO itself + mache's
#   canonical-view migration in T8.4.
#
# Schema decision (post-cdcae2 alignment-options comment, 2026-05-08):
# emit BOTH the construct-level enclosing node AND the leaf ref-site
# node. Schema-as-data-plane lets each consumer pick its own level —
# `find_callers` MCP wants `constructNodeId` (function/method scope);
# byte-precise tooling wants `refSiteNodeId`. Eliminates the
# "one column, two semantics" drift that broke Falsifiability B
# at the SQL boundary.

using Common = import "common.capnp";

struct BindingRecord {
  # The target the LSP resolved to (the symbol's defining node).
  # Same as `_lsp_refs.node_id` in the SQL projection.
  targetNodeId @0 :Text;

  # The textual lemma at the ref site (e.g. "Validate").
  refToken @1 :Text;

  # Where the reference lives, in two distinct AST levels:
  #
  # `constructNodeId` — smallest enclosing function/method/constructor
  # declaration. What `find_callers` MCP wants and what
  # `node_refs.node_id` records (after construct-level normalization).
  # Empty string when the ref site has no enclosing construct in
  # `_ast` (e.g. top-level let-binding, cross-repo reference whose
  # source isn't projected). Construct kinds matched: `function_*`,
  # `method_*`, `*_definition`, `*_declaration` per language; full
  # list curated in CONSTRUCT_KINDS in lsp/src/project.rs.
  constructNodeId @2 :Text;

  # `refSiteNodeId` — the smallest enclosing AST node at the ref
  # location. Typically a leaf `(type_)?identifier` or
  # `field_identifier`. Most precise locator, but can sit several
  # levels below `constructNodeId`. Empty string under the same
  # missing-_ast conditions as above.
  refSiteNodeId @3 :Text;

  # The file URI the ref appears in (canonicalized; matches
  # `_source.path` post-be6136). `file://` scheme.
  refUri @4 :Text;

  # The reference-site range (for byte-precise tooling).
  refRange @5 :Common.Range;

  # Causality — the parse generation that emitted this record. Lets
  # consumers correlate a binding to a specific Σ root advance.
  # T8.5 will hash segments per-generation into Σ root.
  parseGen @6 :UInt64;
}
