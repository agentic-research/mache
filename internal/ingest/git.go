package ingest

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/agentic-research/mache/internal/gitutil"
)

// GetGitHints returns the default inference hints for Git repositories.
func GetGitHints() map[string]string {
	return map[string]string{
		"sha":     "id",
		"tree":    "reference",
		"parents": "reference",
		"date":    "temporal",
	}
}

// LoadGitCommits loads the first-parent history of revs (HEAD when none are
// given) from the repository at repoPath, newest first.
//
// The walk is `git log --first-parent <revs>` under gitutil.IsolatedGitCommand,
// so the records are a function of the repository's objects and the named
// revisions and nothing else. It used to be `git log --all`, which walks every
// ref — local branches, remote-tracking refs, the stash, notes — so the same
// repository projected on two machines, or on one machine before and after a
// `git fetch` or `git stash`, produced different records (mache-82ded3).
// First-parent keeps the walk to the line of history the revision names:
// the commits of a merged branch are reachable from the merge but are not
// what `HEAD` is the history OF, and including them made the record set
// depend on how each branch happened to be integrated.
//
// revs are passed to git as given: a single revision, several, or a range
// such as `v1.0..HEAD`.
func LoadGitCommits(repoPath string, revs ...string) ([]any, error) {
	if len(revs) == 0 {
		revs = []string{"HEAD"}
	}

	// Use a custom separator that is unlikely to appear in commit messages.
	const sep = "|||MACHE_SEP|||"

	// Format: SHA, Tree, Parents, Author, Date, Body
	// %H: Commit hash
	// %T: Tree hash
	// %P: Parent hashes
	// %an: Author name
	// %aI: Author date (ISO 8601)
	// %B: Raw body (subject + body)
	format := "%H%n%T%n%P%n%an%n%aI%n%B" + sep

	args := append([]string{"log", "--first-parent", "--date=iso", "--pretty=format:" + format}, revs...)
	cmd := gitutil.IsolatedGitCommand(args...)
	cmd.Dir = repoPath

	// Increase buffer size for large logs
	var out bytes.Buffer
	cmd.Stdout = &out

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git log failed: %w", err)
	}

	var commits []any
	scanner := bufio.NewScanner(&out)
	scanner.Split(splitOn([]byte(sep)))
	for scanner.Scan() {
		if commit, ok := parseCommitRecord(scanner.Text()); ok {
			commits = append(commits, commit)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanner error: %w", err)
	}

	return commits, nil
}

// splitOn is a bufio.SplitFunc yielding the text between occurrences of sep,
// and whatever follows the last one.
func splitOn(sep []byte) bufio.SplitFunc {
	return func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		if atEOF && len(data) == 0 {
			return 0, nil, nil
		}
		if i := bytes.Index(data, sep); i >= 0 {
			return i + len(sep), data[0:i], nil
		}
		if atEOF {
			return len(data), data, nil
		}
		return 0, nil, nil
	}
}

// parseCommitRecord turns one `git log` record in LoadGitCommits' format into
// a commit map. ok is false for an empty or malformed record, which is skipped.
func parseCommitRecord(text string) (map[string]any, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, false
	}

	lines := strings.SplitN(text, "\n", 6)
	if len(lines) < 6 {
		if len(lines) < 5 {
			return nil, false // malformed
		}
		lines = append(lines, "") // empty body
	}

	commit := map[string]any{
		"sha":     lines[0],
		"tree":    lines[1],
		"parents": strings.Fields(lines[2]), // Split by space
		"author":  lines[3],
		"date":    lines[4],
		"message": strings.TrimSpace(lines[5]),
	}

	// Parse date and enrich
	if t, err := time.Parse(time.RFC3339, lines[4]); err == nil {
		// Inject derived fields for robust sharding
		commit["date_year"] = fmt.Sprintf("%04d", t.Year())
		commit["date_month"] = fmt.Sprintf("%02d", t.Month())
		commit["date_day"] = fmt.Sprintf("%02d", t.Day())
	}
	return commit, true
}
