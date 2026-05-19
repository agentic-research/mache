package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestParseProfile(t *testing.T) {
	in := `mode: set
github.com/x/y/foo.go:10.20,15.2 3 1
github.com/x/y/foo.go:20.2,22.3 2 0
github.com/x/y/bar.go:5.1,9.2 4 2
`
	prof, err := parseProfile(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parseProfile: %v", err)
	}
	want := profile{
		"github.com/x/y/foo.go": {
			{startLine: 10, endLine: 15, hits: 1},
			{startLine: 20, endLine: 22, hits: 0},
		},
		"github.com/x/y/bar.go": {
			{startLine: 5, endLine: 9, hits: 2},
		},
	}
	if !reflect.DeepEqual(prof, want) {
		t.Fatalf("profile mismatch\n got=%#v\nwant=%#v", prof, want)
	}
}

func TestParseProfileMalformed(t *testing.T) {
	cases := []string{
		"mode: set\nno-colon-here 1 1\n",
		"mode: set\nfoo.go:bad,10.2 1 1\n",
		"mode: set\nfoo.go:10.2,bad 1 1\n",
		"mode: set\nfoo.go:10.2,15.3 1 abc\n",
		"mode: set\nfoo.go:10-2,15.3 1 1\n", // span without comma
		"mode: set\nfoo.go:10.2,15.3 1\n",   // only 2 fields after colon
	}
	for i, c := range cases {
		if _, err := parseProfile(strings.NewReader(c)); err == nil {
			t.Errorf("case %d: expected error, got nil", i)
		}
	}
}

func TestParseDiff(t *testing.T) {
	in := `diff --git a/foo.go b/foo.go
--- a/foo.go
+++ b/foo.go
@@ -1,3 +5,6 @@
 context1
+added line 1
+added line 2
 context2
+added line 3
@@ -20,1 +30,2 @@
-removed
+replacement
`
	ds, err := parseDiff(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parseDiff: %v", err)
	}
	wantFoo := map[int]bool{6: true, 7: true, 9: true, 30: true}
	if !reflect.DeepEqual(ds["foo.go"], wantFoo) {
		t.Fatalf("foo.go lines mismatch\n got=%v\nwant=%v", ds["foo.go"], wantFoo)
	}
}

func TestIgnoreTestFiles(t *testing.T) {
	in := `diff --git a/x_test.go b/x_test.go
--- a/x_test.go
+++ b/x_test.go
@@ -1,1 +1,2 @@
 context
+added in test file
diff --git a/x.go b/x.go
--- a/x.go
+++ b/x.go
@@ -1,1 +5,2 @@
 context
+added in prod
`
	ds, err := parseDiff(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parseDiff: %v", err)
	}
	if _, ok := ds["x_test.go"]; ok {
		t.Errorf("expected x_test.go to be excluded, got: %v", ds["x_test.go"])
	}
	if ds["x.go"][6] != true {
		t.Errorf("expected x.go line 6 to be tracked, got: %v", ds["x.go"])
	}
}

func TestIgnoreTestdataAndNonGo(t *testing.T) {
	in := `diff --git a/internal/foo/testdata/x.go b/internal/foo/testdata/x.go
--- a/internal/foo/testdata/x.go
+++ b/internal/foo/testdata/x.go
@@ -1,1 +1,2 @@
 context
+added in testdata
diff --git a/README.md b/README.md
--- a/README.md
+++ b/README.md
@@ -1,1 +1,2 @@
 context
+added text
`
	ds, err := parseDiff(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parseDiff: %v", err)
	}
	if _, ok := ds["internal/foo/testdata/x.go"]; ok {
		t.Errorf("expected testdata to be excluded")
	}
	if _, ok := ds["README.md"]; ok {
		t.Errorf("expected non-.go file to be excluded")
	}
}

func TestIgnoreComments(t *testing.T) {
	in := `diff --git a/x.go b/x.go
--- a/x.go
+++ b/x.go
@@ -1,1 +1,8 @@
 context
+// a comment line
+/* block comment */
+   // indented comment
+
+real code line
+code with trailing // coverage:ignore
+another real
`
	ds, err := parseDiff(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parseDiff: %v", err)
	}
	// Lines 2,3,4 are comments. Line 5 is blank. Line 6 is real. Line 7 has
	// `coverage:ignore` and is excluded. Line 8 is real.
	if ds["x.go"][2] {
		t.Errorf("line 2 (// comment) should be excluded")
	}
	if ds["x.go"][3] {
		t.Errorf("line 3 (/* block) should be excluded")
	}
	if ds["x.go"][4] {
		t.Errorf("line 4 (indented //) should be excluded")
	}
	if ds["x.go"][5] {
		t.Errorf("line 5 (blank) should be excluded")
	}
	if !ds["x.go"][6] {
		t.Errorf("line 6 (real code) should be tracked")
	}
	if ds["x.go"][7] {
		t.Errorf("line 7 (coverage:ignore) should be excluded")
	}
	if !ds["x.go"][8] {
		t.Errorf("line 8 (real code) should be tracked")
	}
}

func TestIntersect(t *testing.T) {
	prof := profile{
		"foo.go": {
			{startLine: 10, endLine: 12, hits: 1}, // covered
			{startLine: 20, endLine: 25, hits: 0}, // uncovered
			{startLine: 30, endLine: 30, hits: 0}, // uncovered
		},
	}
	ds := diffSet{
		"foo.go": {
			10: true, // covered → ok
			11: true, // covered → ok
			20: true, // uncovered → flag
			22: true, // uncovered → flag
			25: true, // uncovered → flag
			30: true, // uncovered → flag
			50: true, // outside any range → skip
		},
		"missing.go": {
			3: true, // file not in profile → skip
		},
	}
	got := intersect(prof, ds)
	want := map[string][]int{
		"foo.go": {20, 22, 25, 30},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("intersect mismatch\n got=%v\nwant=%v", got, want)
	}
}

func TestIntersectOverlappingCoveredWins(t *testing.T) {
	// If two cover ranges overlap and one is covered, the covered one wins.
	prof := profile{
		"foo.go": {
			{startLine: 10, endLine: 20, hits: 0},
			{startLine: 15, endLine: 17, hits: 5},
		},
	}
	ds := diffSet{
		"foo.go": {15: true, 16: true, 19: true},
	}
	got := intersect(prof, ds)
	want := map[string][]int{
		"foo.go": {19}, // 15 and 16 are inside the hits=5 range; 19 only in hits=0
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("overlap test\n got=%v\nwant=%v", got, want)
	}
}

func TestCollapseBlocks(t *testing.T) {
	got := collapseBlocks([]int{1, 2, 3, 5, 7, 8})
	want := []uncoveredBlock{
		{startLine: 1, endLine: 3},
		{startLine: 5, endLine: 5},
		{startLine: 7, endLine: 8},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("collapseBlocks\n got=%v\nwant=%v", got, want)
	}
	if collapseBlocks(nil) != nil {
		t.Errorf("expected nil from empty input")
	}
}

func TestParseHunkNewStart(t *testing.T) {
	cases := map[string]int{
		"@@ -1,3 +5,6 @@":            5,
		"@@ -1,3 +5,6 @@ func foo()": 5,
		"@@ -1 +42 @@":               42,
	}
	for s, want := range cases {
		got, err := parseHunkNewStart(s)
		if err != nil {
			t.Errorf("%q: %v", s, err)
			continue
		}
		if got != want {
			t.Errorf("%q: got %d want %d", s, got, want)
		}
	}
}

// --- Integration tests using a built binary ---

func buildBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "coverage-gate")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	return bin
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func TestExitCodeZeroWhenCovered(t *testing.T) {
	bin := buildBinary(t)
	cover := "mode: set\nfoo.go:10.1,12.2 2 1\n"
	diff := `diff --git a/foo.go b/foo.go
--- a/foo.go
+++ b/foo.go
@@ -1,1 +10,2 @@
 context
+added code
`
	covPath := writeTemp(t, "cover.out", cover)
	diffPath := writeTemp(t, "diff.patch", diff)
	cmd := exec.Command(bin, covPath, diffPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expected exit 0, got error: %v\noutput=%s", err, out)
	}
	if strings.Contains(string(out), "NEW PROD LINES NOT COVERED") {
		t.Errorf("did not expect uncovered report, got: %s", out)
	}
}

func TestExitCodeOneWhenUncovered(t *testing.T) {
	bin := buildBinary(t)
	cover := "mode: set\nfoo.go:10.1,12.2 2 0\n"
	diff := `diff --git a/foo.go b/foo.go
--- a/foo.go
+++ b/foo.go
@@ -1,1 +10,2 @@
 context
+added code
`
	covPath := writeTemp(t, "cover.out", cover)
	diffPath := writeTemp(t, "diff.patch", diff)
	cmd := exec.Command(bin, covPath, diffPath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected exit 1, got success\noutput=%s", out)
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected ExitError, got %T: %v", err, err)
	}
	if exitErr.ExitCode() != 1 {
		t.Errorf("expected exit code 1, got %d", exitErr.ExitCode())
	}
	if !strings.Contains(string(out), "NEW PROD LINES NOT COVERED") {
		t.Errorf("missing report header, got: %s", out)
	}
	if !strings.Contains(string(out), "L11") {
		t.Errorf("expected L11 in report, got: %s", out)
	}
}

func TestExitCodeOneReportFormat(t *testing.T) {
	bin := buildBinary(t)
	// Two files, one with a contiguous range, one with a single line.
	cover := `mode: set
a.go:10.1,12.2 2 0
b.go:42.1,42.10 1 0
`
	diff := `diff --git a/a.go b/a.go
--- a/a.go
+++ b/a.go
@@ -1,1 +10,3 @@
+code1
+code2
+code3
diff --git a/b.go b/b.go
--- a/b.go
+++ b/b.go
@@ -1,1 +42,1 @@
+code
`
	covPath := writeTemp(t, "cover.out", cover)
	diffPath := writeTemp(t, "diff.patch", diff)
	cmd := exec.Command(bin, covPath, diffPath)
	out, _ := cmd.CombinedOutput()
	s := string(out)
	// Expect a.go block "L10-12" and b.go "L42", with files sorted.
	aIdx := strings.Index(s, "a.go")
	bIdx := strings.Index(s, "b.go")
	if aIdx < 0 || bIdx < 0 || aIdx > bIdx {
		t.Errorf("expected a.go before b.go in sorted output, got: %s", s)
	}
	if !strings.Contains(s, "L10-12") {
		t.Errorf("expected L10-12 range, got: %s", s)
	}
	if !strings.Contains(s, "L42\n") {
		t.Errorf("expected single L42, got: %s", s)
	}
}

func TestNormalizeProfilePaths(t *testing.T) {
	in := profile{
		"github.com/foo/bar/internal/x.go": {{startLine: 1, endLine: 2, hits: 1}},
		"github.com/foo/bar/cmd/main.go":   {{startLine: 5, endLine: 7, hits: 0}},
	}
	got := normalizeProfilePaths(in, "github.com/foo/bar")
	want := profile{
		"internal/x.go": {{startLine: 1, endLine: 2, hits: 1}},
		"cmd/main.go":   {{startLine: 5, endLine: 7, hits: 0}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalize mismatch\n got=%v\nwant=%v", got, want)
	}
	if !reflect.DeepEqual(normalizeProfilePaths(in, ""), in) {
		t.Errorf("empty modulePath should return unchanged")
	}
}

func TestIntersectStableSort(t *testing.T) {
	prof := profile{
		"a.go": {{startLine: 1, endLine: 100, hits: 0}},
	}
	ds := diffSet{"a.go": {}}
	for _, n := range []int{50, 5, 25, 75, 10} {
		ds["a.go"][n] = true
	}
	got := intersect(prof, ds)["a.go"]
	if !sort.IntsAreSorted(got) {
		t.Errorf("expected sorted output, got %v", got)
	}
}
