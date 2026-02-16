package ingest

func init() {
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
