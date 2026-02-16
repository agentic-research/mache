package ingest

func init() {
	// Register Go context query
	RegisterContextQuery("go", `
		; (package_clause) @ctx
		(import_declaration) @ctx
		(const_declaration) @ctx
		(var_declaration) @ctx
		(type_declaration) @ctx
	`)

	// Register HCL queries (Terraform modules, variables)
	RegisterRefQuery("hcl", `(string_lit) @s`)

	// Register YAML queries (Anchors, Aliases)
	RegisterRefQuery("yaml", `
		(anchor (anchor_name) @def)
		(alias (alias_name) @ref)
	`)

	// Register Python queries (Call extraction)
	// Python uses 'call' node, not 'call_expression'
	RegisterRefQuery("python", `
		(call function: (identifier) @call)
		(call function: (attribute attribute: (identifier) @call))
	`)
}
