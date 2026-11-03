package lint

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/gitutil"
	"github.com/agentic-research/mache/internal/testutil"
)

// conflictMarkers are the line prefixes git writes into a conflicted file. The
// base marker matters as much as the other two: with merge.conflictstyle=diff3
// a resolution that handles only <<< and >>> leaves the base section behind,
// which is how a "||||||| parent of ..." line reached CHANGELOG.md on main.
// A bare "=======" is not checked: it is also a Markdown heading underline.
var conflictMarkers = []string{"<<<<<<< ", "||||||| ", ">>>>>>> "}

// No tracked text file carries a line git would write for a conflict.
func TestNoTrackedFileHasConflictMarkers(t *testing.T) {
	root := testutil.MacheRepoRoot(t)
	cmd := gitutil.HermeticGitCommand("ls-files", "-z")
	cmd.Dir = root
	out, err := cmd.Output()
	require.NoError(t, err)

	var found []string
	for _, rel := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil || bytes.IndexByte(data, 0) >= 0 {
			continue // removed in the worktree, or binary
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
		for n := 1; sc.Scan(); n++ {
			for _, m := range conflictMarkers {
				if strings.HasPrefix(sc.Text(), m) {
					found = append(found, rel+":"+strconv.Itoa(n)+": "+sc.Text())
				}
			}
		}
	}
	assert.Empty(t, found, "unresolved conflict markers in tracked files")
}
