package leyline

// RecordResolved publishes the provenance of a leyline binary resolved outside
// the managed-daemon path, so Provenance() reports it too.
//
// Provenance was populated only by DiscoverOrStart (socket.go), the daemon
// route. Every other caller of ResolveBinary — notably `mache build` via
// autoInvokeLeylineParse — resolved a binary, shelled out to it, and left
// Provenance() reporting "mache hasn't resolved one this process". That is a
// lie by omission in exactly the situation the provenance record exists for:
// mache had just run a leyline binary and could not say which.
//
// It matters beyond diagnostics. `mache build` writes a persisted .db, and the
// leyline that produced it determines how merkle-AST node addresses are DERIVED
// — LLO v0.11.0 bumped IR_SCHEMA_VERSION from merkle-ast-v1 to merkle-ast-v2,
// rewriting every Rust trait signature's node_hash from BYTE-IDENTICAL sources
// (ley-line-open #282). No content- or time-based staleness check can see that:
// mache's cache lockfiles hash raw source bytes, the parse skip is mtime+size,
// and _mache_meta recorded nothing about leyline at all. Two .db files built
// from identical sources under different leyline lineages were
// indistinguishable (mache-438104).
//
// Recording the resolved binary is the part of that mache can fix alone, today.
// Consuming a lineage tag published by LLO is the durable fix and is separate
// (mache-43d63d, blocked on ley-line-open-348de6).
func RecordResolved(path, source string) {
	if path == "" {
		return
	}
	recordResolvedLeyline(path, source)
}
