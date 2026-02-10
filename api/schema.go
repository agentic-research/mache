package api

// Topology represents the root configuration of the semantic overlay.
// It maps the input data source to a directory structure.
type Topology struct {
	// Version of the Mache schema.
	Version string `json:"version"`
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
	// Children directories.
	Children []Node `json:"children,omitempty"`
	// Files within this directory.
	Files []Leaf `json:"files,omitempty"`
}

// Leaf represents a file in the filesystem.
type Leaf struct {
	// Name of the file. Can be a template string.
	Name string `json:"name"`
	// ContentTemplate is the template string used to generate the file content.
	ContentTemplate string `json:"content_template"`
	// Attributes defines file permissions/metadata (optional).
	Attributes *Attributes `json:"attributes,omitempty"`
}

// Attributes defines optional metadata for nodes/leaves.
type Attributes struct {
	Mode uint32 `json:"mode,omitempty"` // File mode (e.g., 0644)
}
