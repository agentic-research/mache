package ingest

func init() {
	// Context extraction is Go-only; other languages return nil.
	RegisterContextQuery("go", `
		(import_declaration) @ctx
		(const_declaration) @ctx
		(var_declaration) @ctx
		(type_declaration) @ctx
	`)

	// Register Go qualified call query — captures both @call and @pkg.
	// Pattern 0: bare calls like foo()
	// Pattern 1: qualified calls like auth.Validate()
	RegisterQualifiedCallQuery("go", `
		(call_expression function: (identifier) @call)
		(call_expression function: (selector_expression
			operand: (identifier) @pkg
			field: (field_identifier) @call))
	`)

	// Register Go ref query (used by ExtractCalls, the bare-token
	// path that populates node_refs). Mirrors defaultCallQuery for
	// call_expression and adds the keyed_element pattern so
	// dead_code stops flagging cobra RunE callbacks (mache-02r9):
	//
	//   &cobra.Command{ RunE: runServe }   — keyed_element value
	//
	// We capture the IDENTIFIER inside the SECOND literal_element of
	// a keyed_element (the value), not the first (the field name).
	// Field-name identifiers like "RunE" rarely match defined-token
	// shapes anyway, but this query is precise so node_refs stays
	// signal-rich.
	//
	// Earlier iterations of this query also matched
	// assignment_statement and short_var_declaration RHS identifiers
	// to catch 'factories[k] = goFactory' and 'h := someFn' shapes
	// — but those over-collected (every 'filtered := findings[:0]'
	// in normal Go code added 'findings' to node_refs as a
	// 'callee'), inflating fan_out_skew and add scoring noise to
	// every other rule. Drop those two patterns; the remaining
	// keyed_element shape captures the highest-value cobra/init
	// case without the over-capture.
	//
	// Patterns fire on whatever scope ExtractCalls is given.
	// Function-body scopes catch nested cobra literals; file-level
	// cases (top-level var declarations) still need a separate pass
	// — known follow-up under mache-02r9.
	//
	// Type-reference patterns (mache-9c615a): bench surfaced that
	// `find_callers(Topology)` returned [] despite 141 grep hits.
	// Root cause was that node_refs only indexed call_expression,
	// so type usages — struct fields, function parameters, composite
	// literals, pointer/slice/map element types — never landed in
	// the index. Agent asking "who uses this type?" got silent empty.
	//
	// Each pattern below targets a type POSITION where the matched
	// (type_identifier) can never be the declaration of itself (which
	// is `type_spec name: (type_identifier)`, syntactically distinct).
	// So adding these does NOT introduce self-references; existing
	// self-edge filtering in find_callers handlers is unaffected.
	//
	// Patterns NOT included (would self-reference or over-capture):
	//   - bare `(type_identifier) @call` — matches the declaration too
	//   - `(type_spec)` patterns — definition site, not a reference
	//   - `(generic_type)` instantiations — handled by their inner
	//     (type_identifier) child via the qualified_type pattern when
	//     present; bare generic-arg shapes are tracked under a
	//     follow-up bead if needed
	RegisterRefQuery("go", `
		(call_expression function: (identifier) @call)
		(call_expression function: (selector_expression field: (field_identifier) @call))
		(keyed_element
			(literal_element)
			(literal_element (identifier) @call))
		(parameter_declaration (type_identifier) @call)
		(parameter_declaration (pointer_type (type_identifier) @call))
		(parameter_declaration (qualified_type name: (type_identifier) @call))
		(parameter_declaration (pointer_type (qualified_type name: (type_identifier) @call)))
		(field_declaration (type_identifier) @call)
		(field_declaration (pointer_type (type_identifier) @call))
		(field_declaration (qualified_type name: (type_identifier) @call))
		(field_declaration (pointer_type (qualified_type name: (type_identifier) @call)))
		(composite_literal type: (type_identifier) @call)
		(composite_literal type: (qualified_type name: (type_identifier) @call))
		(type_assertion_expression (type_identifier) @call)
		(type_assertion_expression (qualified_type name: (type_identifier) @call))
		(var_spec (type_identifier) @call)
		(var_spec (pointer_type (type_identifier) @call))
		(var_spec (qualified_type name: (type_identifier) @call))
		(var_spec (pointer_type (qualified_type name: (type_identifier) @call)))
	`)

	// Register Go file-level ref query — runs once per FILE against
	// the source_file root, catching tokens in positions that the
	// per-scope ExtractCalls can't see. Two cases (mache-02r9):
	//
	//   var serveCmd = &cobra.Command{ RunE: runServe }
	//   ^^^ keyed_element value identifier at file root
	//
	//   var initCmd = &cobra.Command{
	//       RunE: func(cmd *cobra.Command, args []string) error {
	//           return cliExit(4, ...)   <-- call_expression inside
	//       },                                  func_literal value
	//   }
	//
	// The outer var_declaration is at the file root, NOT inside any
	// function_declaration. Per-scope ExtractCalls (which walks
	// function bodies) never sees either case.
	//
	// Captures land in node_refs under a SENTINEL caller_id
	// '_file_level:<path>' — see engine.go's worker phase. dead_code
	// reads token presence so it's correct; fan_out_skew /
	// GetCallers exclude the sentinel (PR #270) so per-construct
	// aggregations stay clean.
	//
	// Per-scope-already-captured tokens (a Go function body's normal
	// call_expressions) are duplicated into the sentinel row — they
	// also exist as legitimate per-scope rows. Storage waste is
	// bounded; correctness is unaffected.
	RegisterFileLevelRefQuery("go", `
		(keyed_element
			(literal_element)
			(literal_element (identifier) @call))
		(call_expression function: (identifier) @call)
		(call_expression function: (selector_expression field: (field_identifier) @call))
		(call_expression
			arguments: (argument_list (identifier) @call))
	`)

	// ASTWalker context kinds — top-level node kinds whose source bytes
	// constitute the context blob. Mirrors the Go context query above.
	RegisterASTContextKinds("go", []string{
		"import_declaration", "const_declaration", "var_declaration", "type_declaration",
	})

	// ASTWalker (pure-Go path): batched JOIN-style fast path. One CallPattern
	// per shape; ExtractCalls/ExtractQualifiedCalls translate each into a
	// single SQL query that returns all matches in one pass.
	// Mirrors the Go RefQuery above (which SitterWalker.ExtractCalls uses via
	// getCallQuery) so the callers/ refs index matches across backends. Field
	// labels (name:/type:) don't matter here — queryCallPattern matches the kind
	// chain by parent_id. Parity asserted by TestASTQueryParity callers check.
	RegisterASTCallPatterns("go", []CallPattern{
		// function calls (bare + qualified)
		{OuterKind: "call_expression", LeafKind: "identifier"},
		{OuterKind: "call_expression", Ancestors: []string{"selector_expression"}, LeafKind: "field_identifier", QualifierKind: "identifier"},
		// composite literal element + type
		{OuterKind: "composite_literal", Ancestors: []string{"literal_element"}, LeafKind: "identifier"},
		{OuterKind: "composite_literal", LeafKind: "type_identifier"},
		{OuterKind: "composite_literal", Ancestors: []string{"qualified_type"}, LeafKind: "type_identifier"},
		// parameter types (incl. receiver) — bare / pointer / qualified
		{OuterKind: "parameter_declaration", LeafKind: "type_identifier"},
		{OuterKind: "parameter_declaration", Ancestors: []string{"pointer_type"}, LeafKind: "type_identifier"},
		{OuterKind: "parameter_declaration", Ancestors: []string{"qualified_type"}, LeafKind: "type_identifier"},
		{OuterKind: "parameter_declaration", Ancestors: []string{"pointer_type", "qualified_type"}, LeafKind: "type_identifier"},
		// struct field types
		{OuterKind: "field_declaration", LeafKind: "type_identifier"},
		{OuterKind: "field_declaration", Ancestors: []string{"pointer_type"}, LeafKind: "type_identifier"},
		{OuterKind: "field_declaration", Ancestors: []string{"qualified_type"}, LeafKind: "type_identifier"},
		{OuterKind: "field_declaration", Ancestors: []string{"pointer_type", "qualified_type"}, LeafKind: "type_identifier"},
		// type assertions
		{OuterKind: "type_assertion_expression", LeafKind: "type_identifier"},
		{OuterKind: "type_assertion_expression", Ancestors: []string{"qualified_type"}, LeafKind: "type_identifier"},
		// var declaration types
		{OuterKind: "var_spec", LeafKind: "type_identifier"},
		{OuterKind: "var_spec", Ancestors: []string{"pointer_type"}, LeafKind: "type_identifier"},
		{OuterKind: "var_spec", Ancestors: []string{"qualified_type"}, LeafKind: "type_identifier"},
		{OuterKind: "var_spec", Ancestors: []string{"pointer_type", "qualified_type"}, LeafKind: "type_identifier"},
	})
	RegisterASTCallPatterns("python", []CallPattern{
		{OuterKind: "call", LeafKind: "identifier"},
		{OuterKind: "call", Ancestors: []string{"attribute"}, LeafKind: "identifier"},
	})
	RegisterASTCallPatterns("rust", []CallPattern{
		{OuterKind: "call_expression", LeafKind: "identifier"},
		{OuterKind: "call_expression", Ancestors: []string{"scoped_identifier"}, LeafKind: "identifier"},
		{OuterKind: "call_expression", Ancestors: []string{"field_expression"}, LeafKind: "field_identifier"},
	})
	RegisterASTCallPatterns("elixir", []CallPattern{
		{OuterKind: "call", LeafKind: "identifier"},
		{OuterKind: "call", Ancestors: []string{"dot"}, LeafKind: "identifier"},
	})

	// Register HCL/Terraform queries — narrow to semantic references:
	// module sources, variable defaults, and provider/resource references.
	RegisterRefQuery("terraform", `
		(block (identifier) @_type (body (attribute (identifier) @_key (expression (literal_value (string_lit) @ref)))
			(#eq? @_key "source")))
		(block (identifier) @_type (body (attribute (identifier) @_key (expression (literal_value (string_lit) @ref)))
			(#eq? @_key "default")))
	`)

	// Register YAML queries — only aliases (references), not anchors (definitions).
	RegisterRefQuery("yaml", `
		(alias (alias_name) @ref)
	`)

	// Register Python queries (Call extraction).
	// Python uses 'call' node, not 'call_expression'.
	RegisterRefQuery("python", `
		(call function: (identifier) @call)
		(call function: (attribute attribute: (identifier) @call))
	`)

	// Register Rust queries — function calls and method calls.
	RegisterRefQuery("rust", `
		(call_expression function: (identifier) @call)
		(call_expression function: (scoped_identifier name: (identifier) @call))
		(call_expression function: (field_expression field: (field_identifier) @call))
	`)

	// Register Elixir queries — local and qualified function calls.
	// Pattern 0: local calls like func_name(args)
	// Pattern 1: qualified calls like Module.func_name(args)
	RegisterRefQuery("elixir", `
		(call target: (identifier) @call)
		(call target: (dot right: (identifier) @call))
	`)

	// --- Address-aware ref queries ---
	// These emit typed ref tokens (scheme:value) that bridge across languages.
	// The @ref capture is unquoted and prefixed with the scheme automatically.

	// Go: os.Getenv("VAR_NAME") → env:VAR_NAME
	RegisterAddressRefQuery("go", "env", `
		(call_expression
			function: (selector_expression
				operand: (identifier) @_pkg
				field: (field_identifier) @_func)
			arguments: (argument_list
				(interpreted_string_literal) @ref)
			(#eq? @_pkg "os")
			(#eq? @_func "Getenv"))
	`)

	// HCL: variable "VAR_NAME" { ... } → env:VAR_NAME
	RegisterAddressRefQuery("terraform", "env", `
		(block
			(identifier) @_type
			(string_lit) @ref
			(#eq? @_type "variable"))
	`)

	// HCL: module "X" { source = "..." } → mod:<source-value>
	// First milestone of mache-q43l (cross-language ref graph). The emitted
	// token is consumed by the resolver layer: local-path locators project
	// the target directory; remote locators are recorded as edges only.
	RegisterAddressRefQuery("terraform", "mod", `
		(block
			(identifier) @_type
			(body (attribute (identifier) @_key (expression (literal_value (string_lit) @ref))))
			(#eq? @_type "module")
			(#eq? @_key "source"))
	`)
}
