package graph

import (
	"math/rand"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// mergeSortedDedup
// ---------------------------------------------------------------------------

func TestMergeSortedDedup_BothEmpty(t *testing.T) {
	assert.Empty(t, mergeSortedDedup(nil, nil))
	assert.Empty(t, mergeSortedDedup([]string{}, []string{}))
}

func TestMergeSortedDedup_OneEmpty(t *testing.T) {
	a := []string{"a", "c", "e"}
	assert.Equal(t, a, mergeSortedDedup(a, nil))
	assert.Equal(t, a, mergeSortedDedup(nil, a))
}

func TestMergeSortedDedup_Disjoint(t *testing.T) {
	a := []string{"a", "c", "e"}
	b := []string{"b", "d", "f"}
	assert.Equal(t, []string{"a", "b", "c", "d", "e", "f"}, mergeSortedDedup(a, b))
}

func TestMergeSortedDedup_Overlapping(t *testing.T) {
	a := []string{"a", "b", "c", "d"}
	b := []string{"c", "d", "e", "f"}
	assert.Equal(t, []string{"a", "b", "c", "d", "e", "f"}, mergeSortedDedup(a, b))
}

func TestMergeSortedDedup_Identical(t *testing.T) {
	a := []string{"x", "y", "z"}
	assert.Equal(t, []string{"x", "y", "z"}, mergeSortedDedup(a, a))
}

func TestMergeSortedDedup_DuplicatesWithinInput(t *testing.T) {
	// Input may have internal dupes (e.g. unsorted batch that was sorted but not compacted)
	a := []string{"a", "a", "b"}
	b := []string{"b", "c", "c"}
	assert.Equal(t, []string{"a", "b", "c"}, mergeSortedDedup(a, b))
}

func TestMergeSortedDedup_SingleElements(t *testing.T) {
	assert.Equal(t, []string{"a"}, mergeSortedDedup([]string{"a"}, []string{"a"}))
	assert.Equal(t, []string{"a", "b"}, mergeSortedDedup([]string{"a"}, []string{"b"}))
}

func TestMergeSortedDedup_PreservesIsChildContract(t *testing.T) {
	// The output must work with sort.SearchStrings (binary search).
	// This is the contract that isChild in sqlite_graph.go depends on.
	a := []string{"vulns/2024/01", "vulns/2024/03", "vulns/2024/05"}
	b := []string{"vulns/2024/02", "vulns/2024/03", "vulns/2024/04"}
	merged := mergeSortedDedup(a, b)

	require.True(t, sort.StringsAreSorted(merged), "output must be sorted for binary search")
	// Verify binary search finds every element
	for _, want := range merged {
		idx := sort.SearchStrings(merged, want)
		require.True(t, idx < len(merged) && merged[idx] == want,
			"sort.SearchStrings must find %q", want)
	}
}

// ---------------------------------------------------------------------------
// compactSorted
// ---------------------------------------------------------------------------

func TestCompactSorted_Empty(t *testing.T) {
	assert.Empty(t, compactSorted(nil))
}

func TestCompactSorted_SingleElement(t *testing.T) {
	s := []string{"a"}
	got := compactSorted(s)
	assert.Equal(t, []string{"a"}, got)
}

func TestCompactSorted_NoDupes(t *testing.T) {
	s := []string{"a", "b", "c"}
	assert.Equal(t, s, compactSorted(s))
}

func TestCompactSorted_AllDupes(t *testing.T) {
	assert.Equal(t, []string{"x"}, compactSorted([]string{"x", "x", "x"}))
}

func TestCompactSorted_Mixed(t *testing.T) {
	assert.Equal(t, []string{"a", "b", "c"}, compactSorted([]string{"a", "a", "b", "c", "c"}))
}

// ---------------------------------------------------------------------------
// flushChildSlices integration: the merge path that had zero test coverage
// ---------------------------------------------------------------------------

func TestFlushChildSlices_MergePath(t *testing.T) {
	target := &sync.Map{}

	// Simulate batch 1: store initial sorted children
	batch1 := map[string][]string{
		"vulns/2024": {"vulns/2024/01", "vulns/2024/03", "vulns/2024/05"},
	}
	flushChildSlices(batch1, target)

	got1, ok := target.Load("vulns/2024")
	require.True(t, ok)
	assert.Equal(t, []string{"vulns/2024/01", "vulns/2024/03", "vulns/2024/05"}, got1.([]string))

	// Simulate batch 2: overlapping + new children — triggers merge path
	batch2 := map[string][]string{
		"vulns/2024": {"vulns/2024/02", "vulns/2024/03", "vulns/2024/04"},
	}
	flushChildSlices(batch2, target)

	got2, ok := target.Load("vulns/2024")
	require.True(t, ok)
	merged := got2.([]string)

	expected := []string{
		"vulns/2024/01", "vulns/2024/02", "vulns/2024/03",
		"vulns/2024/04", "vulns/2024/05",
	}
	assert.Equal(t, expected, merged)
	assert.True(t, sort.StringsAreSorted(merged), "result must be sorted for isChild binary search")
}

func TestFlushChildSlices_MultipleParents(t *testing.T) {
	target := &sync.Map{}

	batch := map[string][]string{
		"a": {"a/1", "a/2"},
		"b": {"b/1"},
	}
	flushChildSlices(batch, target)

	a, _ := target.Load("a")
	b, _ := target.Load("b")
	assert.Equal(t, []string{"a/1", "a/2"}, a.([]string))
	assert.Equal(t, []string{"b/1"}, b.([]string))
}

func TestFlushChildSlices_UnsortedInput(t *testing.T) {
	target := &sync.Map{}

	// Batch input is NOT pre-sorted — flushChildSlices must handle this
	batch := map[string][]string{
		"p": {"p/z", "p/a", "p/m", "p/a"}, // unsorted with dupe
	}
	flushChildSlices(batch, target)

	got, _ := target.Load("p")
	assert.Equal(t, []string{"p/a", "p/m", "p/z"}, got.([]string))
}

// ---------------------------------------------------------------------------
// Fuzz: merge output is always sorted+deduped regardless of input
// ---------------------------------------------------------------------------

func TestMergeSortedDedup_Fuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 200; i++ {
		a := randomSortedPaths(rng, rng.Intn(100))
		b := randomSortedPaths(rng, rng.Intn(100))
		merged := mergeSortedDedup(a, b)
		require.True(t, sort.StringsAreSorted(merged),
			"iteration %d: output not sorted", i)
		for j := 1; j < len(merged); j++ {
			require.NotEqual(t, merged[j-1], merged[j],
				"iteration %d: duplicate at index %d", i, j)
		}
	}
}

// randomSortedPaths generates path-like strings matching production IDs
// (e.g., "vulns/2024/03/CVE-2024-1234/source"). Exercises lexicographic
// ordering with path separators, not just single characters.
func randomSortedPaths(rng *rand.Rand, n int) []string {
	segments := []string{
		"vulns", "advisories", "tools", "pkg", "cmd", "internal",
		"2023", "2024", "2025", "01", "06", "12",
		"CVE-2024-1234", "CVE-2024-5678", "GHSA-abc", "GHSA-xyz",
		"source", "doc", "severity", "context",
	}
	s := make([]string, n)
	for i := range s {
		depth := 1 + rng.Intn(4) // 1-4 path segments
		parts := make([]string, depth)
		for d := range parts {
			parts[d] = segments[rng.Intn(len(segments))]
		}
		s[i] = parts[0]
		for _, p := range parts[1:] {
			s[i] += "/" + p
		}
	}
	sort.Strings(s)
	return s
}
