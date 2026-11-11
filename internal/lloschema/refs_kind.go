package lloschema

import (
	"slices"
	"strings"
)

// From leyline extraction epoch 6 (shipped in v0.29.0, epoch 7) node_refs
// holds VALUE refs beside call refs, told apart by the occurrence's grammar
// kind (node_refs.node_kind), never by token (TABLE_CONTRACT, node_refs):
//
//   - a CALL ref is anchored at the call: call_expression (Go, Rust, C, C++,
//     JavaScript, TypeScript), Python's call, Java's method_invocation and
//     object_creation_expression, Rust's macro_invocation, Bash's command —
//     the anchors of every language's "Call-target references" queries;
//   - a VALUE ref is a name in value position (an argument, a declaration or
//     assignment right-hand side, a return, a literal element, a method
//     value), anchored at that container: argument_list, var_spec,
//     return_statement, keyed_element, let_declaration and more. Value refs
//     exist in Go, Rust, Python, JavaScript, TypeScript, Java, C and C++, and
//     in those languages every ref that is not a call is one.
//
// Any other row is neither: a markdown backtick mention or an injected
// occurrence (no kind), SQL's table uses, HCL's typed addresses.
//
// A value use is a use, so the rules that ask "is it used?" (dead_code,
// untested_function) read every ref. What counts how much a construct calls,
// or ranks what is referenced most, leaves value refs out: fan_out_skew and
// RefsMap (most-referenced symbols, dependency layers, communities, the
// pack's entry points). Otherwise every local passed or returned (err, ctx,
// x) is a receiver and a hub (mache-c5e2f9).
var (
	callRefKinds = []string{
		"call",
		"call_expression",
		"command",
		"macro_invocation",
		"method_invocation",
		"object_creation_expression",
	}
	valueRefLangs = []string{"c", "cpp", "go", "java", "javascript", "python", "rust", "typescript"}
)

// IsValueRef reports whether a node_refs row whose occurrence has grammar
// kind nodeKind in language lang is a value ref.
func IsValueRef(nodeKind, lang string) bool {
	return nodeKind != "" && !slices.Contains(callRefKinds, nodeKind) && slices.Contains(valueRefLangs, lang)
}

// ValueRefSQL is IsValueRef as a SQL condition over the expressions kind and
// lang. It is never NULL: a row without a kind or a language is not a value
// ref.
func ValueRefSQL(kind, lang string) string {
	return "COALESCE(" + kind + " NOT IN " + sqlList(callRefKinds) + " AND " + lang + " IN " + sqlList(valueRefLangs) + ", 0)"
}

func sqlList(vs []string) string {
	return "('" + strings.Join(vs, "', '") + "')"
}
