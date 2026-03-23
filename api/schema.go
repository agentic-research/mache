package api

// SchemaVersion is the current schema version string.
const SchemaVersion = "v1"

// DiagramDef is a named diagram view rendered by the {{diagram}} template function.
// Grouping, labels, and visibility emerge from community detection on the refs
// graph; the definition intentionally contains only the layout direction.
type DiagramDef struct {
	// Layout is the mermaid direction: "TD", "LR", "BT", or "RL".
	Layout string `json:"layout"`
}

// Topology represents the root configuration of the semantic overlay.
// It maps the input data source to a directory structure.
type Topology struct {
	// Version of the Mache schema.
	Version string `json:"version"`
	// Table is the SQLite table name to query (default: "results").
	Table string `json:"table,omitempty"`
	// Diagrams defines named diagram views that can be rendered via the
	// {{diagram "name"}} template function. Each entry maps a diagram name
	// to its definition. If absent, {{diagram "system"}} still works using
	// the default "TD" layout.
	Diagrams map[string]DiagramDef `json:"diagrams,omitempty"`
	// FileSets defines reusable groups of file definitions that can be
	// included by nodes via the Include field. Avoids duplicating file
	// entries across many construct types.
	FileSets map[string][]Leaf `json:"file_sets,omitempty"`
	// Root nodes of the filesystem.
	Nodes []Node `json:"nodes,omitempty"`
}

// Node represents a directory in the filesystem.
// It can contain other nodes or leaves (files).
type Node struct {
	// Name of the directory. Can be a template string.
	Name string `json:"name"`
	// Selector is a query (e.g., JSONPath) to select data for this node context.
	Selector string `json:"selector,omitempty"`
	// Refs are template strings that generate cross-reference tokens.
	// Each rendered token is added to the ref index, enabling callers/ virtual
	// directories for JSON-projected data (e.g., tool names across MCP servers).
	Refs []string `json:"refs,omitempty"`
	// SkipSelfMatch prevents the selector from matching the current context node itself.
	// Useful for recursive schemas to avoid infinite loops.
	SkipSelfMatch bool `json:"skip_self_match,omitempty"`
	// Language hint for multi-language schemas (e.g., "go", "terraform", "python").
	// Used to filter nodes during ingestion to prevent cross-language query errors.
	Language string `json:"language,omitempty"`
	// Include references named file sets from Topology.FileSets.
	// The referenced leaves are appended to this node's Files during
	// schema resolution.
	Include []string `json:"include,omitempty"`
	// Children directories.
	Children []Node `json:"children,omitempty"`
	// Files within this directory.
	Files []Leaf `json:"files,omitempty"`
}

// ResolveIncludes expands all Include references in the schema tree,
// appending the referenced FileSets leaves to each node's Files.
// Call this once after parsing the schema, before ingestion or materialization.
func (t *Topology) ResolveIncludes() {
	if len(t.FileSets) == 0 {
		return
	}
	resolveNodes(t.Nodes, t.FileSets)
}

func resolveNodes(nodes []Node, sets map[string][]Leaf) {
	for i := range nodes {
		for _, ref := range nodes[i].Include {
			if leaves, ok := sets[ref]; ok {
				nodes[i].Files = append(nodes[i].Files, leaves...)
			}
		}
		resolveNodes(nodes[i].Children, sets)
	}
}

// Leaf represents a file in the filesystem.
type Leaf struct {
	// Name of the file. Can be a template string.
	Name string `json:"name"`
	// ContentTemplate is the template string used to generate the file content.
	// Mutually exclusive with ContentSource.
	ContentTemplate string `json:"content_template"`
	// ContentSource names an auxiliary table that provides file content.
	// The materializer joins the aux table with construct directories by
	// symbol name to create files. Supported sources: "lsp_hover",
	// "lsp_diagnostics", "lsp_defs", "lsp_refs".
	// Mutually exclusive with ContentTemplate.
	ContentSource string `json:"content_source,omitempty"`
	// Attributes defines file permissions/metadata (optional).
	Attributes *Attributes `json:"attributes,omitempty"`
}

// Attributes defines optional metadata for nodes/leaves.
type Attributes struct {
	Mode uint32 `json:"mode,omitempty"` // File mode (e.g., 0644)
}
