package smells

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/lltest"
	machetmpl "github.com/agentic-research/mache/internal/template"
	"github.com/agentic-research/mache/internal/testutil"
)

// The planted corpus. Ground truth is stated here, in source, and the tool is
// judged against it on a real parse by the pinned leyline: nothing below
// describes an AST, so the test cannot pass by agreeing with its own model of
// one.
//
// PLANTED SETS (what find_clones must return, exactly):
//
//	accumulate: SumPositive, TotalAboveZero, countBig   renamed copies, 3 files, 2 packages
//	parse:      ParseFlag, ParseToggle                  one renamed copy
//	drain:      drainFirst, drainSecond                 accumulate's size, a different shape
//	accessor:   widthOf, heightOf                       under the default size floor
//	test-only:  mustBuild, mustMake                     both copies in test files
//
// PLANTED NON-CLONES (what it must not return):
//
//	SumWhile                    same job as accumulate, different control flow
//	SumPositiveLogged           accumulate plus one statement (near-miss; known gap)
//	ReportUsers, ReportOrders   differ overall, share an identical block (block-level; known gap)
var cloneCorpus = map[string]string{
	"pkg/alpha.go": `package pkg

import "errors"

func SumPositive(xs []int) int {
	total := 0
	for _, x := range xs {
		if x > 0 {
			total += x
		}
	}
	return total
}

func ParseFlag(s string) (bool, error) {
	switch s {
	case "on", "yes":
		return true, nil
	case "off", "no":
		return false, nil
	}
	return false, errors.New("bad flag")
}

type box struct{ w, h int }

func (b box) widthOf() int { return b.w }
`,
	"pkg/beta.go": `package pkg

import "fmt"

func TotalAboveZero(vals []int) int {
	sum := 0
	for _, v := range vals {
		if v > 0 {
			sum += v
		}
	}
	return sum
}

func SumWhile(xs []int) int {
	total, i := 0, 0
	for i < len(xs) {
		total += xs[i]
		i++
	}
	return total
}

func SumPositiveLogged(xs []int) int {
	total := 0
	for _, x := range xs {
		if x > 0 {
			total += x
		}
	}
	fmt.Println(total)
	return total
}

func (b box) heightOf() int { return b.h }
`,
	"pkg/delta.go": `package pkg

import "errors"

func ParseToggle(text string) (bool, error) {
	switch text {
	case "enabled", "true":
		return true, nil
	case "disabled", "false":
		return false, nil
	}
	return false, errors.New("bad toggle")
}

func ReportUsers(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return "users: " + out
}

func ReportOrders(ids []string, limit int) (string, bool) {
	if len(ids) > limit {
		return "", false
	}
	out := ""
	for i, n := range ids {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out, true
}
`,
	"other/epsilon.go": `package other

func drainFirst(xs []int) int {
	total := 0
	for _, x := range xs {
		for x > 0 {
			total += x
		}
	}
	return total
}

func drainSecond(ys []int) int {
	acc := 0
	for _, y := range ys {
		for y > 0 {
			acc += y
		}
	}
	return acc
}
`,
	"other/gamma.go": `package other

func countBig(items []int) int {
	n := 0
	for _, it := range items {
		if it > 100 {
			n += it
		}
	}
	return n
}
`,
	"pkg/a_test.go": `package pkg

import "testing"

func mustBuild(t *testing.T, parts []string) string {
	t.Helper()
	if len(parts) == 0 {
		t.Fatal("no parts")
	}
	joined := ""
	for _, p := range parts {
		joined += p
	}
	return joined
}
`,
	"pkg/b_test.go": `package pkg

import "testing"

func mustMake(t *testing.T, pieces []string) string {
	t.Helper()
	if len(pieces) == 0 {
		t.Fatal("no pieces")
	}
	whole := ""
	for _, p := range pieces {
		whole += p
	}
	return whole
}
`,
}

var (
	setAccumulate = []string{"SumPositive", "TotalAboveZero", "countBig"}
	setParse      = []string{"ParseFlag", "ParseToggle"}
	setDrain      = []string{"drainFirst", "drainSecond"}
	setAccessor   = []string{"heightOf", "widthOf"}
	setTestOnly   = []string{"mustBuild", "mustMake"}
)

// parsedCorpus writes files, parses them with the pinned leyline and returns
// a graph over the result. Skips when the pinned leyline is not available.
func parsedCorpus(t *testing.T, files map[string]string) *testutil.SmellTestGraph {
	t.Helper()
	ll := lltest.ResolveBinaryOrSkip(t)
	src := t.TempDir()
	for p, body := range files {
		require.NoError(t, os.MkdirAll(filepath.Join(src, filepath.Dir(p)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(src, p), []byte(body), 0o644))
	}
	dbPath := filepath.Join(t.TempDir(), "corpus.db")
	out, err := exec.Command(ll.Path, "parse", src, "-o", dbPath).CombinedOutput()
	require.NoError(t, err, "%s", out)
	db, err := openSmellDB(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	sg, err := graph.OpenSQLiteGraph(dbPath, &api.Topology{Version: api.SchemaVersion}, machetmpl.Render)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sg.Close() })
	return &testutil.SmellTestGraph{MemoryStore: graph.NewMemoryStore(), DB: db, Path: dbPath, ASTKinds: sg}
}

var funcNameRE = regexp.MustCompile(`^func (?:\([^)]*\) )?(\w+)`)

// findCloneSets calls find_clones and returns each set as the sorted names
// of its functions, read from the corpus at the line the tool reported, with
// the sets themselves sorted. Names are how the ground truth is stated.
func findCloneSets(t *testing.T, g graph.Graph, files map[string]string, args map[string]any) (sets [][]string, resp clonesResponse) {
	t.Helper()
	res, err := MakeFindClonesHandler(g)(context.Background(), testutil.MakeRequest(args))
	require.NoError(t, err)
	require.False(t, res.IsError, testutil.ResultText(t, res))
	require.NoError(t, json.Unmarshal([]byte(testutil.ResultText(t, res)), &resp))
	for _, s := range resp.Sets {
		require.Len(t, s.Members, s.Copies)
		require.GreaterOrEqual(t, s.Copies, 2, "a set of one is not a clone")
		var names []string
		for _, m := range s.Members {
			body, ok := files[m.SourceID]
			require.True(t, ok, "member in a file the corpus does not have: %s", m.SourceID)
			line := strings.Split(body, "\n")[m.Line-1]
			match := funcNameRE.FindStringSubmatch(line)
			require.NotNil(t, match, "%s:%d is not a function declaration: %q", m.SourceID, m.Line, line)
			assert.Contains(t, m.Snippet, match[1], "the snippet shows the copy")
			names = append(names, match[1])
		}
		slices.Sort(names)
		sets = append(sets, names)
	}
	slices.SortFunc(sets, func(a, b []string) int { return strings.Compare(a[0], b[0]) })
	return sets, resp
}

// The claim, stated so it can be refuted: on a corpus whose clones are known,
// find_clones returns exactly those sets. A missing copy, an extra copy, a
// merged or split set, or a non-clone reported all fail it.
func TestFindClones_ReturnsExactlyThePlantedSets(t *testing.T) {
	g := parsedCorpus(t, cloneCorpus)

	t.Run("default: production clones above the size floor", func(t *testing.T) {
		sets, resp := findCloneSets(t, g, cloneCorpus, nil)
		assert.Equal(t, [][]string{setParse, setAccumulate, setDrain}, sets)
		assert.Equal(t, 3, resp.TotalSets)
		assert.False(t, resp.Truncated)
		assert.NotEmpty(t, resp.NotDetected, "an answer says what it cannot see")
	})

	t.Run("min_nodes=0 adds the pair under the floor", func(t *testing.T) {
		sets, _ := findCloneSets(t, g, cloneCorpus, map[string]any{"min_nodes": 0})
		assert.Equal(t, [][]string{setParse, setAccumulate, setDrain, setAccessor}, sets)
	})

	t.Run("include_tests adds the test-only pair", func(t *testing.T) {
		sets, _ := findCloneSets(t, g, cloneCorpus, map[string]any{"include_tests": true})
		assert.Equal(t, [][]string{setParse, setAccumulate, setDrain, setTestOnly}, sets)
	})

	t.Run("source_id keeps the whole set, including copies elsewhere", func(t *testing.T) {
		sets, _ := findCloneSets(t, g, cloneCorpus, map[string]any{"source_id": "other/gamma.go"})
		assert.Equal(t, [][]string{setAccumulate}, sets)
	})

	t.Run("limit truncates and says so", func(t *testing.T) {
		sets, resp := findCloneSets(t, g, cloneCorpus, map[string]any{"limit": 1})
		assert.Len(t, sets, 1)
		assert.Equal(t, 3, resp.TotalSets)
		assert.True(t, resp.Truncated)
	})
}

// Sets are ranked by what one helper would delete, and that number is the
// duplicated size times the copies beyond the first.
func TestFindClones_RanksByRemovableNodes(t *testing.T) {
	g := parsedCorpus(t, cloneCorpus)
	_, resp := findCloneSets(t, g, cloneCorpus, map[string]any{"min_nodes": 0, "include_tests": true})
	require.NotEmpty(t, resp.Sets)
	for i, s := range resp.Sets {
		assert.Equal(t, s.Nodes*int64(s.Copies-1), s.RemovableNodes)
		if i > 0 {
			assert.GreaterOrEqual(t, resp.Sets[i-1].RemovableNodes, s.RemovableNodes)
		}
	}
}

// Change one copy's control flow and exactly that copy leaves its set. This
// is the mutation that separates "groups by structure" from "groups by
// anything else that happens to agree on the first corpus".
func TestFindClones_ACopyThatDivergesLeavesItsSet(t *testing.T) {
	mutated := map[string]string{}
	for p, body := range cloneCorpus {
		mutated[p] = body
	}
	mutated["pkg/beta.go"] = strings.Replace(cloneCorpus["pkg/beta.go"],
		"\t\tif v > 0 {\n\t\t\tsum += v\n\t\t}\n",
		"\t\tfor v > 0 {\n\t\t\tsum += v\n\t\t\tbreak\n\t\t}\n", 1)
	require.NotEqual(t, cloneCorpus["pkg/beta.go"], mutated["pkg/beta.go"], "the mutation must apply")

	sets, _ := findCloneSets(t, parsedCorpus(t, mutated), mutated, nil)
	assert.Equal(t, [][]string{setParse, {"SumPositive", "countBig"}, setDrain}, sets)
}

// The tool has no detector of its own: its sets are the duplicate_code
// rule's findings, partitioned. Same floor, same functions, every one in
// exactly one set.
func TestFindClones_SetsPartitionTheRulesFindings(t *testing.T) {
	g := parsedCorpus(t, cloneCorpus)
	res, err := MakeFindSmellsHandler(g)(context.Background(), testutil.MakeRequest(map[string]any{
		"rule": cloneRuleID, "min_metric": 0, "limit": 10000,
	}))
	require.NoError(t, err)
	var rule struct {
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(testutil.ResultText(t, res)), &rule))
	require.NotEmpty(t, rule.Findings)

	_, resp := findCloneSets(t, g, cloneCorpus, map[string]any{"min_nodes": 0, "include_tests": true})
	type cloneSite struct {
		file string
		line int
	}
	inSets := map[cloneSite]string{}
	for _, s := range resp.Sets {
		for _, m := range s.Members {
			k := cloneSite{m.SourceID, m.Line}
			require.Empty(t, inSets[k], "%v is in two sets", k)
			inSets[k] = s.ID
		}
	}
	require.Len(t, inSets, len(rule.Findings), "every finding is in a set, and nothing else is")
	for _, f := range rule.Findings {
		assert.Equal(t, f.Group, inSets[cloneSite{f.SourceID, f.Line}], "%s:%d", f.SourceID, f.Line)
		assert.NotEmpty(t, f.Group, "find_smells reports the group too")
	}
}

// KNOWN GAP: a near-miss copy. SumPositiveLogged is SumPositive with one
// statement added, which is how copies drift, and it is not reported:
// matching is on exact structure. Near-miss detection is the ley-line _hdc
// tier (mache-40ae5c). This pins today's miss, so the day it is detected this
// test fails and SumPositiveLogged moves into setAccumulate above.
func TestFindClones_KnownGap_NearMissCopyIsNotReported(t *testing.T) {
	sets, _ := findCloneSets(t, parsedCorpus(t, cloneCorpus), cloneCorpus, map[string]any{"min_nodes": 0, "include_tests": true})
	for _, s := range sets {
		assert.NotContains(t, s, "SumPositiveLogged",
			"near-miss clones are now detected: move SumPositiveLogged into setAccumulate and drop it from cloneBlindSpots")
	}
}

// KNOWN GAP: a repeated block. ReportUsers and ReportOrders share an
// identical loop inside bodies that otherwise differ, and neither is
// reported: only whole functions are compared. This pins today's miss, so the
// day block-level clones are detected this test fails and becomes a planted
// set.
func TestFindClones_KnownGap_SharedBlockIsNotReported(t *testing.T) {
	sets, _ := findCloneSets(t, parsedCorpus(t, cloneCorpus), cloneCorpus, map[string]any{"min_nodes": 0, "include_tests": true})
	for _, s := range sets {
		assert.NotContains(t, s, "ReportUsers",
			"block-level clones are now detected: plant ReportUsers/ReportOrders as a set and drop the entry from cloneBlindSpots")
		assert.NotContains(t, s, "ReportOrders")
	}
}

// Without an _ast table there is nothing to compare, and the tool says what
// is missing rather than reporting no clones.
func TestFindClones_WithoutASTNamesWhatIsMissing(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.DB.Close() }()
	_, err := tg.DB.Exec(`DROP TABLE _ast`)
	require.NoError(t, err)
	res, err := MakeFindClonesHandler(tg)(context.Background(), testutil.MakeRequest(nil))
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Contains(t, testutil.ResultText(t, res), "_ast")
}

// Two planted sets are the same size with different shapes, so grouping by
// anything coarser than structure (size, say) would merge them. This checks
// the corpus still has that property: an edit to it that separated the sizes
// would quietly stop the set-equality test from telling the two apart.
func TestFindClones_CorpusHasEqualSizedSetsOfDifferentShape(t *testing.T) {
	_, resp := findCloneSets(t, parsedCorpus(t, cloneCorpus), cloneCorpus, nil)
	nodes := map[string]int64{}
	for _, s := range resp.Sets {
		for _, m := range s.Members {
			nodes[m.SourceID] = s.Nodes
		}
	}
	require.NotZero(t, nodes["other/gamma.go"])
	assert.Equal(t, nodes["other/gamma.go"], nodes["other/epsilon.go"],
		"accumulate and drain must be the same size; adjust the corpus until they are")
}
