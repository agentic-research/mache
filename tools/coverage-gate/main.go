// coverage-gate is a diff-aware NEW-CODE coverage gate.
//
// It reads a Go cover profile and a unified diff, then reports any
// newly-added or modified prod (non-test) Go source lines that have
// zero coverage. Exits 0 if all new prod lines are covered; exits 1
// with a per-file report otherwise.
//
// Usage:
//
//	coverage-gate <cover.out> <diff.patch>
//
// Inputs:
//   - cover.out: produced by `go test -coverprofile=cover.out ./...`
//   - diff.patch: unified diff, e.g. `git diff base..HEAD > diff.patch`
//     or `gh pr diff <num> > diff.patch`
//
// Behavior:
//   - Parses cover profile into file → []{startLine, endLine, hits} ranges.
//   - Parses diff for added lines in .go files (NOT _test.go, NOT testdata).
//   - Skips blank lines and comment-only lines (// or /*-prefixed after trim).
//   - For each added prod line that intersects a cover range with hits==0,
//     records it as uncovered.
//   - Lines not in any cover range are not statements (decl, blank, comment)
//     and are silently skipped.
//   - A `// coverage:ignore` suffix on an added line suppresses the warning
//     for that single line.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

// coverRange is one entry from a Go cover profile.
type coverRange struct {
	startLine int
	endLine   int
	hits      int
}

// profile is the parsed cover profile: file path → ranges.
type profile map[string][]coverRange

// diffSet is the parsed diff: file path → set of new prod line numbers.
type diffSet map[string]map[int]bool

// uncovered describes a contiguous block of uncovered new prod lines in a file.
type uncoveredBlock struct {
	startLine int
	endLine   int
}

// main is a thin wrapper around run() and is never invoked under
// `go test` (which compiles a test binary with a different entry
// point), so the body is exempt from the coverage gate. All logic
// lives in run(), which has direct in-process tests.
func main() { // coverage:ignore
	os.Exit(run(os.Args[0], os.Args[1:], os.Stdout, os.Stderr)) // coverage:ignore
} // coverage:ignore

// run executes the coverage-gate logic and returns the process exit code.
// Extracted from main() for in-process testing — main() is now a thin
// wrapper around os.Exit(run(...)). Observable behavior is identical to
// the previous main(): same argv parsing, same exit codes (0 clean / 1
// uncovered / 2 usage-or-io-error), same stdout/stderr formats.
//
// progName is the program name used in the usage message (os.Args[0]
// when called from main). args is the argument list AFTER the program
// name (i.e. os.Args[1:]). stdout receives the report; stderr receives
// usage and error messages.
func run(progName string, args []string, stdout, stderr io.Writer) int {
	if len(args) != 2 {
		_, _ = fmt.Fprintf(stderr, "usage: %s <cover.out> <diff.patch>\n", progName)
		return 2
	}
	coverPath := args[0]
	diffPath := args[1]

	covF, err := os.Open(coverPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error opening cover profile: %v\n", err)
		return 2
	}
	defer func() { _ = covF.Close() }()

	prof, err := parseProfile(covF)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error parsing cover profile: %v\n", err)
		return 2
	}
	prof = normalizeProfilePaths(prof, modulePathFromGoMod())

	diffF, err := os.Open(diffPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error opening diff: %v\n", err)
		return 2
	}
	defer func() { _ = diffF.Close() }()

	ds, err := parseDiff(diffF)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error parsing diff: %v\n", err)
		return 2
	}

	report := intersect(prof, ds)
	if len(report) == 0 {
		return 0
	}

	_, _ = fmt.Fprintln(stdout, "NEW PROD LINES NOT COVERED:")
	_, _ = fmt.Fprintln(stdout)
	total := 0
	files := make([]string, 0, len(report))
	for f := range report {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		lines := report[f]
		blocks := collapseBlocks(lines)
		_, _ = fmt.Fprintln(stdout, f)
		for _, b := range blocks {
			if b.startLine == b.endLine {
				_, _ = fmt.Fprintf(stdout, "  L%d\n", b.startLine)
			} else {
				_, _ = fmt.Fprintf(stdout, "  L%d-%d\n", b.startLine, b.endLine)
			}
			total += b.endLine - b.startLine + 1
		}
		_, _ = fmt.Fprintln(stdout)
	}
	_, _ = fmt.Fprintf(stdout, "%d new prod line(s) uncovered. Either add tests or annotate the line with `// coverage:ignore`.\n", total)
	return 1
}

// parseProfile reads a Go cover profile and returns file → ranges.
// Format per `go tool cover`:
//
//	mode: set|count|atomic
//	<file>:<sLine>.<sCol>,<eLine>.<eCol> <numStmts> <hitCount>
//
// File paths in the profile are module-qualified (e.g.
// "github.com/foo/bar/internal/baz.go"). The returned map preserves them
// as-is; normalizeProfilePaths must be called separately to strip the
// module prefix for diff-relative comparison.
func parseProfile(r io.Reader) (profile, error) {
	prof := profile{}
	sc := bufio.NewScanner(r)
	// Cover profiles can have long lines; allow generous buffer.
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	first := true
	for sc.Scan() {
		line := sc.Text()
		if first {
			first = false
			if strings.HasPrefix(line, "mode:") {
				continue
			}
		}
		if line == "" {
			continue
		}
		// Split on ':' from the right — the file path itself may contain ':' on
		// odd platforms but is unlikely in Go modules; use last ':' to be safe.
		colon := strings.LastIndex(line, ":")
		if colon < 0 {
			return nil, fmt.Errorf("malformed profile line (no colon): %q", line)
		}
		file := line[:colon]
		rest := line[colon+1:]
		// rest = "<sLine>.<sCol>,<eLine>.<eCol> <numStmts> <hitCount>"
		parts := strings.Fields(rest)
		if len(parts) != 3 {
			return nil, fmt.Errorf("malformed profile line (need 3 fields after colon): %q", line)
		}
		span := parts[0]
		hitsStr := parts[2]
		comma := strings.Index(span, ",")
		if comma < 0 {
			return nil, fmt.Errorf("malformed span (no comma): %q", span)
		}
		sLine, err := parsePosLine(span[:comma])
		if err != nil {
			return nil, fmt.Errorf("bad start in %q: %w", line, err)
		}
		eLine, err := parsePosLine(span[comma+1:])
		if err != nil {
			return nil, fmt.Errorf("bad end in %q: %w", line, err)
		}
		hits, err := strconv.Atoi(hitsStr)
		if err != nil {
			return nil, fmt.Errorf("bad hits in %q: %w", line, err)
		}
		prof[file] = append(prof[file], coverRange{startLine: sLine, endLine: eLine, hits: hits})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return prof, nil
}

// normalizeProfilePaths strips the module prefix from each file key so paths
// match the repo-relative form used in unified diffs. If modulePath is empty
// the profile is returned unchanged.
func normalizeProfilePaths(prof profile, modulePath string) profile {
	if modulePath == "" {
		return prof
	}
	prefix := modulePath
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	out := profile{}
	for file, ranges := range prof {
		out[strings.TrimPrefix(file, prefix)] = ranges
	}
	return out
}

// modulePathFromGoMod returns the module path declared in ./go.mod or the
// nearest ancestor go.mod. Empty string if none is found.
func modulePathFromGoMod() string {
	dir, err := os.Getwd()
	if err != nil { // coverage:ignore — os.Getwd only fails on detached/removed cwd, can't be reproduced from a Go test
		return "" // coverage:ignore
	} // coverage:ignore
	for {
		p := dir + string(os.PathSeparator) + "go.mod"
		if data, err := os.ReadFile(p); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "module ") {
					return strings.TrimSpace(strings.TrimPrefix(line, "module"))
				}
			}
		}
		parent := parentDir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// parentDir returns the parent of path, or path itself at filesystem root.
func parentDir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == os.PathSeparator {
			if i == 0 {
				return string(os.PathSeparator)
			}
			return path[:i]
		}
	}
	return path
}

// parsePosLine extracts the line number from a "line.col" token.
func parsePosLine(tok string) (int, error) {
	dot := strings.Index(tok, ".")
	if dot < 0 {
		return 0, fmt.Errorf("missing dot in %q", tok)
	}
	return strconv.Atoi(tok[:dot])
}

// parseDiff walks a unified diff and returns file → set of NEW prod line numbers.
//
// "New prod line" means: a `+`-prefixed line in the diff that is in a .go file,
// not in a _test.go file, not in /testdata/, not blank, and not a pure
// comment line. Lines suffixed `// coverage:ignore` are excluded.
func parseDiff(r io.Reader) (diffSet, error) {
	out := diffSet{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)

	var curFile string
	var inHunk bool
	var newLine int
	skipFile := true // until we know we're in a tracked file

	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "diff --git "):
			curFile = ""
			inHunk = false
			skipFile = true
		case strings.HasPrefix(line, "--- "):
			// nothing; we trust +++ for the new path
		case strings.HasPrefix(line, "+++ "):
			// "+++ b/path/to/file.go" or "+++ /dev/null"
			p := strings.TrimPrefix(line, "+++ ")
			p = strings.TrimSpace(p)
			if p == "/dev/null" {
				skipFile = true
				curFile = ""
				continue
			}
			p = strings.TrimPrefix(p, "b/")
			curFile = p
			skipFile = !isTrackedProdFile(p)
			inHunk = false
		case strings.HasPrefix(line, "@@"):
			if skipFile {
				inHunk = false
				continue
			}
			// "@@ -oldstart,oldlen +newstart,newlen @@ ..."
			ns, err := parseHunkNewStart(line)
			if err != nil {
				return nil, fmt.Errorf("bad hunk header %q: %w", line, err)
			}
			newLine = ns
			inHunk = true
		default:
			if !inHunk || skipFile {
				continue
			}
			if len(line) == 0 {
				// blank in patch — counts as a context line in some diff tools.
				// Conservative: advance newLine.
				newLine++
				continue
			}
			switch line[0] {
			case '+':
				// Added line. Strip leading '+' to get content.
				content := line[1:]
				if isCountableProdLine(content) {
					if out[curFile] == nil {
						out[curFile] = map[int]bool{}
					}
					out[curFile][newLine] = true
				}
				newLine++
			case '-':
				// Removed line; does not advance newLine.
			case '\\':
				// "\ No newline at end of file" — ignore.
			default:
				// Context line; advance newLine.
				newLine++
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// parseHunkNewStart pulls the +newstart from "@@ -a,b +c,d @@".
func parseHunkNewStart(s string) (int, error) {
	plus := strings.Index(s, "+")
	if plus < 0 {
		return 0, fmt.Errorf("no + in hunk header")
	}
	rest := s[plus+1:]
	// rest = "c,d @@ ..."
	end := strings.IndexAny(rest, " ,")
	if end < 0 {
		return strconv.Atoi(rest)
	}
	return strconv.Atoi(rest[:end])
}

// isTrackedProdFile returns true if path is a Go prod file we care about.
func isTrackedProdFile(p string) bool {
	if !strings.HasSuffix(p, ".go") {
		return false
	}
	if strings.HasSuffix(p, "_test.go") {
		return false
	}
	if strings.Contains(p, "/testdata/") {
		return false
	}
	return true
}

// isCountableProdLine decides if an added line is a candidate for coverage check.
// It excludes blanks, comment-only lines, and lines explicitly tagged
// `// coverage:ignore`.
func isCountableProdLine(content string) bool {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return false
	}
	if strings.HasPrefix(trimmed, "//") {
		return false
	}
	if strings.HasPrefix(trimmed, "/*") {
		return false
	}
	if strings.HasPrefix(trimmed, "*") {
		// continuation line inside a block comment — heuristic skip.
		return false
	}
	if strings.Contains(content, "// coverage:ignore") {
		return false
	}
	return true
}

// intersect walks the diff line set against the cover profile and returns the
// uncovered new prod lines: file → sorted slice of line numbers.
//
// A diff line counts as uncovered iff:
//   - the file appears in the profile, AND
//   - the line is inside a range with hits == 0, AND
//   - the line is NOT inside any range with hits > 0 (covers the case
//     where overlapping ranges disagree; the covered range wins).
//
// Lines not present in any range are skipped (not a statement).
func intersect(prof profile, ds diffSet) map[string][]int {
	out := map[string][]int{}
	for file, lines := range ds {
		ranges, ok := prof[file]
		if !ok {
			// File appears in diff but not in profile. Either it was excluded
			// from the test run or no statements exist. Skip — we can't make
			// a positive claim of uncovered without profile data.
			continue
		}
		for line := range lines {
			covered := false
			zeroHit := false
			for _, r := range ranges {
				if line < r.startLine || line > r.endLine {
					continue
				}
				if r.hits > 0 {
					covered = true
					break
				}
				zeroHit = true
			}
			if covered {
				continue
			}
			if zeroHit {
				out[file] = append(out[file], line)
			}
			// else: line is not inside any range → not a statement, skip.
		}
		sort.Ints(out[file])
	}
	return out
}

// collapseBlocks merges contiguous line numbers into blocks for compact output.
// Input must be sorted ascending.
func collapseBlocks(lines []int) []uncoveredBlock {
	if len(lines) == 0 {
		return nil
	}
	out := []uncoveredBlock{{startLine: lines[0], endLine: lines[0]}}
	for _, n := range lines[1:] {
		last := &out[len(out)-1]
		if n == last.endLine+1 {
			last.endLine = n
		} else {
			out = append(out, uncoveredBlock{startLine: n, endLine: n})
		}
	}
	return out
}
