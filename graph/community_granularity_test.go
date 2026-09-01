package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAggregateRefs_FileCellsAnswerTheFileQuestion pins the point of the
// feature. At construct granularity mache's own largest communities were
// CHANGELOG sections and test-function bodies — well-separated but useless for
// "how should this package be split". The fs hierarchy is already encoded in
// every node id (fs -> CST/AST), so file cells are a prefix operation.
func TestAggregateRefs_FileCellsAnswerTheFileQuestion(t *testing.T) {
	refs := map[string][]string{
		"Greet": {
			"cmd/serve.go/function_declaration_1/block/call_expression",
			"cmd/serve.go/function_declaration_2/block/call_expression",
			"cmd/build.go/function_declaration_0/block/call_expression",
		},
		"section-only": {"CHANGELOG.md/section/section_3/list/list_item_2"},
	}

	got := AggregateRefs(refs, GranularityFile, "")

	assert.Equal(t, []string{"cmd/serve.go", "cmd/build.go"}, got["Greet"],
		"three AST occurrences across two files are TWO file cells — deduped, order first-seen")
	assert.Equal(t, []string{"CHANGELOG.md"}, got["section-only"],
		"a single-cell token survives aggregation; buildProjection's len<2 skip drops it from pairs")
}

// TestAggregateRefs_DedupIsWhatPreventsChattyFileHubs: without per-token dedup
// a token referenced 40 times inside one file would weight that file's edges
// 40x — reintroducing at cell level exactly the hub distortion fan-in pruning
// just removed at token level.
func TestAggregateRefs_DedupIsWhatPreventsChattyFileHubs(t *testing.T) {
	nodes := make([]string, 40)
	for i := range nodes {
		nodes[i] = "a/chatty.go/function_declaration_0/block/x"
	}
	refs := map[string][]string{"tok": append(nodes, "a/other.go/function_declaration_0")}

	got := AggregateRefs(refs, GranularityFile, "")

	assert.Equal(t, []string{"a/chatty.go", "a/other.go"}, got["tok"],
		"41 occurrences collapse to 2 cells: presence, not chatter, is the signal")
}

// TestAggregateRefs_ScopeFiltersBeforeAggregation: scoping to cmd/ must apply
// on the RAW id, whatever the cell size — a dir cell named "cmd" still only
// contains cmd/'s nodes.
func TestAggregateRefs_ScopeFiltersBeforeAggregation(t *testing.T) {
	refs := map[string][]string{
		"tok": {
			"cmd/a.go/function_declaration_0",
			"cmd/b.go/function_declaration_0",
			"graph/c.go/function_declaration_0",
		},
	}

	file := AggregateRefs(refs, GranularityFile, "cmd/")
	assert.Equal(t, []string{"cmd/a.go", "cmd/b.go"}, file["tok"])

	dir := AggregateRefs(refs, GranularityDir, "cmd/")
	assert.Equal(t, []string{"cmd"}, dir["tok"],
		"both survivors share the dir cell; graph/ is excluded before aggregation")

	construct := AggregateRefs(refs, GranularityConstruct, "cmd/")
	require.Len(t, construct["tok"], 2, "scope works at construct granularity too")
}

// TestAggregateRefs_ConstructUnscopedIsIdentity: the default path must hand the
// caller's map straight back — RefsMap snapshots are shared, and the common
// case should allocate nothing.
func TestAggregateRefs_ConstructUnscopedIsIdentity(t *testing.T) {
	refs := map[string][]string{"tok": {"a.go/x"}}
	got := AggregateRefs(refs, GranularityConstruct, "")
	assert.Equal(t, refs, got)
}

// TestFileOf_TrimsAtTheFirstDottedComponent documents the boundary heuristic
// and its accepted failure mode.
func TestFileOf_TrimsAtTheFirstDottedComponent(t *testing.T) {
	for _, tc := range []struct{ id, want string }{
		{"cmd/serve.go/function_declaration_1/block", "cmd/serve.go"},
		{"a.go/function_declaration_0", "a.go"},
		{"CHANGELOG.md/section/section_3", "CHANGELOG.md"},
		{"a.go", "a.go"},
		// No dotted component anywhere: returned whole rather than guessed at.
		{"nodots/thing", "nodots/thing"},
		// A dotted DIRECTORY trims early — the cell gets coarser, which is a
		// grouping error, not a crash. Accepted and documented.
		{"v1.2/pkg/a.go/function_declaration_0", "v1.2"},
	} {
		assert.Equal(t, tc.want, fileOf(tc.id), "fileOf(%q)", tc.id)
	}
}

// TestDirOfFile_RootFilesShareACell: every top-level file maps to "." so the
// repo root is one cell, not one cell per file.
func TestDirOfFile_RootFilesShareACell(t *testing.T) {
	assert.Equal(t, "cmd", dirOfFile("cmd/serve.go"))
	assert.Equal(t, "a/b", dirOfFile("a/b/c.go"))
	assert.Equal(t, ".", dirOfFile("CHANGELOG.md"))
}
