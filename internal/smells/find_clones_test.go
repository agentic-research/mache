package smells

import (
	"context"
	"database/sql"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
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

// funcNameAt is the name of the function or method declared at line of a Go
// source, or "" when no declaration starts there. Read from the syntax tree
// the Go parser builds, which is independent of the parse under test.
func funcNameAt(t *testing.T, src string, line int) string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "corpus.go", src, parser.SkipObjectResolution)
	require.NoError(t, err)
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fset.Position(fn.Pos()).Line == line {
			return fn.Name.Name
		}
	}
	return ""
}

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
			name := funcNameAt(t, body, m.Line)
			require.NotEmpty(t, name, "%s:%d is not a function declaration", m.SourceID, m.Line)
			assert.Contains(t, m.Snippet, name, "the snippet shows the copy")
			names = append(names, name)
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

// A database with no parse in it has nothing to compare, and the tool says
// so rather than reporting no clones.
func TestFindClones_WithoutAParseIsAnErrorNotAnEmptyAnswer(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "empty.db"))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	require.NoError(t, db.Ping())
	g := &testutil.SmellTestGraph{MemoryStore: graph.NewMemoryStore(), DB: db}
	res, err := MakeFindClonesHandler(g)(context.Background(), testutil.MakeRequest(nil))
	require.NoError(t, err)
	require.True(t, res.IsError, testutil.ResultText(t, res))
	assert.Contains(t, testutil.ResultText(t, res), "find_clones")
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

// diffCorpus is cloneCorpus plus pairs planted to differ in exactly one way
// the structure does not record.
//
//	capAbove / capBelow   one operator: > against <
//	shout / hush          one called function: ToUpper against ToLower
//	subLeft / subRight    the same two parameters, used in the other order
//	across / upward       methods that call different methods on another value
//	twiceWide / twiceTall methods that each call the method of their own name
func diffCorpus() map[string]string {
	files := map[string]string{}
	for p, body := range cloneCorpus {
		files[p] = body
	}
	files["pkg/ops.go"] = `package pkg

import "strings"

func capAbove(xs []int, limit int) int {
	n := 0
	for _, x := range xs {
		if x > limit {
			n++
		}
	}
	return n
}

func capBelow(ys []int, bound int) int {
	c := 0
	for _, y := range ys {
		if y < bound {
			c++
		}
	}
	return c
}

func shout(s string) string {
	// Loud.
	return strings.ToUpper(strings.TrimSpace(s))
}

func hush(t string) string {
	return strings.ToLower(strings.TrimSpace(t)) /* quiet */
}

type span struct{ inner box }

func (s span) across() int {
	return s.inner.widthOf()
}

func (s span) upward() int {
	return s.inner.heightOf()
}

func (b box) twiceWide() int {
	return 2 * b.twiceWide()
}

func (b box) twiceTall() int {
	return 2 * b.twiceTall()
}

func subLeft(x, y int) int {
	return x - y
}

func subRight(p, q int) int {
	return q - p
}
`
	return files
}

// setDifferences is what find_clones says differs within one set, in a form
// the planted truth can be written in: each difference as
// "kind: name=text, name=text", names sorted.
type setDifferences struct {
	Identical bool
	Renamed   bool
	Diffs     []string
}

// differencesBySet calls find_clones on files and returns each set's
// differences, keyed by the set's sorted function names joined with "+".
func differencesBySet(t *testing.T, files map[string]string) map[string]setDifferences {
	t.Helper()
	g := parsedCorpus(t, files)
	_, resp := findCloneSets(t, g, files, map[string]any{"min_nodes": 0, "include_tests": true})
	out := map[string]setDifferences{}
	for _, s := range resp.Sets {
		require.Empty(t, s.DifferencesUnavailable)
		names := make([]string, len(s.Members))
		for i, m := range s.Members {
			names[i] = funcNameAt(t, files[m.SourceID], m.Line)
		}
		d := setDifferences{Identical: s.IdenticalModuloRenames, Renamed: s.RenamedPositions > 0}
		for _, diff := range s.Differences {
			require.Len(t, diff.Values, len(s.Members))
			var parts []string
			for i, v := range diff.Values {
				assert.Equal(t, s.Members[i].SourceID, v.SourceID)
				assert.Contains(t, strings.Split(files[v.SourceID], "\n")[v.Line-1], v.Text, "the line holds the text")
				parts = append(parts, names[i]+"="+v.Text)
			}
			slices.Sort(parts)
			d.Diffs = append(d.Diffs, diff.Kind+": "+strings.Join(parts, ", "))
		}
		sorted := slices.Clone(names)
		slices.Sort(sorted)
		out[strings.Join(sorted, "+")] = d
	}
	return out
}

// The claim, stated so it can be refuted: for every set, find_clones lists
// exactly the places where the copies do different things, and nothing else.
// Each expectation below is planted in the corpus source. Names, constants
// and operators are what the clone structure does not record, so they are
// what a merge can silently change.
func TestFindClones_DifferencesAreExactlyThePlantedOnes(t *testing.T) {
	want := map[string]setDifferences{
		// Three copies; only countBig's threshold differs.
		"SumPositive+TotalAboveZero+countBig": {Renamed: true, Diffs: []string{
			"literal: SumPositive=0, TotalAboveZero=0, countBig=100",
		}},
		"ParseFlag+ParseToggle": {Renamed: true, Diffs: []string{
			"literal: ParseFlag=on, ParseToggle=enabled",
			"literal: ParseFlag=yes, ParseToggle=true",
			"literal: ParseFlag=off, ParseToggle=disabled",
			"literal: ParseFlag=no, ParseToggle=false",
			"literal: ParseFlag=bad flag, ParseToggle=bad toggle",
		}},
		// The same code under other names: nothing to parameterize.
		"drainFirst+drainSecond": {Identical: true, Renamed: true},
		"mustBuild+mustMake": {Renamed: true, Diffs: []string{
			"literal: mustBuild=no parts, mustMake=no pieces",
		}},
		// A field is not a local: the two accessors read different data.
		"heightOf+widthOf": {Renamed: true, Diffs: []string{
			"reference: heightOf=h, widthOf=w",
		}},
		// One character apart, and opposite in meaning. The parse stores no
		// node for an operator, so only the text between nodes shows it.
		"capAbove+capBelow": {Renamed: true, Diffs: []string{
			"operator: capAbove=>, capBelow=<",
		}},
		// Comments differ between these two and are not a difference.
		"hush+shout": {Renamed: true, Diffs: []string{
			"reference: hush=ToLower, shout=ToUpper",
		}},
		// Each forwards to a different method of another value. That the
		// two methods are themselves differently named does not make the
		// call a renaming.
		"across+upward": {Renamed: true, Diffs: []string{
			"reference: across=widthOf, upward=heightOf",
		}},
		// A selector is never a local, so a method calling the method of
		// its own name is reported too: the tool cannot tell recursion from
		// forwarding to another value's method, and says so by listing it.
		"twiceTall+twiceWide": {Renamed: true, Diffs: []string{
			"reference: twiceTall=twiceTall, twiceWide=twiceWide",
		}},
		// Both rename (x, y) to (p, q), and then use them the other way
		// round: a consistent renaming cannot explain either position.
		"subLeft+subRight": {Renamed: true, Diffs: []string{
			"reference: subLeft=x, subRight=q",
			"reference: subLeft=y, subRight=p",
		}},
	}
	assert.Equal(t, want, differencesBySet(t, diffCorpus()))
}

func TestBetweenNodes(t *testing.T) {
	for in, want := range map[string]string{
		" > ":                         ">",
		"\n\t\tif ":                   "if",
		") { // why\n\treturn ":       ") { return",
		" /* a */ += /* b */ ":        "+=",
		" // only a comment":          "",
		"":                            "",
		" /* never closed":            "",
		"}\n\n// doc\n// more\nfunc ": "} func",
	} {
		assert.Equal(t, want, betweenNodes([]byte(in)), "%q", in)
	}
}

// A local the copy declares under two names, or two locals it gives one
// name, is no renaming: positions that use it must be reported, not passed.
func TestLocalRenames_RefusesAnInconsistentMapping(t *testing.T) {
	tok := func(names ...string) []cloneToken {
		out := make([]cloneToken, len(names))
		for i, n := range names {
			out[i] = cloneToken{kind: "identifier", text: n, declares: true}
		}
		return out
	}
	consistent := localRenames(cloneCopy{tokens: tok("a", "b", "a")}, cloneCopy{tokens: tok("x", "y", "x")})
	assert.Equal(t, map[string]string{"a": "x", "b": "y"}, consistent)

	split := localRenames(cloneCopy{tokens: tok("a", "b", "a")}, cloneCopy{tokens: tok("x", "y", "z")})
	assert.Equal(t, map[string]string{"b": "y"}, split, "a is declared as both x and z")

	merged := localRenames(cloneCopy{tokens: tok("a", "b")}, cloneCopy{tokens: tok("x", "x")})
	assert.Empty(t, merged, "a and b both become x")
}
