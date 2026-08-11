package ingest

func init() {
	// ASTWalker context kinds — top-level node kinds whose source bytes
	// constitute the context blob. Mirrors the Go context query above.
	RegisterASTContextKinds("go", []string{
		"import_declaration", "const_declaration", "var_declaration", "type_declaration",
	})

	// ASTWalker file-level ref patterns — the pure-Go mirror of the Go
	// RegisterFileLevelRefQuery above. Each tree-sitter capture becomes a
	// CallPattern kind-chain (queryCallPattern matches by parent_id):
	//   (keyed_element (literal_element) (literal_element (identifier) @call))
	//   (call_expression function: (identifier) @call)
	//   (call_expression function: (selector_expression field: (field_identifier) @call))
	//   (call_expression arguments: (argument_list (identifier) @call))
	// Parity with SitterWalker.ExtractFileLevelRefs is asserted by
	// TestASTFileLevelRefsParity_Go.
	RegisterASTFileLevelRefPatterns("go", []CallPattern{
		{OuterKind: "keyed_element", Ancestors: []string{"literal_element"}, LeafKind: "identifier", RequirePriorSibling: true},
		{OuterKind: "call_expression", LeafKind: "identifier"},
		{OuterKind: "call_expression", Ancestors: []string{"selector_expression"}, LeafKind: "field_identifier"},
		{OuterKind: "call_expression", Ancestors: []string{"argument_list"}, LeafKind: "identifier"},
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

	// Go imports become typed module references. The resolver layer decides
	// whether the path is stdlib, part of the main module, or an external
	// module; keeping the import path in node_refs preserves the cross-graph
	// join even when resolution is deferred.
	RegisterAddressRefQuery("go", "gomod", `
		(import_spec
			path: (interpreted_string_literal) @ref)
	`)
	RegisterAddressRefQuery("go", "gomod", `
		(import_spec
			path: (raw_string_literal) @ref)
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
