package graph

import (
	"errors"
	"fmt"
	"strings"

	"github.com/agentic-research/mache/api"
)

// ErrDirectSchemaUnsupported says a schema uses something the direct path
// (one JSON record per row, projected on demand) cannot honour.
var ErrDirectSchemaUnsupported = errors.New("schema not supported by the direct path")

// directRecordSelectors are the selectors the direct path honours: each one
// means "this record". The scan treats every row as one record and every
// node below a root as a view of it; it evaluates no JSONPath.
var directRecordSelectors = map[string]bool{"": true, "$": true, "$[*]": true}

// checkDirectSchema refuses a schema the direct path would serve wrongly.
// It used to serve one anyway: a nested selector such as `$.tools[*]` was
// ignored, so a record's tools were never projected, and a templated root
// was never listed (mache-b33274). The answer the engine gives is the
// specified one; this path either matches it or says why it cannot, naming
// the engine path, which handles all of these.
func checkDirectSchema(schema *api.Topology) error {
	var problems []string
	for _, root := range schema.Nodes {
		if strings.Contains(root.Name, "{{") {
			problems = append(problems, fmt.Sprintf("root %q has a templated name, which the direct path cannot list", root.Name))
		}
		problems = append(problems, nestedSelectors(root.Children, root.Name)...)
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s. Build the projection with the engine instead: `mache --schema <schema> --data <data> --out out.db`, then serve out.db",
		ErrDirectSchemaUnsupported, strings.Join(problems, "; "))
}

// nestedSelectors lists the nodes under path whose selector picks something
// other than the record itself.
func nestedSelectors(nodes []api.Node, path string) []string {
	var problems []string
	for _, n := range nodes {
		at := path + "/" + n.Name
		if !directRecordSelectors[n.Selector] {
			problems = append(problems, fmt.Sprintf("node %q selects %q inside a record, which the direct path does not evaluate", at, n.Selector))
		}
		problems = append(problems, nestedSelectors(n.Children, at)...)
	}
	return problems
}
