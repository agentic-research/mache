package ingest

// Walker abstracts over JSONPath (Data) and Tree-sitter (Code).
// It provides a unified way to query a tree-like structure and extract values for path templating.
type Walker interface {
	// Query executes a selector (query) against the given root node and returns a list of matches.
	// The root node can be a *sitter.Node (for code) or a generic Go object (for data).
	Query(root any, selector string) ([]Match, error)
}

// Match represents a single result from a query.
// It provides a map of values that can be used to render path templates.
type Match interface {
	// Values returns the captured values.
	// For Tree-sitter, these are the named captures from the query (e.g., "res.type" -> "aws_s3_bucket").
	// For JSONPath, if the match is an object, its fields are returned as values.
	// If the match is a primitive, it might be returned under a default key (e.g., "value").
	Values() map[string]string

	// Context returns the underlying object/node to be used as the root for child queries.
	// For JSONPath, this is the matched object.
	// For Tree-sitter, this is the node captured as @scope (or similar convention).
	Context() any
}
