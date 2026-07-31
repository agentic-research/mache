package fixturedb

// Optional facts, stated declaratively.
//
// These are STRUCTS rather than functional options on purpose. Every field
// names something true about the code being modelled — the file it lives in,
// the construct that contains it, the deduped subtree it shares — and never a
// column, a table, or a fragment of SQL. A struct literal reads as a claim; a
// pile of one-line closures reads as configuration, and duplicates itself.

// Where states where a construct lives. The zero value means "infer it", which
// is the common case: [Builder.Construct] derives the source from the id's
// longest leading path prefix whose last segment has a file extension —
// "pkg/served_test.go/function_declaration_0" infers "pkg/served_test.go",
// which is exactly how ley-line composes these ids.
type Where struct {
	// Source pins the construct's source file instead of inferring it.
	Source SourceID
	// Name sets `nodes.name`. Defaults to the id's last path segment.
	Name string
	// Parent sets `nodes.parent_id`.
	Parent ConstructID
	// Directory marks a directory node (`nodes.kind` 0) rather than a leaf (1).
	Directory bool
}

// Detail states the optional facts about a definition, a reference or an `_ast`
// row. Fields that do not apply to the row kind are ignored, and every field is
// dropped on [Standalone] when the mache projection has no column for it.
type Detail struct {
	// Container is the construct a DEFINITION is nested inside —
	// `node_defs.container_node_id`, which ley-line populates for a method in an
	// impl/type block and leaves NULL for a top-level item.
	Container ConstructID

	// Subtree labels a deduped merkle subtree. Occurrences carrying the same
	// label share one node_hash; an empty label means "unique to this
	// occurrence".
	//
	// It is a NAME, not bytes. node_hash is one-to-many by construction (one
	// content row, many occurrences), and a fan-out test needs to say "these
	// are the same subtree" without any test ever writing a hash literal.
	Subtree string

	// Token is an `_ast` node's token text, which the builder stores in
	// `node_content` and points at from `_ast.node_hash` — the same indirection
	// ley-line uses, and the one v_test_nodes' attribute detection walks.
	Token string
}

// first returns the single optional value, or the zero value when none was
// given. Passing more than one is a caller error and the extras are ignored;
// the variadic exists only to make the argument optional.
func first[T any](vals []T) T {
	var zero T
	if len(vals) == 0 {
		return zero
	}
	return vals[0]
}

// label turns a Detail's subtree name into the hash label, defaulting to a
// per-occurrence unique one so distinct occurrences never collide by accident.
func (d Detail) label(unique string) string {
	if d.Subtree == "" {
		return unique
	}
	return "label:" + d.Subtree
}
