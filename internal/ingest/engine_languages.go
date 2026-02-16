package ingest

func init() {
	// Register Go context query.
	// Note: context extraction is Go-only for now; other languages return nil.
	RegisterContextQuery("go", `
		(import_declaration) @ctx
		(const_declaration) @ctx
		(var_declaration) @ctx
		(type_declaration) @ctx
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
}
