package mcpserve

import (
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

// symbolArg reads the name a reference tool is asked about. find_callers
// documents it as `token`; find_definition, get_impact and get_dataflow as
// `symbol`. They mean the same thing, and an agent that learned one spelling
// from one tool sent it to the next and was told the argument was missing
// (#820). Each tool keeps its documented name and accepts the other.
func symbolArg(request mcp.CallToolRequest, name, alias string) string {
	if v := request.GetString(name, ""); v != "" {
		return v
	}
	return request.GetString(alias, "")
}

// symbolRequired is the error for a call that gave neither spelling.
func symbolRequired(name, alias string) string {
	return fmt.Sprintf("%s is required (%s is accepted as an alias)", name, alias)
}
