@0xa1b2c3d4e5f60001;

using Go = import "/go.capnp";
$Go.package("schema");
$Go.import("github.com/agentic-research/mache/internal/leyline/schema");

# Daemon protocol schema — the contract between ley-line daemon (Rust)
# and mache (Go) over the UDS control socket.
#
# This is the single source of truth. Both sides generate types from
# this file. Schema drift is a build error, not a runtime bug.

# ── Data types ────────────────────────────────────────────────────

struct Node {
  id       @0 :Text;
  parentId @1 :Text;
  name     @2 :Text;
  kind     @3 :Int32;   # 0=file, 1=dir
  size     @4 :Int64;
  record   @5 :Text;    # JSON-encoded content or metadata
}

struct Ref {
  nodeId   @0 :Text;
  sourceId @1 :Text;
}

struct ParseStats {
  parsed       @0 :UInt64;
  unchanged    @1 :UInt64;
  deleted      @2 :UInt64;
  errors       @3 :UInt64;
  changedFiles @4 :List(Text);
}

struct EnrichmentStats {
  passName       @0 :Text;
  filesProcessed @1 :UInt64;
  itemsAdded     @2 :UInt64;
  durationMs     @3 :UInt64;
}

struct Event {
  seq    @0 :UInt64;
  topic  @1 :Text;
  source @2 :Text;
  data   @3 :Text;   # JSON-encoded payload (flexible per topic)
}

struct QueryRow {
  values @0 :List(Text);
}

# ── Request / Response pairs ──────────────────────────────────────
#
# The UDS protocol is request-response (one request, one response per
# line). Each request has an "op" field. These structs define the
# typed shape for each op.

struct StatusResponse {
  ok         @0 :Bool;
  generation @1 :UInt64;
  arenaPath  @2 :Text;
  arenaSize  @3 :UInt64;
}

struct ReparseRequest {
  source @0 :Text;
  lang   @1 :Text;
}

struct ReparseResponse {
  ok         @0 :Bool;
  generation @1 :UInt64;
  stats      @2 :ParseStats;
}

struct SnapshotResponse {
  ok         @0 :Bool;
  generation @1 :UInt64;
}

struct EnrichRequest {
  pass  @0 :Text;
  files @1 :List(Text);
}

struct EnrichResponse {
  ok         @0 :Bool;
  generation @1 :UInt64;
  passes     @2 :List(EnrichmentStats);
}

struct LoadRequest {
  db @0 :Data;   # raw .db bytes (not base64 — capnp handles binary)
}

struct LoadResponse {
  ok         @0 :Bool;
  generation @1 :UInt64;
}

struct QueryRequest {
  sql @0 :Text;
}

struct QueryResponse {
  ok      @0 :Bool;
  columns @1 :List(Text);
  rows    @2 :List(QueryRow);
}

struct ListChildrenRequest {
  id @0 :Text;
}

struct ListChildrenResponse {
  ok       @0 :Bool;
  children @1 :List(Node);
}

struct ReadContentRequest {
  id @0 :Text;
}

struct ReadContentResponse {
  ok      @0 :Bool;
  content @1 :Text;
  error   @2 :Text;
}

struct FindCallersRequest {
  token @0 :Text;
}

struct FindCallersResponse {
  ok      @0 :Bool;
  callers @1 :List(Ref);
}

struct FindDefsRequest {
  token @0 :Text;
}

struct FindDefsResponse {
  ok   @0 :Bool;
  defs @1 :List(Ref);
}

struct GetNodeRequest {
  id @0 :Text;
}

struct GetNodeResponse {
  ok    @0 :Bool;
  node  @1 :Node;
  error @2 :Text;
}

struct SubscribeRequest {
  topics   @0 :List(Text);
  identity @1 :Text;
  since    @2 :UInt64;
}

struct SubscribeResponse {
  ok           @0 :Bool;
  headSeq      @1 :UInt64;
  replayCount  @2 :UInt64;
  replayGap    @3 :Bool;
}

struct ErrorResponse {
  error @0 :Text;
}
