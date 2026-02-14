package ingest

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"strings"
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

// LoadGitCommits loads all commits from a repository using git log.
func LoadGitCommits(repoPath string) ([]any, error) {
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

	cmd := exec.Command("git", "log", "--all", "--date=iso", fmt.Sprintf("--pretty=format:%s", format))
	cmd.Dir = repoPath

	// Increase buffer size for large logs
	var out bytes.Buffer
	cmd.Stdout = &out

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git log failed: %w", err)
	}

	var commits []any
	scanner := bufio.NewScanner(&out)

	// Split by our custom separator
	scanner.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		if atEOF && len(data) == 0 {
			return 0, nil, nil
		}
		if i := bytes.Index(data, []byte(sep)); i >= 0 {
			return i + len(sep), data[0:i], nil
		}
		if atEOF {
			return len(data), data, nil
		}
		return 0, nil, nil
	})

	for scanner.Scan() {
		text := scanner.Text()
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}

		lines := strings.SplitN(text, "\n", 6)
		if len(lines) < 6 {
			// Maybe empty body?
			if len(lines) >= 5 {
				// Pad with empty body
				lines = append(lines, "")
			} else {
				continue // Skip malformed
			}
		}

		commit := map[string]any{
			"sha":     lines[0],
			"tree":    lines[1],
			"parents": strings.Fields(lines[2]), // Split by space
			"author":  lines[3],
			"date":    lines[4],
			"message": strings.TrimSpace(lines[5]),
		}

		commits = append(commits, commit)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanner error: %w", err)
	}

	return commits, nil
}
