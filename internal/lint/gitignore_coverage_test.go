package lint

// The working tree must stay clean of generated and per-clone files
// (mache-7bc00f). This repo's standing rule is never `git add -A` / `git add .`
// / `git commit -a`, and that rule is only as safe as the ignore list beneath
// it: an untracked artifact sitting in the tree is the hazard the rule exists
// to avoid. Each path below was found dirtying a real checkout.
//
// A test, not a shell script, so it runs under `task test` -> `task ci` -> the
// pre-push hook and CI alongside the other repo invariants here.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/gitutil"
	"github.com/agentic-research/mache/internal/testutil"
)

// ignoredPaths are repo-relative paths that must never become committable,
// each with the reason a future .gitignore edit would need to overturn.
//
// The paths are DELIBERATELY spelled as a concrete file rather than a pattern,
// because `git check-ignore` answers about paths and a pattern that matches
// nothing would pass vacuously.
var ignoredPaths = map[string]string{
	// Written by `mache init`, next to .claude/mcp.json. Committing one would
	// impose this clone's schema binding on everyone else's checkout.
	".mache.json": "`mache init` output is per-clone, like the .claude/ half",

	// The .gitignore section "Session-resume scratch notes" already covers
	// /resume.txt and /mache-distrib-resume.txt; these are the same artifact
	// from different agents.
	"claude-resume.txt": "agent session-resume scratch",
	"codex-resume.txt":  "agent session-resume scratch",

	// benchmarks/cost-quality/ ships tracked .py, so CPython writes bytecode
	// beside it on every run.
	"benchmarks/cost-quality/__pycache__/bench.cpython-313.pyc": "Python bytecode beside tracked scripts",
}

func TestGitignore_CoversGeneratedAndPerClonePaths(t *testing.T) {
	root := testutil.MacheRepoRoot(t)

	for rel, why := range ignoredPaths {
		t.Run(rel, func(t *testing.T) {
			// check-ignore exits 0 when the path IS ignored, 1 when it is not,
			// and >1 on a real error — so the exit code alone cannot separate
			// "not ignored" from "git broke". -v makes a match print the rule
			// that caused it, which is the only positive evidence available.
			cmd := gitutil.HermeticGitCommand("-C", root, "check-ignore", "-v", "--no-index", rel)
			out, err := cmd.Output()
			if err != nil {
				require.Empty(t, strings.TrimSpace(string(out)),
					"git check-ignore failed but printed a match for %s — treat as a git error, not a verdict", rel)
				t.Fatalf("%s is NOT ignored (%s). Add a rule to .gitignore; "+
					"an untracked artifact here is exactly what makes `git add -A` dangerous.", rel, why)
			}

			// The rule text is `<file>:<line>:<pattern>\t<path>`. Assert it
			// names a real rule, so a future `!negation` that happens to exit 0
			// cannot pass as coverage.
			line := strings.TrimSpace(string(out))
			assert.Contains(t, line, ".gitignore:", "%s must be ignored by .gitignore itself, not by %q", rel, line)
			assert.NotContains(t, strings.SplitN(line, "\t", 2)[0], ":!",
				"%s is matched by a NEGATION rule, which un-ignores it: %s", rel, line)
		})
	}
}

// trackedButIgnored are the paths that are BOTH tracked and matched by an
// ignore rule — an incoherent state git resolves by silently favouring the
// index, so the rule looks effective while doing nothing. Both predate this
// test and are filed as mache-8b6a8a; resolving either means deleting or
// untracking a committed file, which is not this change's call to make.
//
// This is a ratchet, not an amnesty: anything NOT listed here fails.
var trackedButIgnored = map[string]string{
	// Slipped in with a bulk docs move (#535, mache-bb5e77) although
	// `_agent_log/` has been ignored since long before it.
	"_agent_log/documentation-synthesis-architect_2026-07-21_agent_log.md": "mache-8b6a8a",

	// .gitignore files six /tools/*-probe/ dirs as "local scratch ...
	// uncommitted experiments". Five do not exist on disk; this one exists AND
	// is committed, so the rule contradicts the repo rather than the reverse.
	"tools/sheaf-subscribe-probe/main.go": "mache-8b6a8a",
}

// TestGitignore_DoesNotIgnoreTrackedFiles guards the other direction: a rule
// broad enough to cover the paths above must not swallow something the repo
// actually ships. `__pycache__/` and `*.pyc` are the ones with reach.
func TestGitignore_DoesNotIgnoreTrackedFiles(t *testing.T) {
	root := testutil.MacheRepoRoot(t)

	cmd := gitutil.HermeticGitCommand("-C", root, "ls-files")
	out, err := cmd.Output()
	require.NoError(t, err, "git ls-files")

	tracked := strings.Fields(string(out))
	require.NotEmpty(t, tracked, "git ls-files returned nothing — the guard would pass vacuously")

	// --no-index is LOAD-BEARING: without it, check-ignore refuses to report a
	// path that is tracked, so every path here would come back clean and this
	// guard would pass no matter how broad the rules got. Verified by adding
	// `*.go` to .gitignore — silent without the flag, caught with it.
	//
	// -v prints the matching rule, which is what separates a real match from a
	// NEGATION (`!testdata/*.db`) — check-ignore reports both, but a negation
	// means the path is explicitly NOT ignored.
	//
	// --stdin batches: one process for ~900 files rather than one per file.
	batch := gitutil.HermeticGitCommand("-C", root, "check-ignore", "--no-index", "-v", "--stdin")
	batch.Stdin = strings.NewReader(strings.Join(tracked, "\n"))
	matched, _ := batch.Output() // exit 1 simply means "none matched", which is the pass

	var offenders []string
	for _, line := range strings.Split(strings.TrimSpace(string(matched)), "\n") {
		rule, path, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		if strings.Contains(rule, ":!") { // a negation un-ignores the path
			continue
		}
		if _, known := trackedButIgnored[path]; known {
			continue
		}
		offenders = append(offenders, line)
	}
	assert.Empty(t, offenders,
		"these files are tracked but .gitignore matches them — either the rule is too broad "+
			"or the file should not be committed. Add to trackedButIgnored only with a bead.")
}

// TestGitignore_RatchetHasNoStaleEntries keeps the allowlist honest: once a
// conflict is resolved, its entry must go, or the next real one can hide
// behind it.
func TestGitignore_RatchetHasNoStaleEntries(t *testing.T) {
	root := testutil.MacheRepoRoot(t)

	for path, bead := range trackedButIgnored {
		t.Run(path, func(t *testing.T) {
			ls := gitutil.HermeticGitCommand("-C", root, "ls-files", "--error-unmatch", path)
			require.NoError(t, ls.Run(),
				"%s is no longer tracked — drop it from trackedButIgnored (%s)", path, bead)

			ci := gitutil.HermeticGitCommand("-C", root, "check-ignore", "--no-index", "-q", path)
			require.NoError(t, ci.Run(),
				"%s is no longer ignored — drop it from trackedButIgnored (%s)", path, bead)
		})
	}
}
