package lint

// Failure handling for scripts/changelog-entry-check.sh (mache-55faa4).
//
// The script itself cannot be a Go test: it needs the history a change adds,
// and ci.yml's `test` job checks out at actions/checkout's default depth of 1
// — no tags, no base. A test run there could only skip or pass vacuously,
// which mache-ddf14b established is worse than no gate at all. So detection
// lives in the script and its BEHAVIOUR is pinned here, against throwaway
// repos built in t.TempDir(), the same arrangement flamegraphs_test.go uses
// for scripts/flamegraphs.sh.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/gitutil"
	"github.com/agentic-research/mache/internal/testutil"
)

const changelogCheckScript = "scripts/changelog-entry-check.sh"

// unreleasedOnly is a CHANGELOG whose Unreleased section names one bead and
// whose RELEASED section names another. The released id is the interesting
// one: an entry under a shipped heading says nothing about the change under
// review, so finding it there must not count.
const unreleasedOnly = `# Changelog

## [Unreleased]

### Fixed

- Something real (` + "`mache-aaaaaa`" + `).

## [v1.0.0] — 2026-01-01

### Fixed

- Shipped long ago (` + "`mache-bbbbbb`" + `).
`

type changelogRepo struct {
	t   *testing.T
	dir string
}

func newChangelogRepo(t *testing.T, changelog string) *changelogRepo {
	t.Helper()
	r := &changelogRepo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	r.git("config", "user.email", "test@example.com")
	r.git("config", "user.name", "Test")
	r.write("CHANGELOG.md", changelog)
	r.git("add", "CHANGELOG.md")
	r.commit("[mache-000000] chore: base\n\nChangelog: none")
	return r
}

func (r *changelogRepo) git(args ...string) {
	r.t.Helper()
	cmd := gitutil.HermeticGitCommand(append([]string{"-C", r.dir}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(r.t, err, "git %s: %s", strings.Join(args, " "), out)
}

func (r *changelogRepo) write(name, content string) {
	r.t.Helper()
	require.NoError(r.t, os.WriteFile(filepath.Join(r.dir, name), []byte(content), 0o644))
}

// commit makes an empty commit so a case is defined only by its message.
func (r *changelogRepo) commit(message string) {
	r.t.Helper()
	r.git("commit", "-q", "--allow-empty", "-m", message)
}

// head resolves the current commit. Passing the literal "HEAD" as the base
// would be resolved by the script AFTER the case's commit exists, making
// base..HEAD empty — so every should-fail case would pass having examined
// nothing. Three of these tests did exactly that on their first run.
func (r *changelogRepo) head() string {
	r.t.Helper()
	cmd := gitutil.HermeticGitCommand("-C", r.dir, "rev-parse", "HEAD")
	out, err := cmd.Output()
	require.NoError(r.t, err, "git rev-parse HEAD")
	return strings.TrimSpace(string(out))
}

// checkScript returns the script's path, having READ it first.
//
// The read is load-bearing beyond its two assertions. `go test` caches a
// result against the files the test opened, and exec.Command opens nothing
// in-process — the kernel does, in the child. Without a read here, editing
// the script leaves the cached PASS in place and the mutation that should
// have failed reports "ok (cached)". That happened on the first attempt at
// mutation-testing this, and it is the same "a gate that cannot fire" shape
// as mache-ddf14b.
func checkScript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(testutil.MacheRepoRoot(t), changelogCheckScript)
	body, err := os.ReadFile(path)
	require.NoError(t, err, "reading %s", changelogCheckScript)
	require.NotEmpty(t, body, "%s is empty", changelogCheckScript)
	require.True(t, strings.HasPrefix(string(body), "#!"),
		"%s must carry a shebang — it is executed directly by CI", changelogCheckScript)
	return path
}

// run invokes the script and returns its combined output and exit code.
func (r *changelogRepo) run(base string) (string, int) {
	r.t.Helper()
	script := checkScript(r.t)
	cmd := exec.Command("bash", script, base, r.dir)
	cmd.Env = gitutil.WithoutLocalEnv(os.Environ())
	out, err := cmd.CombinedOutput()
	code := 0
	var exitErr *exec.ExitError
	if err != nil {
		require.ErrorAs(r.t, err, &exitErr, "running %s: %s", changelogCheckScript, out)
		code = exitErr.ExitCode()
	}
	return string(out), code
}

// runCase is the shape every case shares: a repo holding `changelog`, a base
// recorded BEFORE the change, then `message` as the one commit under review.
func runCase(t *testing.T, changelog, message string) (string, int) {
	t.Helper()
	r := newChangelogRepo(t, changelog)
	base := r.head()
	r.commit(message)
	return r.run(base)
}

// TestChangelogEntryCheck_Verdicts is the homogeneous half: one commit, one
// expected exit code, and the phrases the output must carry. Kept as a table
// because these cases differ only in the message and the verdict — spelled out
// one function apiece they were six near-identical three-line bodies, which
// the duplicate_code gate was right to object to.
func TestChangelogEntryCheck_Verdicts(t *testing.T) {
	for _, tc := range []struct {
		name     string
		message  string
		wantCode int
		contains []string
	}{
		{
			name:     "an id under Unreleased passes",
			message:  "[mache-aaaaaa] fix(thing): a user-facing fix",
			wantCode: 0,
			contains: []string{"accounted for"},
		},
		{
			name:    "a missing entry fails, naming the bead, the commit and the way out",
			message: "[mache-cccccc] feat(thing): a whole new command",
			// Exit 1 is the violation code. Exit 2 means the check could not
			// run at all, which the cases below pin separately.
			wantCode: 1,
			contains: []string{"mache-cccccc", "a whole new command", "Changelog: none"},
		},
		{
			name:     "the trailer is the deliberate opt-out",
			message:  "[mache-cccccc] fix(ci): budget a job, no user-facing surface\n\nChangelog: none",
			wantCode: 0,
		},
		{
			// A plain grep over the body matches any commit that merely QUOTES
			// the string — and the commit introducing this check quotes it while
			// explaining it, so it opted itself out of its own gate. The
			// synthetic cases missed that; running it against this repo caught
			// it. Hence `git interpret-trailers`, and hence this case.
			name: "mentioning the trailer is not using it",
			message: "[mache-cccccc] feat: explain the escape hatch\n\n" +
				"When a change has no user-facing surface, say so:\n\n" +
				"    Changelog: none\n\n" +
				"That is the design.",
			wantCode: 1,
			contains: []string{"mache-cccccc"},
		},
		{
			// The case a naive `grep <id> CHANGELOG.md` gets wrong, and why the
			// script slices the Unreleased section rather than searching the file.
			name:     "an id only under a released heading does not count",
			message:  "[mache-bbbbbb] fix(thing): reusing an id that only appears under v1.0.0",
			wantCode: 1,
			contains: []string{"mache-bbbbbb"},
		},
		{
			// The convention is `[bead-id] type(scope): subject`, so a commit
			// naming no bead gives the check nothing to assert on. It must not
			// invent a failure.
			name:     "a commit naming no bead is not a failure",
			message:  "docs: fix a typo",
			wantCode: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, code := runCase(t, unreleasedOnly, tc.message)
			require.Equal(t, tc.wantCode, code, "output:\n%s", out)
			for _, want := range tc.contains {
				assert.Contains(t, out, want)
			}
		})
	}
}

// Every id a commit names must be accounted for — several PRs in this repo's
// history carry two, and covering one of them is not covering the change.
func TestChangelogEntryCheck_EveryIdInASubjectIsChecked(t *testing.T) {
	out, code := runCase(t, unreleasedOnly, "[mache-aaaaaa][mache-cccccc] feat: two beads, one covered")
	require.Equal(t, 1, code, "output:\n%s", out)

	// Assert on the REPORTED ids, not on the whole output: each violation is
	// a "  <id>  <sha>  <subject>" line, and the subject here necessarily
	// echoes the covered id too.
	assert.Equal(t, []string{"mache-cccccc"}, reportedIDs(out),
		"only the uncovered id may be reported; output:\n%s", out)
}

// reportedIDs pulls the bead id out of each violation line.
func reportedIDs(out string) []string {
	var ids []string
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "  ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) > 0 && strings.HasPrefix(fields[0], "mache-") {
			ids = append(ids, fields[0])
		}
	}
	return ids
}

// An unreachable base means the check DID NOT RUN. Reporting that as clean is
// the silent gap mache-ddf14b exists to prevent, so it must fail loudly and
// distinguishably — exit 2, not the exit 1 a real violation uses.
func TestChangelogEntryCheck_UnreachableBaseFailsRatherThanSkips(t *testing.T) {
	r := newChangelogRepo(t, unreleasedOnly)
	r.commit("[mache-cccccc] feat: something")

	out, code := r.run("refs/heads/no-such-base")
	require.Equal(t, 2, code, "a base it cannot resolve must fail, got:\n%s", out)
	assert.Contains(t, out, "cannot resolve")
	assert.Contains(t, out, "fetch-depth: 0", "the remedy must name the CI cause")
}

func TestChangelogEntryCheck_MissingChangelogFails(t *testing.T) {
	r := newChangelogRepo(t, unreleasedOnly)
	base := r.head()
	require.NoError(t, os.Remove(filepath.Join(r.dir, "CHANGELOG.md")))
	r.commit("[mache-cccccc] feat: something")

	out, code := r.run(base)
	require.Equal(t, 2, code, "output:\n%s", out)
	assert.Contains(t, out, "no CHANGELOG.md")
}
