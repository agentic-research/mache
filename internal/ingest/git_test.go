package ingest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/gitutil"
)

// historyRepo is a throwaway repository under a hermetic HOME.
//
// Every Git config location is pointed into the test: HOME and XDG_CONFIG_HOME
// for the paths Git derives, GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM for the
// files it reads instead when set — a developer's shell may already set those,
// and a fixture that read the developer's config would not be a fixture. The
// files start absent; a test that wants a poisoned config writes one.
// GIT_CONFIG_NOSYSTEM is cleared so a shell that skips system config does not
// make the system-config case pass for the wrong reason.
type historyRepo struct {
	t    *testing.T
	dir  string
	home string
}

func newHistoryRepo(t *testing.T) *historyRepo {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(home, "system.gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "")

	r := &historyRepo{t: t, dir: t.TempDir(), home: home}
	r.git("init", "-q", "-b", "main")
	r.git("config", "user.name", "Tester")
	r.git("config", "user.email", "test@example.com")
	return r
}

// git runs a hermetic (but NOT isolated) git command in the repository and
// returns its trimmed stdout. Hermetic only, deliberately: this is the
// fixture's view of the repository, the one that sees the poison a test
// plants, so a test can prove the poison bites before proving LoadGitCommits
// ignores it.
func (r *historyRepo) git(args ...string) string {
	r.t.Helper()
	cmd := gitutil.HermeticGitCommand(args...)
	cmd.Dir = r.dir
	out, err := cmd.Output()
	require.NoError(r.t, err, "git %s", strings.Join(args, " "))
	return strings.TrimSpace(string(out))
}

func (r *historyRepo) commit(message string) string {
	r.t.Helper()
	r.git("commit", "-q", "--allow-empty", "-m", message)
	return r.git("rev-parse", "HEAD")
}

// shas is the sha of each record, in the order LoadGitCommits returned them.
func shas(commits []any) []string {
	out := make([]string, len(commits))
	for i, c := range commits {
		out[i] = c.(map[string]any)["sha"].(string)
	}
	return out
}

func TestLoadGitCommits(t *testing.T) {
	r := newHistoryRepo(t)
	r.commit("Initial commit")
	r.commit(`Second commit

With body`)

	commits, err := LoadGitCommits(r.dir)
	require.NoError(t, err)
	assert.Len(t, commits, 2)

	// Newest first.
	c0 := commits[0].(map[string]any)
	assert.Equal(t, "Tester", c0["author"])
	assert.Contains(t, c0["message"], "Second commit")
	assert.Contains(t, c0["message"], "With body")
	assert.NotEmpty(t, c0["sha"])
	assert.NotEmpty(t, c0["date"])

	c1 := commits[1].(map[string]any)
	assert.Contains(t, c1["message"], "Initial commit")
}

// TestLoadGitCommits_FirstParentOfHEADOnly pins the walk to HEAD's first-parent
// history. `git log --all` walked every ref, so an extra local branch, a
// stash, or a fetched remote-tracking ref changed what the same repository
// projected to (mache-82ded3). A merged side branch is reachable from HEAD but
// is not on its first-parent line, so it is out too; naming the branch, or a
// range, is how a caller asks for it.
func TestLoadGitCommits_FirstParentOfHEADOnly(t *testing.T) {
	r := newHistoryRepo(t)
	a := r.commit("A")
	tracked := filepath.Join(r.dir, "f.txt")
	require.NoError(t, os.WriteFile(tracked, []byte("1\n"), 0o644))
	r.git("add", "f.txt")
	b := r.commit("B")

	r.git("checkout", "-q", "-b", "side", a)
	s := r.commit("S")
	r.git("checkout", "-q", "main")
	r.git("merge", "-q", "--no-ff", "-m", "M", "side")
	m := r.git("rev-parse", "HEAD")

	r.git("checkout", "-q", "-b", "wip")
	w := r.commit("W")
	r.git("checkout", "-q", "main")

	require.NoError(t, os.WriteFile(tracked, []byte("2\n"), 0o644))
	r.git("stash", "push", "-q")
	stash := r.git("rev-parse", "refs/stash")

	// The machine state is really there for a ref-walking log to find.
	all := r.git("log", "--all", "--pretty=format:%H")
	for _, sha := range []string{s, w, stash} {
		require.Contains(t, all, sha)
	}

	got, err := LoadGitCommits(r.dir)
	require.NoError(t, err)
	assert.Equal(t, []string{m, b, a}, shas(got), "HEAD's first-parent line, newest first")

	// An explicit revision or range walks that instead.
	got, err = LoadGitCommits(r.dir, "side")
	require.NoError(t, err)
	assert.Equal(t, []string{s, a}, shas(got))

	got, err = LoadGitCommits(r.dir, a+"..main")
	require.NoError(t, err)
	assert.Equal(t, []string{m, b}, shas(got))
}

// TestLoadGitCommits_IgnoresUserAndSystemConfig plants a global, then a
// system, `i18n.logOutputEncoding` that re-encodes every message `git log`
// prints, shows that a merely hermetic log is re-encoded by it, and requires
// LoadGitCommits to return the same records as before the poison.
func TestLoadGitCommits_IgnoresUserAndSystemConfig(t *testing.T) {
	r := newHistoryRepo(t)
	r.commit("Initial commit")
	r.commit("Second commit")

	want, err := LoadGitCommits(r.dir)
	require.NoError(t, err)

	const poison = "[i18n]\n\tlogOutputEncoding = UTF-16\n"
	for _, file := range []string{".gitconfig", "system.gitconfig"} {
		t.Run(file, func(t *testing.T) {
			path := filepath.Join(r.home, file)
			require.NoError(t, os.WriteFile(path, []byte(poison), 0o644))
			defer func() { require.NoError(t, os.Remove(path)) }()

			// The poison bites: UTF-16 has a NUL between every ASCII byte.
			require.NotContains(t, r.git("log", "-1", "--pretty=format:%B"), "Second commit",
				"%s was not read by git at all, so this test proves nothing", file)

			got, err := LoadGitCommits(r.dir)
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

// TestLoadGitCommits_IgnoresReplaceRefs: a `refs/replace/` entry makes git
// present another object's content under the original SHA, so a merely
// hermetic log shows the replacement's message. The records are the
// repository's commits as written.
func TestLoadGitCommits_IgnoresReplaceRefs(t *testing.T) {
	r := newHistoryRepo(t)
	a := r.commit("Initial commit")
	b := r.commit("Second commit")

	want, err := LoadGitCommits(r.dir)
	require.NoError(t, err)

	replacement := r.git("commit-tree", b+"^{tree}", "-p", a, "-m", "replaced message")
	r.git("replace", b, replacement)
	require.Contains(t, r.git("log", "-1", "--pretty=format:%B"), "replaced message",
		"the replace ref was not honoured by git at all, so this test proves nothing")

	got, err := LoadGitCommits(r.dir)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// TestGetGitHints pins the inference hint contract that ties git
// commit fields to FCA scaling kinds. The map is small and static
// today, but its keys are part of the schema-inference public
// surface — losing or renaming an entry here silently changes
// what attributes BuildContext treats as identifier / reference /
// temporal. The test documents the wire shape and catches
// accidental edits.
func TestGetGitHints(t *testing.T) {
	hints := GetGitHints()
	want := map[string]string{
		"sha":     "id",
		"tree":    "reference",
		"parents": "reference",
		"date":    "temporal",
	}
	assert.Equal(t, want, hints,
		"GetGitHints map is part of the schema-inference public surface; updates require a deliberate change to the inferrer contract")
}
