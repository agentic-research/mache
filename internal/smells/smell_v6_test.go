package smells

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/fixturedb"
)

// The smell engine on ley-line-open's projection-v6 (mache-90cb9f), whose node
// ids are integers. A rule reads them through the vocabulary as opaque keys;
// the engine renders them to paths for the finding and looks locations and
// content addresses up by the key, so a shape-agnostic rule reports the same
// findings from either projection.

func smellSpec(t *testing.T, p fixturedb.Producer) *fixturedb.FixtureDB {
	b := fixturedb.New(t, p)
	b.Source("pkg/a.go", "go", "")
	b.Source("pkg/b.go", "go", "")
	b.ASTNode("pkg/a.go", "source_file", "pkg/a.go", fixturedb.Span{EndByte: 900, EndRow: 90})
	b.ASTNode("pkg/a.go/function_declaration_0", "function_declaration", "pkg/a.go",
		fixturedb.Span{StartByte: 20, EndByte: 400, StartRow: 2, EndRow: 40})
	b.ASTNode("pkg/a.go/function_declaration_1", "function_declaration", "pkg/a.go",
		fixturedb.Span{StartByte: 410, EndByte: 900, StartRow: 41, StartCol: 1, EndRow: 90})
	b.ASTNode("pkg/b.go", "source_file", "pkg/b.go", fixturedb.Span{EndByte: 80, EndRow: 8})
	b.ASTNode("pkg/b.go/method_declaration", "method_declaration", "pkg/b.go",
		fixturedb.Span{StartByte: 10, EndByte: 80, StartRow: 1, EndRow: 8})
	_, f := b.Build()
	return f
}

func TestSmells_ShapeAgnosticRulesSameFindingsFromProjectionV4AndV6(t *testing.T) {
	for _, id := range []string{"long_function", "long_file"} {
		t.Run(id, func(t *testing.T) {
			run := func(p fixturedb.Producer) []smellFinding {
				f := smellSpec(t, p)
				out, err := RunSmellRule(f, embeddedRule(t, id), "", 100)
				require.NoError(t, err)
				for i := range out {
					out[i].key = nil // the raw key is a nid on one side by design
				}
				return out
			}
			v4 := run(fixturedb.Leyline)
			require.NotEmpty(t, v4, "the v4 side must find something, or the comparison is vacuous")
			assert.Equal(t, v4, run(fixturedb.LeylineV6))
		})
	}
}

// A finding on v6 names its node by path, carries the node's content address,
// and is located from _ast through the key.
func TestSmells_V6FindingIsAPathWithAHashAndALocation(t *testing.T) {
	out, err := RunSmellRule(smellSpec(t, fixturedb.LeylineV6), embeddedRule(t, "long_function"), "", 100)
	require.NoError(t, err)
	require.NotEmpty(t, out)
	top := out[0]
	assert.Equal(t, "pkg/a.go/function_declaration_1", top.NodeID)
	assert.Equal(t, "pkg/a.go", top.SourceID)
	assert.NotEmpty(t, top.NodeHash, "the ratchet keys construct findings on their content address")
	assert.Equal(t, 42, top.Line)
	assert.Equal(t, 2, top.Column)
}

// A rule that reads meaning into node_id's shape is refused on v6, where it
// would answer wrongly, and still runs where ids are paths.
func TestSmells_PathShapedRuleIsRefusedOnV6Only(t *testing.T) {
	rule := embeddedRule(t, "cyclomatic_complexity")
	require.True(t, rule.PathShapedIDs)

	_, err := RunSmellRule(smellSpec(t, fixturedb.LeylineV6), rule, "", 100)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrPathShapedRule), "got %v", err)
	assert.Contains(t, err.Error(), "mache-2dc105", "the error names the work that lifts it")

	_, err = RunSmellRule(smellSpec(t, fixturedb.Leyline), rule, "", 100)
	assert.NoError(t, err)
}

// A nid that names no node (rows LLO writes for injected-language nodes it
// does not store, ley-line-open-260fcf) renders as no node rather than as a
// number dressed up as an id.
func TestRenderNodeIDs_ANidWithNoNodeRendersAsNoNode(t *testing.T) {
	f := smellSpec(t, fixturedb.LeylineV6)
	findings := []smellFinding{{key: int64(1) << 40}, {key: "kept/as/is"}}
	require.NoError(t, renderNodeIDs(f, findings))
	assert.Empty(t, findings[0].NodeID)
	assert.Equal(t, "kept/as/is", findings[1].NodeID)
}

// A rule that emits no span (the node_defs-based ones emit 0 AS start_byte)
// is located from _ast by its key: on v6, the nid.
func TestEnrichLocations_V6BackfillsByKey(t *testing.T) {
	f := smellSpec(t, fixturedb.LeylineV6)
	require.NoError(t, ensureSmellQueryContext(f))
	var nid int64
	require.NoError(t, f.DB().QueryRow(`SELECT nid FROM v_node_path WHERE path = ?`,
		"pkg/a.go/function_declaration_1").Scan(&nid))

	findings := []smellFinding{{key: nid}}
	require.NoError(t, renderNodeIDs(f, findings))
	require.NoError(t, enrichLocations(f, findings))
	assert.Equal(t, "pkg/a.go/function_declaration_1", findings[0].NodeID)
	assert.Equal(t, "pkg/a.go", findings[0].SourceID, "the file comes from _ast, through the nid")
	assert.Equal(t, 410, findings[0].StartByte)
	assert.Equal(t, 42, findings[0].Line)
}
