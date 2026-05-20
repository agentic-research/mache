// fixtures-rebaseline updates a single perf-gate baseline in
// testdata/snapshots/baselines.toml with an explicit audit trail.
//
// Per ADR-0019 D.6, baselines are FIXED ANCHORS — they do not
// auto-update on merge. This tool is the only sanctioned path to
// bump wall_ms for a (key, fixture) pair. The audit trail
// (last_intentional_bump + refreshed anchored_at) makes regressions
// visible in `git blame` rather than disappearing into a ratchet.
//
// Usage:
//
//	go run ./tools/fixtures-rebaseline \
//	  --key find_smells:dead_code \
//	  --fixture mache-self \
//	  --wall-ms 200 \
//	  --justification "PR #XXX intentional regression for feature Y"
//
// The tool:
//  1. Loads baselines.toml
//  2. Confirms the (key, fixture) pair exists (refuses to create new
//     anchors silently — that's the helper's t.Fatalf message guiding
//     the developer to run this tool with --justification "initial anchor")
//  3. Rewrites the entry surgically in place — preserving file
//     comments and ordering rather than round-tripping via the TOML
//     encoder (which normalizes whitespace + drops comments)
//  4. Updates wall_ms, last_intentional_bump = "<today> <justification>",
//     anchored_at = current git HEAD short SHA
//  5. Prints the suggested commit-message line so the developer can
//     copy-paste it
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/agentic-research/mache/internal/testfixtures"
)

func main() {
	key := flag.String("key", "", "rule:variant key, e.g. find_smells:dead_code (required)")
	fixture := flag.String("fixture", "", "fixture id from manifest.toml, e.g. mache-self (required)")
	wallMs := flag.Int("wall-ms", 0, "new baseline wall-time in milliseconds (required)")
	justification := flag.String("justification", "", "audit-trail justification for the bump (required)")
	flag.Parse()

	if *key == "" || *fixture == "" || *wallMs <= 0 || *justification == "" {
		fmt.Fprintln(os.Stderr,
			"usage: fixtures-rebaseline --key <key> --fixture <id> --wall-ms <n> --justification <text>")
		flag.PrintDefaults()
		os.Exit(2)
	}

	if err := run(*key, *fixture, *wallMs, *justification); err != nil {
		fmt.Fprintf(os.Stderr, "fixtures-rebaseline: %v\n", err)
		os.Exit(1)
	}
}

func run(key, fixture string, wallMs int, justification string) error {
	repoRoot, err := findMacheRoot()
	if err != nil {
		return err
	}
	path := filepath.Join(repoRoot, "testdata", "snapshots", "baselines.toml")

	// Parse to validate the entry exists + capture the old wall_ms
	// for the commit-message hint. We REPARSE rather than trusting
	// the surgical rewrite alone so a malformed baselines.toml fails
	// loudly before we mutate the file.
	parsed, err := testfixtures.ParseBaselinesFile(path)
	if err != nil {
		return fmt.Errorf("validate baselines.toml: %w", err)
	}
	old, ok := testfixtures.LookupBaseline(parsed, key, fixture)
	if !ok {
		return fmt.Errorf(
			"no baseline for (%q, %q) — to add a new anchor, manually append the entry "+
				"to baselines.toml; this tool only updates existing anchors so it can't be "+
				"used to silently create new perf gates",
			key, fixture)
	}

	sha := detectGitSHA(repoRoot) // current HEAD; the SHA the developer is committing AT
	if sha == "" {
		fmt.Fprintln(os.Stderr,
			"warning: could not detect git HEAD SHA; anchored_at will be left unchanged")
	}
	today := time.Now().Format("2006-01-02")
	bumpLine := fmt.Sprintf("%s: %s", today, justification)

	rawIn, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read baselines: %w", err)
	}
	rawOut, err := rewriteEntry(string(rawIn), key, fixture, wallMs, sha, today, bumpLine)
	if err != nil {
		return fmt.Errorf("rewrite entry: %w", err)
	}

	// Atomic write via tempfile + rename so a partial write doesn't
	// corrupt the file on disk (baselines.toml is single-source-of-
	// truth; losing it because of a crashed tool would be bad).
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(rawOut), 0o644); err != nil {
		return fmt.Errorf("write tempfile: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename tempfile: %w", err)
	}

	// Re-parse to confirm the rewrite didn't break the file.
	if _, err := testfixtures.ParseBaselinesFile(path); err != nil {
		return fmt.Errorf("post-write validation failed (manual fixup needed): %w", err)
	}

	fmt.Println("[fixtures-rebaseline] updated baseline")
	fmt.Printf("  key            : %s\n", key)
	fmt.Printf("  fixture        : %s\n", fixture)
	fmt.Printf("  wall_ms        : %d -> %d\n", old.WallMs, wallMs)
	if sha != "" {
		fmt.Printf("  anchored_at    : %s -> %s\n", old.AnchoredAt, sha)
	}
	fmt.Printf("  anchored_at_date: %s -> %s\n", old.AnchoredAtDate, today)
	fmt.Printf("  bump line      : %s\n", bumpLine)
	fmt.Println()
	fmt.Printf("commit this change as:\n")
	fmt.Printf("  [mache-eb9b30] chore(baselines): %s/%s %dms -> %dms (%s)\n",
		key, fixture, old.WallMs, wallMs, justification)
	return nil
}

// rewriteEntry surgically rewrites the (key, fixture) entry in the
// raw TOML text. We avoid round-tripping via the TOML encoder because
// that normalizes whitespace and drops comments — and baselines.toml
// has hand-authored comments explaining the policy.
//
// Algorithm:
//  1. Find the `[["<key>"]]` table-array header.
//  2. Read forward to the next blank-or-header boundary — that's the
//     entry block.
//  3. If the block's `fixture = "<id>"` matches, rewrite the three
//     value lines (wall_ms, anchored_at, anchored_at_date,
//     last_intentional_bump) using regex.
//  4. If multiple entries share the same key, walk through them and
//     pick the one whose fixture matches.
//  5. If no match, return an error (the validation step above should
//     have caught this — defense-in-depth).
//
// This is line-oriented rather than TOML-aware. It works because
// baselines.toml's schema is intentionally simple (no nested tables,
// no array-of-array values).
func rewriteEntry(
	in, key, fixture string,
	wallMs int,
	sha, today, bumpLine string,
) (string, error) {
	lines := strings.Split(in, "\n")
	header := fmt.Sprintf("[[%q]]", key) // produces [["find_smells:dead_code"]]

	// Locate the start of each block matching this key. We scan
	// once, building (start, end) ranges. End is exclusive: the line
	// before the next [[...]] header OR EOF.
	blocks := []struct{ start, end int }{}
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if t == header {
			end := len(lines)
			for j := i + 1; j < len(lines); j++ {
				tj := strings.TrimSpace(lines[j])
				if strings.HasPrefix(tj, "[[") {
					end = j
					break
				}
			}
			blocks = append(blocks, struct{ start, end int }{i, end})
		}
	}
	if len(blocks) == 0 {
		return "", fmt.Errorf("no [[%s]] header found in baselines.toml", key)
	}

	// Pick the block whose fixture line matches.
	fixtureLine := regexp.MustCompile(`(?m)^\s*fixture\s*=\s*"([^"]+)"\s*$`)
	chosen := -1
	for idx, b := range blocks {
		blockText := strings.Join(lines[b.start:b.end], "\n")
		m := fixtureLine.FindStringSubmatch(blockText)
		if m != nil && m[1] == fixture {
			chosen = idx
			break
		}
	}
	if chosen == -1 {
		return "", fmt.Errorf("block for %q exists but no entry has fixture = %q", key, fixture)
	}

	// Edit the chosen block in place.
	b := blocks[chosen]
	for i := b.start; i < b.end; i++ {
		line := lines[i]
		switch {
		case wallMsRe.MatchString(line):
			lines[i] = wallMsRe.ReplaceAllString(line,
				fmt.Sprintf("${prefix}%d${suffix}", wallMs))
		case sha != "" && anchoredAtRe.MatchString(line):
			lines[i] = anchoredAtRe.ReplaceAllString(line,
				fmt.Sprintf(`${prefix}"%s"${suffix}`, sha))
		case anchoredAtDateRe.MatchString(line):
			lines[i] = anchoredAtDateRe.ReplaceAllString(line,
				fmt.Sprintf(`${prefix}"%s"${suffix}`, today))
		case lastBumpRe.MatchString(line):
			lines[i] = lastBumpRe.ReplaceAllString(line,
				fmt.Sprintf(`${prefix}"%s"${suffix}`, escapeTOML(bumpLine)))
		}
	}
	return strings.Join(lines, "\n"), nil
}

// Field-edit regexes. Each captures a `prefix` (leading whitespace +
// key + `=` + optional space) and `suffix` (trailing comment etc.)
// so the rewrite preserves indentation + inline comments.
var (
	wallMsRe         = regexp.MustCompile(`(?P<prefix>^\s*wall_ms\s*=\s*)\d+(?P<suffix>.*)$`)
	anchoredAtRe     = regexp.MustCompile(`(?P<prefix>^\s*anchored_at\s*=\s*)"[^"]*"(?P<suffix>.*)$`)
	anchoredAtDateRe = regexp.MustCompile(`(?P<prefix>^\s*anchored_at_date\s*=\s*)"[^"]*"(?P<suffix>.*)$`)
	lastBumpRe       = regexp.MustCompile(`(?P<prefix>^\s*last_intentional_bump\s*=\s*)"[^"]*"(?P<suffix>.*)$`)
)

// escapeTOML escapes characters that would break a TOML basic string.
// Justification text is user-supplied; we conservatively escape \" and
// \\ which are the only metacharacters in a non-multiline basic string.
func escapeTOML(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// findMacheRoot walks up from this file's location until it finds a
// go.mod. The tool is built and run via `go run`, so runtime.Caller(0)
// points at this main.go regardless of cwd.
func findMacheRoot() (string, error) {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller(0) failed")
	}
	dir := filepath.Dir(here)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("walked to filesystem root without finding go.mod (started at %s)", here)
		}
		dir = parent
	}
}

// detectGitSHA returns the short HEAD SHA of source if it's a git
// checkout. Returns "" if git is unavailable or source isn't a repo
// (in which case the tool prints a warning and leaves anchored_at
// untouched).
func detectGitSHA(source string) string {
	cmd := exec.Command("git", "-C", source, "rev-parse", "--short", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
