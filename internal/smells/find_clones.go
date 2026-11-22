package smells

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/agentic-research/mache/graph"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// cloneRuleID is the rule find_clones groups. The tool has no detection of
// its own: it is that rule's findings, arranged by the group the rule's own
// query assigned them, so the gate and the tool cannot disagree about what a
// clone is.
const cloneRuleID = "duplicate_code"

// defaultCloneSetLimit is how many sets find_clones returns when not told.
const defaultCloneSetLimit = 50

// cloneMember is one copy in a clone set.
type cloneMember struct {
	SourceID string `json:"source_id"`
	Line     int    `json:"line"`
	NodeID   string `json:"node_id,omitempty"`
	Snippet  string `json:"snippet,omitempty"`
	// Test marks a copy in a test file. A set is reported by default only
	// when at least two of its copies are not.
	Test bool `json:"test,omitempty"`
	// span is the copy's byte range, which the snippet is read from.
	startByte, endByte int
}

// cloneSet is functions that are structural copies of each other.
type cloneSet struct {
	// ID is the group the duplicate_code rule assigned, stable while the
	// shared structure is.
	ID string `json:"id"`
	// Nodes is the size of the duplicated structure in syntax nodes, the
	// same for every copy.
	Nodes  int64 `json:"nodes"`
	Copies int   `json:"copies"`
	// RemovableNodes is what one shared helper would delete: every copy but
	// one. Sets are ranked by it.
	RemovableNodes int64         `json:"removable_nodes"`
	Members        []cloneMember `json:"members"`
}

// clonesResponse is find_clones' answer.
type clonesResponse struct {
	// TotalSets counts every set that matched, before limit.
	TotalSets int `json:"total_sets"`
	// Truncated says Sets holds fewer than TotalSets.
	Truncated bool       `json:"truncated,omitempty"`
	Sets      []cloneSet `json:"sets"`
	// NotDetected names what this tool cannot see, so an empty answer is
	// not read as "no duplication".
	NotDetected []string `json:"not_detected"`
}

// cloneBlindSpots is clonesResponse.NotDetected. Each is pinned by a test in
// find_clones_test.go that fails the day the gap closes.
var cloneBlindSpots = []string{
	"near-miss clones: copies that differ by an added, removed or reordered statement",
	"block-level clones: a repeated block inside functions that otherwise differ",
	"languages other than Go",
}

// cloneQuery is what a find_clones call asks for.
type cloneQuery struct {
	minNodes     int64
	includeTests bool
	sourceID     string
	limit        int
}

// MakeFindClonesHandler returns the find_clones MCP handler: the clone sets
// of the served graph, largest saving first.
func MakeFindClonesHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		rule := RegisteredRule(cloneRuleID)
		if rule == nil || !rule.Grouped { // coverage:ignore — the embedded rule is always present and grouped; guarded so a rule edit fails loudly
			return mcp.NewToolResultError(fmt.Sprintf("rule %q is not registered as a grouped rule", cloneRuleID)), nil // coverage:ignore
		} // coverage:ignore
		q := cloneQuery{
			minNodes:     rule.DefaultMinMetric,
			includeTests: request.GetBool("include_tests", false),
			sourceID:     strings.TrimSpace(request.GetString("source_id", "")),
			limit:        request.GetInt("limit", defaultCloneSetLimit),
		}
		// An explicit min_nodes wins, including 0, whatever number type the
		// client sent it as.
		if _, ok := request.GetArguments()["min_nodes"]; ok {
			q.minNodes = int64(request.GetFloat("min_nodes", 0))
		}
		if q.limit <= 0 {
			q.limit = defaultCloneSetLimit
		}

		qg, ok := g.(graph.RefsQuerier)
		if !ok { // coverage:ignore — every served graph exposes SQL; see MakeFindSmellsHandler
			return mcp.NewToolResultError("the active graph backend doesn't expose a SQL handle; find_clones requires a leyline-parsed .db"), nil // coverage:ignore
		} // coverage:ignore
		scan, err := openSmellScan(ctx, qg)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("find_clones: %v", err)), nil
		}
		defer func() { _ = scan.Close() }()
		if missing, err := missingTables(scan, rule.Requires); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("find_clones: pre-flight table check failed: %v", err)), nil
		} else if len(missing) > 0 {
			return mcp.NewToolResultError(fmt.Sprintf(
				"find_clones requires SQL tables [%s] which aren't present on the active backend; they are produced by ley-line-open's `leyline parse`",
				strings.Join(missing, ", "))), nil
		}

		// Unscoped and uncapped: a set is only whole if every copy is read,
		// so source_id and limit apply to sets, after grouping.
		findings, err := runSmellRuleQuery(scan, rule, "", math.MaxInt32)
		if err != nil { // coverage:ignore — the pre-flight above catches schema gaps; this needs DB IO failure
			return mcp.NewToolResultError(fmt.Sprintf("find_clones: %v", err)), nil // coverage:ignore
		} // coverage:ignore
		sets := cloneSets(aboveMinMetric(findings, q.minNodes), q)
		resp := clonesResponse{TotalSets: len(sets), NotDetected: cloneBlindSpots}
		if len(sets) > q.limit {
			sets, resp.Truncated = sets[:q.limit], true
		}
		resp.Sets = withSnippets(scan, sets)
		return mcp.NewToolResultText(jsonOrPanic(resp)), nil
	}
}

// cloneSets groups findings by the rule's group into the sets q asks for,
// ranked by removable nodes, then by first member for a stable order.
func cloneSets(findings []smellFinding, q cloneQuery) []cloneSet {
	byGroup := map[string][]smellFinding{}
	for _, f := range findings {
		byGroup[f.Group] = append(byGroup[f.Group], f)
	}
	sets := make([]cloneSet, 0, len(byGroup))
	for id, members := range byGroup {
		if set, ok := newCloneSet(id, members, q); ok {
			sets = append(sets, set)
		}
	}
	slices.SortFunc(sets, func(a, b cloneSet) int {
		return cmp.Or(
			cmp.Compare(b.RemovableNodes, a.RemovableNodes),
			cmp.Compare(a.Members[0].SourceID, b.Members[0].SourceID),
			cmp.Compare(a.Members[0].Line, b.Members[0].Line),
		)
	})
	return sets
}

// newCloneSet is the set of the given copies, or false when q excludes it:
// fewer than two copies, fewer than two outside test files unless tests are
// included, or no copy in q's source file.
func newCloneSet(id string, copies []smellFinding, q cloneQuery) (cloneSet, bool) {
	slices.SortFunc(copies, func(a, b smellFinding) int {
		return cmp.Or(cmp.Compare(a.SourceID, b.SourceID), cmp.Compare(a.Line, b.Line))
	})
	set := cloneSet{ID: id, Copies: len(copies), Members: make([]cloneMember, 0, len(copies))}
	production, inSource := 0, q.sourceID == ""
	for _, f := range copies {
		test := strings.HasSuffix(f.SourceID, "_test.go")
		if !test {
			production++
		}
		inSource = inSource || f.SourceID == q.sourceID
		set.Nodes = f.Metric
		set.Members = append(set.Members, cloneMember{
			SourceID: f.SourceID, Line: f.Line, NodeID: f.NodeID, Test: test,
			startByte: f.StartByte, endByte: f.EndByte,
		})
	}
	if len(copies) < 2 || !inSource || (!q.includeTests && production < 2) {
		return cloneSet{}, false
	}
	set.RemovableNodes = set.Nodes * int64(len(copies)-1)
	return set, true
}

// withSnippets fills each member's source preview, read only for the sets
// being returned.
func withSnippets(qg graph.RefsQuerier, sets []cloneSet) []cloneSet {
	var findings []smellFinding
	for _, s := range sets {
		for _, m := range s.Members {
			findings = append(findings, smellFinding{SourceID: m.SourceID, StartByte: m.startByte, EndByte: m.endByte})
		}
	}
	populateSnippets(qg, findings)
	i := 0
	for si := range sets {
		for mi := range sets[si].Members {
			sets[si].Members[mi].Snippet = fromDeclaration(findings[i].Snippet)
			i++
		}
	}
	return sets
}

// fromDeclaration drops the context populateSnippets pads in front of a
// finding, so a copy's preview opens on its own `func` rather than on the
// tail of whatever precedes it.
func fromDeclaration(snippet string) string {
	if i := strings.Index(snippet, "func "); i > 0 {
		return snippet[i:]
	}
	return snippet
}
