package fixturedb

import "database/sql"

// ConstructID identifies an enclosing DEFINITION — a function, method, type or
// module. On both producers this is what lands in `node_defs.node_id`.
type ConstructID string

// SiteID identifies one distinct OCCURRENCE inside a construct: a single call
// expression, not the function containing it. The empty SiteID means "an
// unnamed occurrence" and the builder synthesises a unique leaf path for it.
//
// SiteID exists as its own type because `node_refs.node_id` means a site on
// [Leyline] and a construct on [Standalone]. Making them two parameters of two
// types is the whole reason a migrated test can no longer be ambiguous about
// which producer it modelled.
type SiteID string

// SourceID is a source file's ley-line identity: its path RELATIVE to the parse
// root ("pkg/orphan.go"), never an absolute path and never a bare basename.
type SourceID string

// CanonicalKind is ley-line's closed κ vocabulary, written to
// `node_defs.canonical_kind`. It normalises per-language tree-sitter kinds
// (function_declaration / function_item / function_definition) to one value.
//
// [Standalone] has no column for it; a kind passed to a Standalone fixture is
// dropped, exactly as the mache projection drops it.
type CanonicalKind string

// The κ values ley-line emits. NoKind is for defs whose kind ley-line leaves
// NULL.
const (
	NoKind    CanonicalKind = ""
	Function  CanonicalKind = "function"
	Method    CanonicalKind = "method"
	Type      CanonicalKind = "type"
	Module    CanonicalKind = "module"
	Constant  CanonicalKind = "constant"
	Variable  CanonicalKind = "variable"
	Field     CanonicalKind = "field"
	Interface CanonicalKind = "interface"
)

// Span is a byte-and-row range in a source file, as `_ast` records it.
type Span struct {
	StartByte, EndByte int
	StartRow, StartCol int
	EndRow, EndCol     int
}

// Bytes is the common case: a byte range with rows/cols left at zero.
func Bytes(start, end int) Span { return Span{StartByte: start, EndByte: end} }

// RefsQuerier is the query surface a fixture presents. It is deliberately the
// same shape as cmd's refsQuerier so a fixture drops straight into the
// production call path.
type RefsQuerier interface {
	QueryRefs(query string, args ...any) (*sql.Rows, error)
}

// viewInstaller is the canonical-view installer the consuming package registers
// once, in an init(). fixturedb cannot import cmd (cmd imports fixturedb), and
// installing the views must not be a per-test responsibility — a forgotten
// install is precisely the class of bug this package exists to remove. So the
// consumer hands the installer over ONCE and [Builder.Build] applies it to every
// fixture it ever produces.
//
// Packages with no canonical views (internal/graph, internal/ingest) leave it
// unset and Build simply skips the step.
var viewInstaller func(RefsQuerier) error

// RegisterViewInstaller records the canonical-view installer for this test
// binary. Call it from an init() in the package under test; calling it twice is
// a programming error and panics rather than silently taking the last write.
func RegisterViewInstaller(install func(RefsQuerier) error) {
	if viewInstaller != nil {
		panic("fixturedb: view installer registered twice")
	}
	viewInstaller = install
}
