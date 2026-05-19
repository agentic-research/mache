package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// errReader is an io.Reader that returns a non-EOF error on first Read.
// Used to drive bufio.Scanner into its err-not-EOF path so we can pin
// parseProfile / parseDiff's `if err := sc.Err(); err != nil` branches.
type errReader struct{ err error }

func (e *errReader) Read([]byte) (int, error) { return 0, e.err }

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

// --- In-process run() tests ---
//
// These call run() directly (no os/exec) so the cover profile counts
// every line of main()'s logic. The os/exec integration tests above
// stay — they exercise the real binary end-to-end — but coverage of
// run() comes from these.

func TestRun_CleanExitOnCoveredDiff(t *testing.T) {
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
	var stdout, stderr bytes.Buffer
	code := run("coverage-gate", []string{covPath, diffPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0 on clean diff, got %d (stderr=%q stdout=%q)", code, stderr.String(), stdout.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("expected empty stdout on clean exit, got %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("expected empty stderr on clean exit, got %q", stderr.String())
	}
}

func TestRun_NonZeroExitWithUncoveredLines(t *testing.T) {
	// Two files so we exercise the sort + multi-file render path.
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
	var stdout, stderr bytes.Buffer
	code := run("coverage-gate", []string{covPath, diffPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit 1, got %d (stderr=%q)", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "NEW PROD LINES NOT COVERED:") {
		t.Errorf("missing header in stdout, got: %q", out)
	}
	// Files sorted: a.go before b.go.
	aIdx := strings.Index(out, "a.go")
	bIdx := strings.Index(out, "b.go")
	if aIdx < 0 || bIdx < 0 || aIdx > bIdx {
		t.Errorf("expected a.go before b.go in sorted output, got: %q", out)
	}
	// Range collapse: 10-12, single L42.
	if !strings.Contains(out, "L10-12") {
		t.Errorf("expected L10-12 collapsed range, got: %q", out)
	}
	if !strings.Contains(out, "L42\n") {
		t.Errorf("expected single L42, got: %q", out)
	}
	// Totals line.
	if !strings.Contains(out, "4 new prod line(s) uncovered") {
		t.Errorf("expected '4 new prod line(s) uncovered' total, got: %q", out)
	}
}

func TestRun_ErrorOnMissingProfile(t *testing.T) {
	// Profile path that doesn't exist; diff is irrelevant (won't be read).
	missing := filepath.Join(t.TempDir(), "does-not-exist.cov")
	diffPath := writeTemp(t, "diff.patch", "")
	var stdout, stderr bytes.Buffer
	code := run("coverage-gate", []string{missing, diffPath}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit 2 on missing profile, got %d (stderr=%q)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "error opening cover profile") {
		t.Errorf("expected 'error opening cover profile' in stderr, got: %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("expected empty stdout on io error, got %q", stdout.String())
	}
}

func TestRun_UsageOnWrongArgCount(t *testing.T) {
	// Zero args.
	var stdout, stderr bytes.Buffer
	code := run("coverage-gate", []string{}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit 2 on zero args, got %d", code)
	}
	if !strings.Contains(stderr.String(), "usage: coverage-gate") {
		t.Errorf("expected usage line in stderr, got: %q", stderr.String())
	}

	// One arg.
	stdout.Reset()
	stderr.Reset()
	code = run("coverage-gate", []string{"only-one"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit 2 on one arg, got %d", code)
	}

	// Three args.
	stdout.Reset()
	stderr.Reset()
	code = run("coverage-gate", []string{"a", "b", "c"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit 2 on three args, got %d", code)
	}
}

func TestRun_ErrorOnMalformedProfile(t *testing.T) {
	// Profile that parses past the mode header but trips parseProfile.
	covPath := writeTemp(t, "cover.out", "mode: set\nno-colon-here 1 1\n")
	diffPath := writeTemp(t, "diff.patch", "")
	var stdout, stderr bytes.Buffer
	code := run("coverage-gate", []string{covPath, diffPath}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit 2 on malformed profile, got %d", code)
	}
	if !strings.Contains(stderr.String(), "error parsing cover profile") {
		t.Errorf("expected 'error parsing cover profile' in stderr, got: %q", stderr.String())
	}
}

func TestRun_ErrorOnMissingDiff(t *testing.T) {
	// Valid (empty) profile, missing diff file.
	covPath := writeTemp(t, "cover.out", "mode: set\n")
	missingDiff := filepath.Join(t.TempDir(), "does-not-exist.diff")
	var stdout, stderr bytes.Buffer
	code := run("coverage-gate", []string{covPath, missingDiff}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit 2 on missing diff, got %d", code)
	}
	if !strings.Contains(stderr.String(), "error opening diff") {
		t.Errorf("expected 'error opening diff' in stderr, got: %q", stderr.String())
	}
}

func TestParseProfile_ScannerError(t *testing.T) {
	// Pins parseProfile's `if err := sc.Err(); err != nil` branch
	// (L199-200): bufio.Scanner surfaces non-EOF read errors via
	// Err() after Scan() returns false. We inject a custom reader
	// that fails on the first Read; parseProfile must propagate the
	// underlying error, not swallow it.
	want := errors.New("synthetic read failure")
	_, err := parseProfile(&errReader{err: want})
	if err == nil {
		t.Fatalf("expected parseProfile to surface reader error, got nil")
	}
	if !errors.Is(err, want) && !strings.Contains(err.Error(), want.Error()) {
		t.Errorf("expected %q in error, got %v", want, err)
	}
}

func TestIsCountableProdLine_BlockCommentContinuation(t *testing.T) {
	// Pins isCountableProdLine's `if strings.HasPrefix(trimmed, "*")`
	// branch (L402-404 in main.go): inside a /* */ block comment the
	// continuation lines typically start with " * ..." — these must
	// NOT be flagged as countable prod lines.
	if isCountableProdLine(" * continuation line") {
		t.Errorf("expected ' * continuation' to be excluded as block-comment continuation")
	}
	if isCountableProdLine("\t* tab-indented continuation") {
		t.Errorf("expected tab-indented '* ...' continuation to be excluded")
	}
}

func TestParseDiff_NoNewlineAtEndOfFile(t *testing.T) {
	// Pins the `case '\\':` branch in parseDiff (L345 in main.go).
	// `\ No newline at end of file` is a marker emitted by `git diff`
	// when the file lacks a trailing newline. It must NOT advance
	// newLine and must NOT be flagged as countable.
	in := `diff --git a/x.go b/x.go
--- a/x.go
+++ b/x.go
@@ -1,1 +5,2 @@
 context
+added without trailing newline
\ No newline at end of file
`
	ds, err := parseDiff(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parseDiff: %v", err)
	}
	// L6 is the "+added" line; L7 would be the "\ No newline" marker.
	// Marker must not produce a tracked line at 7.
	if ds["x.go"][7] {
		t.Errorf("'\\ No newline at end of file' marker must not be counted as a prod line, got L7 in: %v", ds["x.go"])
	}
	if !ds["x.go"][6] {
		t.Errorf("expected the real added line at L6, got: %v", ds["x.go"])
	}
}

func TestParseDiff_ScannerError(t *testing.T) {
	// Pins parseDiff's `if err := sc.Err(); err != nil` branch
	// (L350-351). Same mechanism as TestParseProfile_ScannerError.
	want := errors.New("synthetic read failure")
	_, err := parseDiff(&errReader{err: want})
	if err == nil {
		t.Fatalf("expected parseDiff to surface reader error, got nil")
	}
	if !errors.Is(err, want) && !strings.Contains(err.Error(), want.Error()) {
		t.Errorf("expected %q in error, got %v", want, err)
	}
}

// silence: ensure io is used even on platforms / build configs where
// the only test reference happens to be in a guarded path.
var _ = io.Reader(&errReader{})

func TestParseProfile_SkipsEmptyLines(t *testing.T) {
	// An empty line mid-profile must be tolerated (not error), and the
	// subsequent records must still parse. Pins parseProfile's
	// `if line == "" { continue }` branch (L163).
	in := "mode: set\nfoo.go:1.1,2.2 1 1\n\nbar.go:5.1,9.2 4 0\n"
	prof, err := parseProfile(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parseProfile: %v", err)
	}
	if len(prof) != 2 {
		t.Errorf("expected 2 files parsed across the empty line, got %d (%v)", len(prof), prof)
	}
}

func TestParseProfile_SpanWithoutComma(t *testing.T) {
	// Pins parseProfile's `if comma < 0 { error }` branch (L182-183).
	// The token has 3 fields after the colon but the first field has
	// no comma — `10.1 15.3 1` parses to len(parts)==3 but span lacks ','.
	in := "mode: set\nfoo.go:10.1 1 1\n"
	if _, err := parseProfile(strings.NewReader(in)); err == nil {
		t.Errorf("expected error on span without comma, got nil")
	}
}

func TestParseHunkNewStart_NoPlus(t *testing.T) {
	// Pins the `if plus < 0 { error }` branch (L359-360) of
	// parseHunkNewStart. The hunk header literally has no '+' so the
	// parser must return an error rather than panic on rest[plus+1:].
	if _, err := parseHunkNewStart("@@ -1,3 -5,6 @@"); err == nil {
		t.Errorf("expected error when hunk header has no '+', got nil")
	}
}

func TestParseHunkNewStart_NoTerminator(t *testing.T) {
	// Pins the `if end < 0 { return strconv.Atoi(rest) }` branch
	// (L365-366) — the substring after '+' has neither space nor comma,
	// so end == -1 and parseHunkNewStart Atois the whole tail.
	got, err := parseHunkNewStart("@@ -1 +42")
	if err != nil {
		t.Fatalf("parseHunkNewStart: %v", err)
	}
	if got != 42 {
		t.Errorf("expected 42, got %d", got)
	}
}

func TestParseDiff_DevNullSkipped(t *testing.T) {
	// Pins the `/dev/null` branch in parseDiff (L298-300): a file
	// deletion shows `+++ /dev/null`, which must be skipped (no entries
	// for it in the diff set).
	in := `diff --git a/gone.go b/gone.go
--- a/gone.go
+++ /dev/null
@@ -1,2 +0,0 @@
-old line 1
-old line 2
diff --git a/kept.go b/kept.go
--- a/kept.go
+++ b/kept.go
@@ -1,1 +5,2 @@
 context
+added
`
	ds, err := parseDiff(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parseDiff: %v", err)
	}
	if _, ok := ds["gone.go"]; ok {
		t.Errorf("expected gone.go (/dev/null target) to be skipped, got: %v", ds["gone.go"])
	}
	if !ds["kept.go"][6] {
		t.Errorf("expected kept.go L6 tracked after /dev/null hunk, got: %v", ds["kept.go"])
	}
}

func TestParseDiff_BlankLineInHunkAdvances(t *testing.T) {
	// Pins parseDiff's `if len(line) == 0 { newLine++ }` branch
	// (L323-325). A bare empty line inside a hunk is treated as a
	// context line and must advance newLine — proven by the line
	// number assigned to the next '+' addition.
	in := "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1,3 +10,4 @@\n context1\n\n context3\n+added\n"
	ds, err := parseDiff(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parseDiff: %v", err)
	}
	// Hunk starts at newLine=10. context1 → 11, blank → 12, context3 →
	// 13, +added at 13. (Three pre-existing-context-like lines advance
	// newLine three times before the '+'.)
	if !ds["x.go"][13] {
		t.Errorf("expected x.go L13 from added line after blank-in-hunk, got: %v", ds["x.go"])
	}
}

func TestParentDir_Root(t *testing.T) {
	// Pins parentDir at filesystem root (L252-253): when i==0 and
	// path[0] is a separator, must return string(os.PathSeparator).
	sep := string(os.PathSeparator)
	if got := parentDir(sep); got != sep {
		t.Errorf("parentDir(%q) = %q, want %q", sep, got, sep)
	}
	// Also pins the no-separator fall-through (last return path):
	if got := parentDir("nosep"); got != "nosep" {
		t.Errorf("parentDir(\"nosep\") = %q, want \"nosep\"", got)
	}
}

func TestModulePathFromGoMod_WalksUpAndStops(t *testing.T) {
	// Pins modulePathFromGoMod's "reached filesystem root, no go.mod"
	// branch (L241-242) by chdir'ing into a tmpdir that contains no
	// go.mod anywhere on its path up to root. We then assert the
	// function returns "".
	//
	// macOS /private/tmp is hierarchically below /, and there's no
	// go.mod in /private, /tmp, or / — so the walk terminates with "".
	dir := t.TempDir()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir tmp: %v", err)
	}
	got := modulePathFromGoMod()
	if got != "" {
		t.Errorf("expected empty module path when no go.mod on the path, got %q", got)
	}
}

func TestRun_ErrorOnMalformedDiff(t *testing.T) {
	covPath := writeTemp(t, "cover.out", "mode: set\nfoo.go:10.1,12.2 2 0\n")
	// Hunk header with garbage after the '+' so parseHunkNewStart fails.
	badDiff := `diff --git a/foo.go b/foo.go
--- a/foo.go
+++ b/foo.go
@@ -1,1 +notanumber @@
+code
`
	diffPath := writeTemp(t, "diff.patch", badDiff)
	var stdout, stderr bytes.Buffer
	code := run("coverage-gate", []string{covPath, diffPath}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit 2 on malformed diff, got %d (stderr=%q)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "error parsing diff") {
		t.Errorf("expected 'error parsing diff' in stderr, got: %q", stderr.String())
	}
}

// --- Rename / similarity detection tests ---
//
// `git diff -M` and `git diff -C` emit per-block headers like:
//
//	diff --git a/old.go b/new.go
//	similarity index 80%
//	rename from old.go
//	rename to new.go
//	--- a/old.go
//	+++ b/new.go
//	@@ ... @@
//	 <context>
//	+<moved line>
//
// When similarity is at or above -rename-threshold, the `+` lines are
// not new code — they're the moved-byte portion of a refactor. These
// tests pin the threshold semantics and lock down regression behavior
// for the godfile-decomposition campaign that surfaced bead mache-e256f5.

func TestParseDiff_RenameHunkExcludesAllLines(t *testing.T) {
	// similarity 100% — every `+` line in the rename block is a pure
	// move. Even with zero coverage we must NOT flag anything.
	in := `diff --git a/old.go b/new.go
similarity index 100%
rename from old.go
rename to new.go
--- a/old.go
+++ b/new.go
@@ -1,3 +1,3 @@
+moved line 1
+moved line 2
+moved line 3
`
	ds, err := parseDiffWithRenames(strings.NewReader(in), 50)
	if err != nil {
		t.Fatalf("parseDiffWithRenames: %v", err)
	}
	if got, ok := ds["new.go"]; ok && len(got) > 0 {
		t.Errorf("expected NO lines flagged for 100%% rename, got: %v", got)
	}
}

func TestParseDiff_RenameHunkBelowThresholdCountsAsNew(t *testing.T) {
	// similarity 30%, threshold default 50 — block is NOT treated as a
	// pure move; `+` lines must still appear in the diff set.
	in := `diff --git a/old.go b/new.go
similarity index 30%
rename from old.go
rename to new.go
--- a/old.go
+++ b/new.go
@@ -1,1 +1,3 @@
 context
+new line 1
+new line 2
`
	ds, err := parseDiffWithRenames(strings.NewReader(in), 50)
	if err != nil {
		t.Fatalf("parseDiffWithRenames: %v", err)
	}
	// new.go line 2 and 3 (after the context line at 1) should be tracked.
	if !ds["new.go"][2] {
		t.Errorf("expected new.go L2 tracked (similarity 30 < threshold 50), got: %v", ds["new.go"])
	}
	if !ds["new.go"][3] {
		t.Errorf("expected new.go L3 tracked, got: %v", ds["new.go"])
	}
}

func TestParseDiff_RenameHunkWithEditsCountsEdits(t *testing.T) {
	// similarity 80% — overall a rename, BUT the hunk shows 1 context +
	// 1 deletion + 1 addition (the edit). Because the whole block is
	// classified as a move at or above threshold, the `+` (the edit)
	// is excluded too. This documents the current model: per-BLOCK
	// classification, not per-line. A future refinement could try to
	// distinguish "moved" vs "edited" `+` lines, but git itself doesn't
	// give us that granularity without recomputing similarity.
	in := `diff --git a/old.go b/new.go
similarity index 80%
rename from old.go
rename to new.go
--- a/old.go
+++ b/new.go
@@ -1,2 +1,2 @@
 unchanged context
-removed original
+added replacement
`
	ds, err := parseDiffWithRenames(strings.NewReader(in), 50)
	if err != nil {
		t.Fatalf("parseDiffWithRenames: %v", err)
	}
	if got, ok := ds["new.go"]; ok && len(got) > 0 {
		t.Errorf("expected NO lines flagged when block-level similarity (80%%) "+
			"is at/above threshold (50%%); got: %v", got)
	}

	// Same diff, threshold raised to 90 — now the block IS treated as
	// new code, and the `+ added replacement` line at L2 must be flagged.
	ds2, err := parseDiffWithRenames(strings.NewReader(in), 90)
	if err != nil {
		t.Fatalf("parseDiffWithRenames (strict): %v", err)
	}
	if !ds2["new.go"][2] {
		t.Errorf("with threshold 90, expected new.go L2 (the edit) tracked, got: %v", ds2["new.go"])
	}
}

func TestRenameDetection_GodfileServeHandlersWIP(t *testing.T) {
	// End-to-end regression on the WIP godfile-serve-handlers worktree.
	//
	// What the godfile diff actually looks like with `git diff -B5% -M5%
	// -C5%`:
	//
	//   - 7 per-tool files were copy-detected by git (similarities
	//     5-16%) — their `+` lines were already eliminated by git's
	//     own preprocessing; the blocks contain only `-` deletions.
	//   - 3 per-tool files (find_callers, get_sheaf_status,
	//     semantic_search) are net-new (`new file mode`, `--- /dev/null`)
	//     — git could not associate them with serve_handlers.go at the
	//     5% threshold, so they show as pure additions with no
	//     similarity header.
	//   - serve_handlers.go has a normal in-place diff (most of its
	//     content was removed in this WIP commit).
	//
	// The bead's "185 → ~0" claim was the baseline-without-flags vs the
	// theoretical-perfect-detection. The reality after git's heuristics
	// is closer to ~76 lines flagged before this change. This test
	// pins the two properties this change actually delivers:
	//
	//   1. At threshold 50 (the default), the flagged-line count is
	//      strictly less than OR equal to the count at threshold 101+
	//      (effectively-disabled) — we never INCREASE false positives.
	//   2. When we synthesize a high-similarity rename hunk on top of
	//      the godfile diff, the count drops at threshold 50. This
	//      proves rename detection fires when given a real opportunity.
	//
	// The WIP branch lives at /private/tmp/mache-worktrees/godfile-serve-handlers
	// — if the path is missing (e.g. CI on a fresh checkout) we skip
	// rather than fail.
	const godfilePath = "/private/tmp/mache-worktrees/godfile-serve-handlers"
	if _, err := os.Stat(godfilePath); err != nil {
		t.Skipf("godfile-serve-handlers worktree not present at %s — skipping live regression test", godfilePath)
	}
	cmd := exec.Command("git",
		"-C", godfilePath,
		"diff", "-B5%", "-M5%", "-C5%", "origin/main..HEAD",
	)
	diffBytes, err := cmd.Output()
	if err != nil {
		t.Skipf("git diff failed for godfile worktree (%v); skipping", err)
	}
	if len(diffBytes) == 0 {
		t.Skipf("godfile diff is empty; nothing to regress against")
	}

	// (1) No-regression: default threshold ≤ strict threshold.
	dsDefault, err := parseDiffWithRenames(bytes.NewReader(diffBytes), 50)
	if err != nil {
		t.Fatalf("parseDiffWithRenames (default): %v", err)
	}
	dsStrict, err := parseDiffWithRenames(bytes.NewReader(diffBytes), 100)
	if err != nil {
		t.Fatalf("parseDiffWithRenames (strict): %v", err)
	}
	defaultTotal := 0
	for _, lines := range dsDefault {
		defaultTotal += len(lines)
	}
	strictTotal := 0
	for _, lines := range dsStrict {
		strictTotal += len(lines)
	}
	if defaultTotal > strictTotal {
		t.Errorf("rename detection regressed: default threshold flagged MORE "+
			"lines than strict. default=%d strict=%d files(default)=%v",
			defaultTotal, strictTotal, summarizeDiffSet(dsDefault))
	}
	t.Logf("godfile-serve-handlers WIP diff: default(50)=%d, strict(100)=%d",
		defaultTotal, strictTotal)

	// (2) Concatenate a synthesized high-similarity rename hunk to the
	// real diff. parseDiff should drop the synthesized `+` lines at
	// threshold 50 but NOT at threshold 100.
	const synth = `
diff --git a/_synth_old.go b/_synth_new.go
similarity index 95%
rename from _synth_old.go
rename to _synth_new.go
--- a/_synth_old.go
+++ b/_synth_new.go
@@ -1,3 +1,3 @@
+synth line 1
+synth line 2
+synth line 3
`
	combined := append([]byte{}, diffBytes...)
	combined = append(combined, synth...)

	dsCombinedRelaxed, err := parseDiffWithRenames(bytes.NewReader(combined), 50)
	if err != nil {
		t.Fatalf("parseDiffWithRenames (combined,relaxed): %v", err)
	}
	dsCombinedStrict, err := parseDiffWithRenames(bytes.NewReader(combined), 100)
	if err != nil {
		t.Fatalf("parseDiffWithRenames (combined,strict): %v", err)
	}
	if got := len(dsCombinedRelaxed["_synth_new.go"]); got != 0 {
		t.Errorf("synth rename at 95%% similarity should be EXCLUDED at "+
			"threshold 50, got %d flagged: %v",
			got, dsCombinedRelaxed["_synth_new.go"])
	}
	if got := len(dsCombinedStrict["_synth_new.go"]); got != 3 {
		t.Errorf("synth rename at 95%% similarity should be INCLUDED at "+
			"threshold 100, got %d flagged (want 3): %v",
			got, dsCombinedStrict["_synth_new.go"])
	}
}

// summarizeDiffSet produces a compact per-file count summary for test failures.
func summarizeDiffSet(ds diffSet) map[string]int {
	out := make(map[string]int, len(ds))
	for f, lines := range ds {
		out[f] = len(lines)
	}
	return out
}

func TestParseSimilarityPercent(t *testing.T) {
	// Direct unit test of the helper for completeness.
	cases := map[string]struct {
		want int
		ok   bool
	}{
		"similarity index 100%": {100, true},
		"similarity index 50%":  {50, true},
		"similarity index 0%":   {0, true},
		"similarity index 7%":   {7, true},
		"similarity index abc%": {0, false},
		"similarity index ":     {0, false},
	}
	for in, exp := range cases {
		got, ok := parseSimilarityPercent(in)
		if ok != exp.ok || got != exp.want {
			t.Errorf("parseSimilarityPercent(%q) = (%d,%v), want (%d,%v)",
				in, got, ok, exp.want, exp.ok)
		}
	}
}

func TestRun_InvalidRenameThreshold(t *testing.T) {
	covPath := writeTemp(t, "cover.out", "mode: set\n")
	diffPath := writeTemp(t, "diff.patch", "")
	for _, bad := range []string{"-1", "101", "9999"} {
		var stdout, stderr bytes.Buffer
		code := run("coverage-gate",
			[]string{"-rename-threshold", bad, covPath, diffPath},
			&stdout, &stderr,
		)
		if code != 2 {
			t.Errorf("threshold %q: expected exit 2, got %d (stderr=%q)", bad, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "rename-threshold") {
			t.Errorf("threshold %q: expected stderr to mention rename-threshold, got %q",
				bad, stderr.String())
		}
	}
}

func TestRun_RenameThresholdPropagates(t *testing.T) {
	// End-to-end through run(): a 100%-similarity rename block with
	// zero coverage on the destination file must NOT trigger the gate
	// at threshold 50, but MUST trigger at threshold 101.
	cover := "mode: set\nnew.go:1.1,3.10 3 0\n"
	diff := `diff --git a/old.go b/new.go
similarity index 100%
rename from old.go
rename to new.go
--- a/old.go
+++ b/new.go
@@ -1,3 +1,3 @@
+moved line 1
+moved line 2
+moved line 3
`
	covPath := writeTemp(t, "cover.out", cover)
	diffPath := writeTemp(t, "diff.patch", diff)

	// Default threshold (50) — 100%% similarity excludes the block.
	var stdout, stderr bytes.Buffer
	code := run("coverage-gate", []string{covPath, diffPath}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit 0 with default threshold on 100%% rename, got %d (stdout=%q stderr=%q)",
			code, stdout.String(), stderr.String())
	}

	// Strict threshold (101) — no rename ever matches, gate trips.
	stdout.Reset()
	stderr.Reset()
	code = run("coverage-gate",
		[]string{"-rename-threshold", "101"},
		&stdout, &stderr,
	)
	// 101 is out of range; expect exit 2 (validation error).
	if code != 2 {
		t.Errorf("expected exit 2 on out-of-range threshold 101, got %d (stderr=%q)", code, stderr.String())
	}

	// Threshold 100 (boundary inclusive) — still excluded.
	stdout.Reset()
	stderr.Reset()
	code = run("coverage-gate",
		[]string{"-rename-threshold", "100", covPath, diffPath},
		&stdout, &stderr,
	)
	if code != 0 {
		t.Errorf("expected exit 0 with threshold=100 on 100%% rename, got %d (stdout=%q)",
			code, stdout.String())
	}

	// Threshold 0 — every similarity-tagged block is excluded.
	stdout.Reset()
	stderr.Reset()
	code = run("coverage-gate",
		[]string{"-rename-threshold", "0", covPath, diffPath},
		&stdout, &stderr,
	)
	if code != 0 {
		t.Errorf("expected exit 0 with threshold=0, got %d (stdout=%q)",
			code, stdout.String())
	}
}
