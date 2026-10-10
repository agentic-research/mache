package fixturedb

// dialect is everything about a fixture that depends on which producer it
// models: the DDL, and how each spec becomes columns.
//
// Every [Producer] carries exactly one, so the producer difference is a METHOD
// a dialect implements, never a branch a writer takes. That is what makes the
// set exhaustive. The writers used to ask `producer != Leyline`, so a third
// producer silently fell into the Standalone arm and wrote mache-schema rows
// into whatever table set it had; the v6 producer would have been the first to
// do it. Now a producer whose dialect lacks a writer does not compile, and one
// with no dialect at all is refused by [New].
type dialect interface {
	// schema returns the producer's DDL for this fixture, in creation order.
	schema(b *Builder) []string
	// hasNodeContent reports whether this fixture has a node_content table to
	// point node_hash values at.
	hasNodeContent(b *Builder) bool

	emitNodes(e *emitter)
	// emitSymbols writes node_defs and node_refs.
	emitSymbols(e *emitter)
	emitAST(e *emitter)
	emitSources(e *emitter)
	emitImports(e *emitter)
}
