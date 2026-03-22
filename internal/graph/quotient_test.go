package graph

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComputeQuotient_Basic(t *testing.T) {
	cr := &CommunityResult{
		Communities: []Community{
			{ID: 0, Members: []string{"a", "b"}},
			{ID: 1, Members: []string{"c", "d"}},
		},
		Membership: map[string]int{
			"a": 0, "b": 0,
			"c": 1, "d": 1,
		},
	}

	refs := map[string][]string{
		"shared_token": {"a", "c"},      // cross-community
		"internal_0":   {"a", "b"},      // internal to class 0
		"internal_1":   {"c", "d"},      // internal to class 1
		"bridge":       {"b", "c", "d"}, // cross-community
	}

	q := ComputeQuotient(cr, refs)

	require.Len(t, q.Classes, 2)
	require.Len(t, q.Edges, 1, "should have one edge between the two classes")

	// Verify edge connects class 0 and class 1.
	assert.Equal(t, 0, q.Edges[0].From)
	assert.Equal(t, 1, q.Edges[0].To)
	assert.Greater(t, q.Edges[0].Weight, 0.0)

	// Both cross-community tokens should appear.
	assert.Contains(t, q.Edges[0].Tokens, "shared_token")
	assert.Contains(t, q.Edges[0].Tokens, "bridge")

	// Internal weight should reflect internal refs.
	assert.Greater(t, q.Classes[0].InternalW, 0.0, "class 0 has internal refs")
	assert.Greater(t, q.Classes[1].InternalW, 0.0, "class 1 has internal refs")

	// ClassOf mapping.
	assert.Equal(t, 0, q.ClassOf["a"])
	assert.Equal(t, 0, q.ClassOf["b"])
	assert.Equal(t, 1, q.ClassOf["c"])
	assert.Equal(t, 1, q.ClassOf["d"])
}

func TestComputeQuotient_NilCommunityResult(t *testing.T) {
	q := ComputeQuotient(nil, nil)
	assert.NotNil(t, q.ClassOf, "ClassOf should be initialized even for nil input")
	assert.Empty(t, q.Classes)
	assert.Empty(t, q.Edges)
}

func TestComputeQuotient_EmptyCommunities(t *testing.T) {
	cr := &CommunityResult{
		Communities: []Community{},
		Membership:  map[string]int{},
	}
	q := ComputeQuotient(cr, map[string][]string{})
	assert.Empty(t, q.Classes)
	assert.Empty(t, q.Edges)
}

func TestComputeQuotient_SingleCommunity(t *testing.T) {
	cr := &CommunityResult{
		Communities: []Community{
			{ID: 0, Members: []string{"a", "b", "c"}},
		},
		Membership: map[string]int{
			"a": 0, "b": 0, "c": 0,
		},
	}
	refs := map[string][]string{
		"tok1": {"a", "b"},
		"tok2": {"b", "c"},
	}

	q := ComputeQuotient(cr, refs)
	assert.Len(t, q.Classes, 1)
	assert.Empty(t, q.Edges, "single community should have no cross-class edges")
	assert.Greater(t, q.Classes[0].InternalW, 0.0)
}

func TestComputeQuotient_NoCrossEdges(t *testing.T) {
	cr := &CommunityResult{
		Communities: []Community{
			{ID: 0, Members: []string{"a", "b"}},
			{ID: 1, Members: []string{"c", "d"}},
		},
		Membership: map[string]int{
			"a": 0, "b": 0, "c": 1, "d": 1,
		},
	}
	refs := map[string][]string{
		"tok_a": {"a", "b"},
		"tok_c": {"c", "d"},
	}

	q := ComputeQuotient(cr, refs)
	assert.Len(t, q.Classes, 2)
	assert.Empty(t, q.Edges, "no shared tokens means no cross-class edges")
}

func TestComputeQuotient_ThreeCommunities(t *testing.T) {
	cr := &CommunityResult{
		Communities: []Community{
			{ID: 0, Members: []string{"a"}},
			{ID: 1, Members: []string{"b"}},
			{ID: 2, Members: []string{"c"}},
		},
		Membership: map[string]int{
			"a": 0, "b": 1, "c": 2,
		},
	}
	refs := map[string][]string{
		"all": {"a", "b", "c"}, // connects all three
		"ab":  {"a", "b"},      // connects 0-1
	}

	q := ComputeQuotient(cr, refs)
	assert.Len(t, q.Classes, 3)
	// "all" creates edges 0-1, 0-2, 1-2. "ab" adds to 0-1.
	assert.Len(t, q.Edges, 3, "triangle of edges")
}

func TestComputeQuotient_LabelDerivation(t *testing.T) {
	cr := &CommunityResult{
		Communities: []Community{
			{ID: 0, Members: []string{"a", "b", "c"}},
		},
		Membership: map[string]int{
			"a": 0, "b": 0, "c": 0,
		},
	}
	refs := map[string][]string{
		"rare":   {"a"},
		"common": {"a", "b", "c"}, // most referenced
		"mid":    {"a", "b"},
	}

	q := ComputeQuotient(cr, refs)
	assert.Equal(t, "common", q.Classes[0].Label, "label should be most-referenced token")
}

func TestComputeQuotient_LabelTiebreaker(t *testing.T) {
	cr := &CommunityResult{
		Communities: []Community{
			{ID: 0, Members: []string{"a", "b"}},
		},
		Membership: map[string]int{
			"a": 0, "b": 0,
		},
	}
	refs := map[string][]string{
		"beta":  {"a", "b"}, // same count
		"alpha": {"a", "b"}, // same count, lexicographically first
	}

	q := ComputeQuotient(cr, refs)
	assert.Equal(t, "alpha", q.Classes[0].Label, "ties broken lexicographically")
}

func TestComputeQuotient_NilRefs(t *testing.T) {
	cr := &CommunityResult{
		Communities: []Community{
			{ID: 0, Members: []string{"a"}},
		},
		Membership: map[string]int{"a": 0},
	}
	q := ComputeQuotient(cr, nil)
	assert.Len(t, q.Classes, 1)
	assert.Equal(t, "cluster_0", q.Classes[0].Label, "nil refs gets fallback label")
}

func TestComputeQuotient_WeightFormula(t *testing.T) {
	// Verify weight = product of member counts per community.
	cr := &CommunityResult{
		Communities: []Community{
			{ID: 0, Members: []string{"a1", "a2", "a3"}},
			{ID: 1, Members: []string{"b1", "b2"}},
		},
		Membership: map[string]int{
			"a1": 0, "a2": 0, "a3": 0,
			"b1": 1, "b2": 1,
		},
	}
	refs := map[string][]string{
		"shared": {"a1", "a2", "b1", "b2"}, // 2 from class 0, 2 from class 1
	}

	q := ComputeQuotient(cr, refs)
	require.Len(t, q.Edges, 1)
	// Weight = count_in_0 * count_in_1 = 2 * 2 = 4
	assert.Equal(t, 4.0, q.Edges[0].Weight)
}

func TestMermaid_BasicOutput(t *testing.T) {
	q := &QuotientGraph{
		Classes: []Class{
			{ID: 0, Label: "graph", Members: []string{"a", "b"}},
			{ID: 1, Label: "ingest", Members: []string{"c", "d"}},
		},
		Edges: []QuotientEdge{
			{From: 0, To: 1, Weight: 5, Tokens: []string{"Engine"}},
		},
	}

	out := q.Mermaid("TD")
	assert.Contains(t, out, "graph TD")
	assert.Contains(t, out, `subgraph C0["graph"]`)
	assert.Contains(t, out, `subgraph C1["ingest"]`)
	assert.Contains(t, out, "C0 -->|Engine| C1")
	assert.Contains(t, out, "end")
}

func TestMermaid_SingleMemberNode(t *testing.T) {
	q := &QuotientGraph{
		Classes: []Class{
			{ID: 0, Label: "singleton", Members: []string{"only"}},
		},
	}

	out := q.Mermaid("LR")
	assert.Contains(t, out, "graph LR")
	assert.Contains(t, out, `C0["singleton"]`)
	assert.NotContains(t, out, "subgraph")
}

func TestMermaid_DefaultLayout(t *testing.T) {
	q := &QuotientGraph{
		Classes: []Class{
			{ID: 0, Label: "x", Members: []string{"a"}},
		},
	}
	out := q.Mermaid("")
	assert.True(t, strings.HasPrefix(out, "graph TD"))
}

func TestMermaid_EmptyGraph(t *testing.T) {
	q := &QuotientGraph{}
	out := q.Mermaid("TD")
	assert.Equal(t, "graph TD\n", out)
}

func TestMermaid_EdgeLabelCollapse(t *testing.T) {
	q := &QuotientGraph{
		Classes: []Class{
			{ID: 0, Label: "a", Members: []string{"a1", "a2"}},
			{ID: 1, Label: "b", Members: []string{"b1", "b2"}},
		},
		Edges: []QuotientEdge{
			{
				From: 0, To: 1, Weight: 10,
				Tokens: []string{"t1", "t2", "t3", "t4", "t5"},
				TokenWeights: map[string]float64{
					"t1": 4, // above mean (10/5 = 2)
					"t2": 3, // above mean
					"t3": 1, // below mean
					"t4": 1, // below mean
					"t5": 1, // below mean
				},
			},
		},
	}

	out := q.Mermaid("TD")
	// Should show only above-mean tokens (t1, t2) plus count of remaining.
	assert.Contains(t, out, "t1, t2 (+3 more)")
}

func TestMermaid_FewTokensShowAll(t *testing.T) {
	q := &QuotientGraph{
		Classes: []Class{
			{ID: 0, Label: "a", Members: []string{"a1", "a2"}},
			{ID: 1, Label: "b", Members: []string{"b1", "b2"}},
		},
		Edges: []QuotientEdge{
			{From: 0, To: 1, Weight: 5, Tokens: []string{"alpha", "beta"}},
		},
	}

	out := q.Mermaid("TD")
	assert.Contains(t, out, "alpha, beta")
}

func TestMermaid_SanitizesNodeIDs(t *testing.T) {
	q := &QuotientGraph{
		Classes: []Class{
			{ID: 0, Label: "graph core", Members: []string{"internal/graph/store.go", "internal/graph/node.go"}},
		},
	}

	out := q.Mermaid("TD")
	assert.Contains(t, out, "internal_graph_store_go")
	assert.Contains(t, out, "internal_graph_node_go")
	assert.NotContains(t, out, "internal/graph")
}

func TestEdgeLabel_NoTokens(t *testing.T) {
	e := QuotientEdge{Tokens: nil}
	assert.Equal(t, "", edgeLabel(e))
}

func TestEdgeLabel_OneToken(t *testing.T) {
	e := QuotientEdge{Tokens: []string{"Validate"}}
	assert.Equal(t, "Validate", edgeLabel(e))
}

func TestEdgeLabel_ThreeTokens(t *testing.T) {
	e := QuotientEdge{Tokens: []string{"a", "b", "c"}}
	assert.Equal(t, "a, b, c", edgeLabel(e))
}

func TestEdgeLabel_EqualWeightsShowAll(t *testing.T) {
	// When all tokens have equal weight, all are at the mean — show all.
	e := QuotientEdge{
		Tokens:       []string{"a", "b", "c", "d"},
		Weight:       8,
		TokenWeights: map[string]float64{"a": 2, "b": 2, "c": 2, "d": 2},
	}
	assert.Equal(t, "a, b, c, d", edgeLabel(e))
}

func TestEdgeLabel_WeightedFiltering(t *testing.T) {
	// Only tokens above the mean weight should appear.
	e := QuotientEdge{
		Tokens:       []string{"heavy", "light1", "light2", "medium"},
		Weight:       10,
		TokenWeights: map[string]float64{"heavy": 6, "medium": 3, "light1": 0.5, "light2": 0.5},
	}
	// Mean = 10/4 = 2.5. "heavy" (6) and "medium" (3) are >= 2.5.
	label := edgeLabel(e)
	assert.Equal(t, "heavy, medium (+2 more)", label)
}

func TestComputeQuotient_TokenWeightsPopulated(t *testing.T) {
	cr := &CommunityResult{
		Communities: []Community{
			{ID: 0, Members: []string{"a1", "a2"}},
			{ID: 1, Members: []string{"b1", "b2"}},
		},
		Membership: map[string]int{
			"a1": 0, "a2": 0, "b1": 1, "b2": 1,
		},
	}
	refs := map[string][]string{
		"heavy": {"a1", "a2", "b1", "b2"}, // 2*2 = 4
		"light": {"a1", "b1"},             // 1*1 = 1
	}

	q := ComputeQuotient(cr, refs)
	require.Len(t, q.Edges, 1)
	assert.Equal(t, 5.0, q.Edges[0].Weight) // 4 + 1
	assert.Equal(t, 4.0, q.Edges[0].TokenWeights["heavy"])
	assert.Equal(t, 1.0, q.Edges[0].TokenWeights["light"])
}

// ---------------------------------------------------------------------------
// Self-hosting validation: realistic refs mirroring mache's own codebase
// ---------------------------------------------------------------------------

// buildMacheRefsMap constructs a refs map that mirrors mache's actual package
// topology and cross-package call patterns. Node IDs follow the tree-sitter
// ingestion convention: "pkg/category/Name/source" (the leaf file that holds
// the source of each function/type/method). Tokens are the function and type
// names that appear in call_expression and selector_expression captures.
//
// The packages modeled here:
//   - graph:     MemoryStore, Node, GetNode, ListChildren, AddRef, AddNode, etc.
//   - ingest:    Engine, Ingest, SitterWalker, RenderTemplate, processNode
//   - fs:        MacheFS, Root, Opendir, Readdir, Read
//   - nfsmount:  GraphFS, NFS server, handle-based ops
//   - writeback: Splice, Format, Validate
//   - lattice:   FormalContext, NextClosure, Inferrer
//   - cmd:       serve, mount, handler registration
func buildMacheRefsMap() map[string][]string {
	return map[string][]string{
		// --- graph package internal refs (shared heavily within package) ---
		"Node": {
			"graph/types/Node/source",
			"graph/methods/MemoryStore.GetNode/source",
			"graph/methods/MemoryStore.AddNode/source",
			"graph/methods/MemoryStore.ListChildren/source",
			"graph/methods/MemoryStore.ReadContent/source",
			"graph/methods/SQLiteGraph.GetNode/source",
			"graph/methods/SQLiteGraph.ListChildren/source",
			"ingest/functions/processNode/source",
			"ingest/methods/SQLiteWriter.AddNode/source",
			"fs/methods/MacheFS.Read/source",
			"nfsmount/methods/GraphFS.ReadFile/source",
			"cmd/functions/makeGetOverviewHandler/source",
		},
		"MemoryStore": {
			"graph/types/MemoryStore/source",
			"graph/functions/NewMemoryStore/source",
			"graph/methods/MemoryStore.GetNode/source",
			"graph/methods/MemoryStore.AddNode/source",
			"graph/methods/MemoryStore.AddRef/source",
			"graph/methods/MemoryStore.AddDef/source",
			"graph/methods/MemoryStore.ListChildren/source",
			"graph/methods/MemoryStore.ReadContent/source",
			"graph/methods/MemoryStore.GetCallers/source",
			"graph/methods/MemoryStore.GetCallees/source",
			"graph/methods/MemoryStore.RefsMap/source",
			"ingest/functions/NewEngine/source",
			"cmd/functions/buildGraph/source",
		},
		"AddRef": {
			"graph/methods/MemoryStore.AddRef/source",
			"ingest/functions/processNode/source",
			"ingest/methods/Engine.collectNodes/source",
		},
		"AddNode": {
			"graph/methods/MemoryStore.AddNode/source",
			"ingest/functions/processNode/source",
			"ingest/methods/SQLiteWriter.AddNode/source",
		},
		"GetNode": {
			"graph/methods/MemoryStore.GetNode/source",
			"graph/methods/SQLiteGraph.GetNode/source",
			"fs/methods/MacheFS.Getattr/source",
			"fs/methods/MacheFS.Read/source",
			"nfsmount/methods/GraphFS.ReadFile/source",
			"nfsmount/methods/GraphFS.Stat/source",
			"cmd/functions/makeReadFileHandler/source",
		},
		"ListChildren": {
			"graph/methods/MemoryStore.ListChildren/source",
			"graph/methods/SQLiteGraph.ListChildren/source",
			"fs/methods/MacheFS.Readdir/source",
			"nfsmount/methods/GraphFS.ReadDir/source",
			"cmd/functions/makeListDirHandler/source",
		},
		"ReadContent": {
			"graph/methods/MemoryStore.ReadContent/source",
			"graph/methods/SQLiteGraph.ReadContent/source",
			"fs/methods/MacheFS.Read/source",
			"nfsmount/methods/GraphFS.ReadFile/source",
		},
		"GetCallers": {
			"graph/methods/MemoryStore.GetCallers/source",
			"fs/methods/MacheFS.Readdir/source",
			"nfsmount/methods/GraphFS.ReadDir/source",
		},
		"ContentRef": {
			"graph/types/ContentRef/source",
			"graph/methods/MemoryStore.ReadContent/source",
			"ingest/types/SQLiteResolver/source",
			"ingest/methods/SQLiteResolver.Resolve/source",
		},
		"ErrNotFound": {
			"graph/variables/ErrNotFound/source",
			"graph/methods/MemoryStore.GetNode/source",
			"graph/methods/SQLiteGraph.GetNode/source",
			"fs/methods/MacheFS.Getattr/source",
			"nfsmount/methods/GraphFS.Stat/source",
		},

		// --- graph: community detection / quotient types ---
		"DetectCommunities": {
			"graph/functions/DetectCommunities/source",
			"cmd/functions/makeGetDiagramHandler/source",
			"ingest/methods/Engine.DiagramFuncMap/source",
		},
		"ComputeQuotient": {
			"graph/functions/ComputeQuotient/source",
			"cmd/functions/makeGetDiagramHandler/source",
			"ingest/methods/Engine.DiagramFuncMap/source",
		},
		"RefsMap": {
			"graph/methods/MemoryStore.RefsMap/source",
			"graph/methods/SQLiteGraph.RefsMap/source",
			"cmd/functions/makeGetDiagramHandler/source",
			"ingest/methods/Engine.DiagramFuncMap/source",
		},

		// --- ingest package internal refs ---
		"Engine": {
			"ingest/types/Engine/source",
			"ingest/functions/NewEngine/source",
			"ingest/methods/Engine.Ingest/source",
			"ingest/methods/Engine.DiagramFuncMap/source",
			"ingest/methods/Engine.RenderContentTemplate/source",
			"cmd/functions/buildGraph/source",
			"cmd/functions/runServe/source",
		},
		"Ingest": {
			"ingest/methods/Engine.Ingest/source",
			"cmd/functions/buildGraph/source",
			"cmd/functions/runServe/source",
		},
		"SitterWalker": {
			"ingest/types/SitterWalker/source",
			"ingest/functions/NewSitterWalker/source",
			"ingest/functions/processNode/source",
		},
		"RenderTemplate": {
			"ingest/functions/RenderTemplate/source",
			"ingest/functions/processNode/source",
			"ingest/methods/Engine.collectNodes/source",
			"graph/methods/SQLiteGraph.renderContent/source",
		},
		"Walker": {
			"ingest/types/Walker/source",
			"ingest/functions/processNode/source",
			"ingest/types/SitterWalker/source",
			"ingest/types/JSONWalker/source",
		},
		"Match": {
			"ingest/types/Match/source",
			"ingest/functions/processNode/source",
			"ingest/types/SitterWalker/source",
		},
		"processNode": {
			"ingest/functions/processNode/source",
			"ingest/methods/Engine.Ingest/source",
		},
		"ExtractCalls": {
			"ingest/methods/SitterWalker.ExtractCalls/source",
			"ingest/functions/processNode/source",
		},

		// --- fs package (FUSE layer) ---
		"MacheFS": {
			"fs/types/MacheFS/source",
			"fs/methods/MacheFS.Getattr/source",
			"fs/methods/MacheFS.Read/source",
			"fs/methods/MacheFS.Readdir/source",
			"fs/methods/MacheFS.Opendir/source",
		},
		"Opendir": {
			"fs/methods/MacheFS.Opendir/source",
			"fs/methods/MacheFS.Readdir/source",
		},
		"Readdir": {
			"fs/methods/MacheFS.Readdir/source",
		},

		// --- nfsmount package ---
		"GraphFS": {
			"nfsmount/types/GraphFS/source",
			"nfsmount/methods/GraphFS.ReadFile/source",
			"nfsmount/methods/GraphFS.ReadDir/source",
			"nfsmount/methods/GraphFS.Stat/source",
			"cmd/functions/runMount/source",
		},

		// --- writeback package ---
		"Splice": {
			"writeback/functions/Splice/source",
			"writeback/functions/SpliceAndFormat/source",
			"fs/methods/MacheFS.Write/source",
			"nfsmount/methods/GraphFS.WriteFile/source",
		},
		"Format": {
			"writeback/functions/Format/source",
			"writeback/functions/SpliceAndFormat/source",
		},
		"Validate": {
			"writeback/functions/Validate/source",
			"writeback/functions/SpliceAndFormat/source",
		},

		// --- lattice package ---
		"FormalContext": {
			"lattice/types/FormalContext/source",
			"lattice/functions/NewFormalContext/source",
			"lattice/methods/FormalContext.Derive/source",
			"lattice/functions/NextClosure/source",
			"lattice/methods/Inferrer.Infer/source",
		},
		"NextClosure": {
			"lattice/functions/NextClosure/source",
			"lattice/methods/Inferrer.Infer/source",
		},
		"Inferrer": {
			"lattice/types/Inferrer/source",
			"lattice/methods/Inferrer.Infer/source",
			"cmd/functions/runMount/source",
		},

		// --- cmd package ---
		"buildGraph": {
			"cmd/functions/buildGraph/source",
			"cmd/functions/runServe/source",
			"cmd/functions/runMount/source",
		},

		// --- cross-cutting: standard library tokens used everywhere ---
		"Errorf": {
			"graph/methods/MemoryStore.GetNode/source",
			"graph/methods/MemoryStore.ReadContent/source",
			"graph/methods/SQLiteGraph.GetNode/source",
			"ingest/methods/Engine.Ingest/source",
			"ingest/functions/processNode/source",
			"fs/methods/MacheFS.Getattr/source",
			"nfsmount/methods/GraphFS.ReadFile/source",
			"writeback/functions/Splice/source",
			"lattice/functions/NextClosure/source",
			"cmd/functions/buildGraph/source",
		},
		"Sprintf": {
			"graph/functions/ComputeQuotient/source",
			"graph/methods/QuotientGraph.Mermaid/source",
			"graph/methods/MemoryStore.GetNode/source",
			"graph/methods/SQLiteGraph.GetNode/source",
			"ingest/functions/RenderTemplate/source",
			"ingest/functions/processNode/source",
			"fs/methods/MacheFS.Getattr/source",
			"nfsmount/methods/GraphFS.Stat/source",
			"writeback/functions/Splice/source",
			"lattice/methods/Inferrer.Infer/source",
			"cmd/functions/makeGetOverviewHandler/source",
			"cmd/functions/makeGetDiagramHandler/source",
		},
		"RLock": {
			"graph/methods/MemoryStore.GetNode/source",
			"graph/methods/MemoryStore.ListChildren/source",
			"graph/methods/MemoryStore.ReadContent/source",
			"graph/methods/MemoryStore.GetCallers/source",
			"graph/methods/MemoryStore.RefsMap/source",
		},
		"Lock": {
			"graph/methods/MemoryStore.AddNode/source",
			"graph/methods/MemoryStore.AddRef/source",
			"graph/methods/MemoryStore.AddDef/source",
		},

		// --- SQLiteGraph internal refs ---
		"SQLiteGraph": {
			"graph/types/SQLiteGraph/source",
			"graph/functions/OpenSQLiteGraph/source",
			"graph/methods/SQLiteGraph.GetNode/source",
			"graph/methods/SQLiteGraph.ListChildren/source",
			"graph/methods/SQLiteGraph.ReadContent/source",
			"graph/methods/SQLiteGraph.RefsMap/source",
			"graph/methods/SQLiteGraph.renderContent/source",
			"cmd/functions/buildGraph/source",
		},
		"OpenSQLiteGraph": {
			"graph/functions/OpenSQLiteGraph/source",
			"cmd/functions/buildGraph/source",
		},
		"SourceOrigin": {
			"graph/types/SourceOrigin/source",
			"graph/methods/MemoryStore.UpdateNodeContent/source",
			"ingest/functions/processNode/source",
			"writeback/functions/Splice/source",
		},

		// --- write-back bridge: graph <-> writeback ---
		"UpdateNodeContent": {
			"graph/methods/MemoryStore.UpdateNodeContent/source",
			"writeback/functions/SpliceAndFormat/source",
			"fs/methods/MacheFS.Write/source",
			"nfsmount/methods/GraphFS.WriteFile/source",
		},
		"ShiftOrigins": {
			"graph/methods/MemoryStore.ShiftOrigins/source",
			"writeback/functions/SpliceAndFormat/source",
		},

		// --- composite graph ---
		"CompositeGraph": {
			"graph/types/CompositeGraph/source",
			"graph/functions/NewCompositeGraph/source",
			"cmd/functions/buildGraph/source",
		},
	}
}

func TestQuotientGraph_SelfHosting(t *testing.T) {
	refs := buildMacheRefsMap()

	// Phase 1: Detect communities from the realistic refs map.
	cr := DetectCommunities(refs, 2)
	require.NotNil(t, cr, "community detection should not return nil")

	t.Logf("Community detection results:")
	t.Logf("  Nodes: %d, Edges: %d, Modularity: %.4f",
		cr.NumNodes, cr.NumEdges, cr.Modularity)
	t.Logf("  Communities detected: %d", len(cr.Communities))

	// Structural assertions on community detection.
	assert.GreaterOrEqual(t, len(cr.Communities), 3,
		"mache has at least 3 distinct packages (graph, ingest, fs/nfsmount/writeback/lattice/cmd) "+
			"that should form separate communities")
	assert.Greater(t, cr.Modularity, 0.0,
		"modularity should be positive for well-structured code")
	assert.Greater(t, cr.NumNodes, 30,
		"realistic refs map should have many nodes")
	assert.Greater(t, cr.NumEdges, 10,
		"realistic refs map should have many edges")

	for i, comm := range cr.Communities {
		t.Logf("  Community %d (%d members): %v", i, len(comm.Members), comm.Members)
	}

	// Phase 2: Compute the quotient graph.
	q := ComputeQuotient(cr, refs)
	require.NotNil(t, q, "quotient graph should not be nil")

	assert.Equal(t, len(cr.Communities), len(q.Classes),
		"quotient classes should match community count")
	assert.Greater(t, len(q.Edges), 0,
		"cross-community edges should exist (packages call each other)")

	t.Logf("\nQuotient graph:")
	for _, c := range q.Classes {
		t.Logf("  Class %d [%s] (%d members, internal_w=%.1f)",
			c.ID, c.Label, len(c.Members), c.InternalW)
	}
	for _, e := range q.Edges {
		t.Logf("  Edge C%d -> C%d (w=%.1f, tokens=%v)",
			e.From, e.To, e.Weight, e.Tokens)
	}

	// Phase 3: Render mermaid and validate syntax.
	mermaid := q.Mermaid("TD")
	require.NotEmpty(t, mermaid, "mermaid output should not be empty")

	t.Logf("\nMermaid output:\n%s", mermaid)

	// Basic mermaid syntax validation.
	assert.True(t, strings.HasPrefix(mermaid, "graph TD\n"),
		"mermaid should start with graph direction")

	// Validate structural properties of the mermaid output.
	lines := strings.Split(strings.TrimSpace(mermaid), "\n")
	assert.Greater(t, len(lines), 1,
		"mermaid should have more than just the header line")

	// Count subgraphs (multi-member communities) and plain nodes (singletons).
	subgraphCount := 0
	endCount := 0
	edgeCount := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "subgraph ") {
			subgraphCount++
		}
		if trimmed == "end" {
			endCount++
		}
		if strings.Contains(trimmed, "-->") {
			edgeCount++
		}
	}

	assert.Equal(t, subgraphCount, endCount,
		"every subgraph should have a matching end")
	assert.Greater(t, edgeCount, 0,
		"diagram should have at least one edge between communities")

	// Label quality: no community should be labeled with a trivial/ubiquitous token
	// like "Errorf" or "Sprintf" when more specific tokens are available.
	for _, c := range q.Classes {
		assert.NotEqual(t, "Errorf", c.Label,
			"community label should not be a generic stdlib function")
		assert.NotEqual(t, "Sprintf", c.Label,
			"community label should not be a generic stdlib function")
		assert.NotEqual(t, "cluster_0", c.Label,
			"community label should be derived from refs, not fallback")
		assert.NotEmpty(t, c.Label,
			"every community should have a label")
	}

	// Node IDs in mermaid should be sanitized (no slashes).
	for _, line := range lines {
		if strings.Contains(line, "subgraph") || strings.Contains(line, "-->") {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "end" || trimmed == "" || strings.HasPrefix(trimmed, "graph ") {
			continue
		}
		// Member lines inside subgraphs should not contain raw slashes.
		assert.NotContains(t, trimmed, "/",
			"mermaid node IDs should not contain raw slashes: %s", trimmed)
	}
}
