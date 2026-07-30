package installverify

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// expectEnv pins the version the installed binary must report, for the
// release-verification mode of this gate: `task install:verify
// BIN=/tmp/mache-v0.20.0 MACHE_VERIFY_EXPECT_VERSION=0.20.0` checks a
// downloaded release asset against its own tag (bead mache-4d7f2c) rather than
// against the working tree.
const expectEnv = "MACHE_VERIFY_EXPECT_VERSION"

// versionLineRe parses `mache version 0.20.0-2-g56c54b3 (commit …, built …)`.
// Anchored on the "mache version " prefix so a binary that prints something
// else entirely fails loudly instead of matching a stray semver elsewhere in
// its output.
var versionLineRe = regexp.MustCompile(`(?m)^mache version ([^\s]+) `)

// describeRe decomposes a `git describe`-shaped version into its parts:
// <base>[-<distance>-g<sha>][-dirty]. `task build` stamps exactly this, and
// cmd.Version strips only the leading "v".
var describeRe = regexp.MustCompile(`^(\d+\.\d+\.\d+)(?:-(\d+)-g([0-9a-f]+))?(-dirty)?$`)

// reportedVersion runs `<mache> version` and returns the version token.
func reportedVersion(t *testing.T, bin string) string {
	t.Helper()
	res := runner{}.mustRun(t, bin, "version")
	m := versionLineRe.FindStringSubmatch(res.stdout)
	require.Lenf(t, m, 2,
		"`%s version` must print `mache version <ver> (commit …, built …)`; got:\n%s", bin, res.combined())
	return m[1]
}

// git runs a git command in root, returning trimmed stdout and whether it
// succeeded. Every git fact this gate uses is optional — a released tarball
// verified outside a checkout has no git at all.
func git(t *testing.T, root string, args ...string) (string, bool) {
	t.Helper()
	res := runner{dir: root}.run(t, "git", args...)
	if res.err != nil || res.code != 0 {
		return "", false
	}
	return strings.TrimSpace(res.stdout), true
}

// tagAtHEAD returns the v-prefixed release tag pointing at HEAD, or "".
// Mirrors `task version:check`'s `git tag --points-at HEAD | grep '^v'`.
func tagAtHEAD(t *testing.T, root string) string {
	t.Helper()
	out, ok := git(t, root, "tag", "--points-at", "HEAD")
	if !ok {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		if tag := strings.TrimSpace(line); strings.HasPrefix(tag, "v") {
			return tag
		}
	}
	return ""
}

// versionTxt is the canonical clean release base, read from the TREE rather
// than imported from internal/buildinfo on purpose: importing it would compile
// the expectation from the same source that produced the binary under test,
// which cannot detect a stale installed artifact. The file is the contract.
func versionTxt(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "internal", "buildinfo", "version.txt"))
	require.NoError(t, err, "read internal/buildinfo/version.txt")
	v := strings.TrimSpace(string(b))
	require.NotEmpty(t, v, "internal/buildinfo/version.txt is empty")
	return v
}

// TestInstalledMacheReportsExpectedVersion is gate (b): the installed binary's
// reported version must be one this tree could have produced.
//
// The failure it catches, stated plainly: `mache` is not the mache you think
// it is. That has cost an afternoon in this repo already — a Homebrew-tap
// mache reporting v0.6.6 shadowed a v0.19.0 install, and because both answer
// to the same name the skew was only findable by auditing PATH by hand (see
// the mache-6ec106 triage).
//
// How strict the comparison is depends on how exact an answer exists:
//
//   - MACHE_VERIFY_EXPECT_VERSION set (release-asset mode) -> exact equality;
//   - a tag at HEAD -> exact equality with the tag, mirroring `task
//     version:check`, which checks version.txt against melange.yaml and the tag
//     but never against a binary;
//   - otherwise -> the RELEASE BASE must be one this tree carries, and the
//     embedded commit must be an ancestor of HEAD.
//
// That last case is deliberately not exact-describe equality. A binary built
// three commits ago reports a smaller distance than `git describe` says today,
// and `task build`'s up-to-date check legitimately skips a rebuild when only
// non-Go files changed — neither is an installation defect, and a gate that
// cried wolf on both would be turned off. The ancestry check keeps the
// load-bearing half: a FOREIGN binary's commit is not in this history.
func TestInstalledMacheReportsExpectedVersion(t *testing.T) {
	bin := macheBinary(t)
	root := macheTreeRoot(t)
	got := reportedVersion(t, bin)

	if want := strings.TrimPrefix(strings.TrimSpace(os.Getenv(expectEnv)), "v"); want != "" {
		assert.Equalf(t, want, got,
			"installed mache at %s reports %q but %s says %q", bin, got, expectEnv, want)
		return
	}

	clean := versionTxt(t, root)
	if tag := tagAtHEAD(t, root); tag != "" {
		assert.Equal(t, "v"+clean, tag,
			"tag at HEAD must equal v<version.txt> — the same invariant `task version:check` enforces")
		assert.Equalf(t, clean, got,
			"HEAD is tagged %s, so the installed binary must report %q exactly (no git-distance suffix); got %q from %s",
			tag, clean, got, bin)
		return
	}

	parts := describeRe.FindStringSubmatch(got)
	require.Lenf(t, parts, 5,
		"installed mache at %s reports %q, which is not a version this tree's build could produce "+
			"(expected <base>[-<distance>-g<sha>][-dirty])", bin, got)
	base, sha := parts[1], parts[3]

	// The base tracks the most recent REACHABLE tag, which legitimately trails
	// version.txt inside the release window (version.txt is bumped in the
	// release commit and tagged afterwards). Accept either, and name both.
	lastTag := strings.TrimPrefix(firstOK(git(t, root, "describe", "--tags", "--abbrev=0")), "v")
	candidates := dedupe([]string{clean, lastTag})
	assert.Containsf(t, candidates, base,
		"installed mache at %s has release base %q, which matches neither version.txt (%q) nor the "+
			"last reachable tag (%q). If this is a published asset, state its version: %s=<version>.",
		bin, base, clean, lastTag, expectEnv)

	if sha == "" {
		return // a bare `go build` stamps no commit; nothing further to check
	}
	_, ok := git(t, root, "merge-base", "--is-ancestor", sha, "HEAD")
	assert.Truef(t, ok,
		"installed mache at %s was built from commit %s, which is not an ancestor of HEAD — "+
			"that binary did not come from this tree", bin, sha)
}

// firstOK collapses git's (value, ok) to just the value; callers that treat ""
// as "unknown" do not need to branch.
func firstOK(v string, _ bool) string { return v }

// dedupe drops empties and duplicates, so an assertion message lists only real
// candidates.
func dedupe(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
