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

	// Register HCL/Terraform queries — narrow to semantic references:
	// module sources, variable defaults, and provider/resource references.
	RegisterRefQuery("hcl", `
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
}
