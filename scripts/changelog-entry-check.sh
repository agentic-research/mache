#!/usr/bin/env bash
# Fail when a change lands work without saying so in the changelog.
#
# 19 of the PRs merged between v0.21.1 and 9bbd8e6 touched CHANGELOG.md not
# at all, and what went missing was not trivia — the whole `mache doctor`
# command, `mache install`, the launchd reload fix, two leyline pin bumps.
# Nothing noticed, because nothing looks. The omission surfaced weeks later
# at a release cut, when the intent had to be reconstructed from commit
# bodies (mache-4fdbfd, mache-55faa4).
#
# WHY A SCRIPT AND NOT A GO TEST. "Did this change update the changelog" is a
# property of a DIFF, not of the tree, so it needs history — and the `test`
# job in ci.yml checks out at actions/checkout's default fetch-depth of 1,
# with no tags and no base history. A Go test run there could only skip or
# pass vacuously, which mache-ddf14b established is worse than no gate.
# Same split as go-workspace-check.sh: detection here, failure handling
# covered by internal/lint/changelog_entry_test.go.
#
# WHY A TRAILER AND NOT A RULE OVER SCOPES. The obvious rule — "every feat:
# or fix: needs an entry" — is wrong. Three of those 19 SHOULD have had no
# entry, and one of them (mache-3e78d2, `fix(projcfg)`) has production-code
# scope with a test-only effect, so the scope cannot tell you. The author
# knows, at the moment they commit, and nobody else does afterwards. The
# trailer moves the judgement there and makes the omission reviewable
# instead of silent.
#
# Usage: changelog-entry-check.sh <base-ref> [repo-root]
set -uo pipefail

BASE="${1:?usage: changelog-entry-check.sh <base-ref> [repo-root]}"
cd "${2:-.}" || exit 1

TRAILER="Changelog: none"

# A base ref we cannot resolve means the check DID NOT RUN. Say so and fail,
# rather than reporting the clean result of having looked at nothing.
if ! MERGE_BASE=$(git merge-base "$BASE" HEAD 2>/dev/null) || [ -z "$MERGE_BASE" ]; then
  {
    echo "changelog-entry-check: cannot resolve a merge base for '$BASE'."
    echo
    echo "This check compares the commits a change ADDS against the Unreleased"
    echo "section, so without history it has nothing to compare and would pass"
    echo "no matter what. That is a silent gap, not a clean result, so it fails."
    echo
    echo "In CI this usually means a shallow checkout — the job needs"
    echo "  - uses: actions/checkout"
    echo "    with:"
    echo "      fetch-depth: 0"
  } >&2
  exit 2
fi

if [ ! -f CHANGELOG.md ]; then
  echo "changelog-entry-check: no CHANGELOG.md in $(pwd)" >&2
  exit 2
fi

# The Unreleased section only: an id that appears solely under a RELEASED
# heading describes shipped work and says nothing about this change.
UNRELEASED=$(awk '
  /^## \[Unreleased\]/ { inside = 1; next }
  /^## \[/            { inside = 0 }
  inside              { print }
' CHANGELOG.md)

missing=0
while IFS= read -r sha; do
  [ -n "$sha" ] || continue

  # Merge commits carry their parents' ids without being the change itself.
  if [ "$(git rev-list --no-walk --count --merges "$sha")" -gt 0 ]; then
    continue
  fi

  subject=$(git log -1 --format=%s "$sha")
  body=$(git log -1 --format=%B "$sha")

  # The author said this one needs no entry. Their call, recorded.
  #
  # Parsed as a git TRAILER, not grepped out of the body. A plain search
  # matches any commit that merely MENTIONS the string — including the one
  # that introduced this check, whose message quotes it while explaining it.
  # That commit opted itself out of its own gate and the synthetic tests did
  # not notice; only running it against this repo did. `git interpret-trailers`
  # is git's own authority on which lines are trailers, the same reason
  # go-workspace-check.sh delegates to `go list -m`.
  if printf '%s\n' "$body" \
    | git interpret-trailers --parse 2>/dev/null \
    | grep -qiE "^Changelog:[[:space:]]*none[[:space:]]*$"; then
    continue
  fi

  # Bead ids are the unit an entry is keyed by, and the commit convention is
  # `[bead-id] type(scope): subject`. A commit naming none cannot be checked.
  ids=$(printf '%s' "$subject" | grep -oE '[a-z][a-z-]*-[0-9a-f]{6}' | sort -u)
  [ -n "$ids" ] || continue

  while IFS= read -r id; do
    [ -n "$id" ] || continue
    if ! printf '%s' "$UNRELEASED" | grep -qF "$id"; then
      if [ "$missing" -eq 0 ]; then
        echo "changelog-entry-check: work landed without a changelog entry." >&2
        echo >&2
      fi
      missing=$((missing + 1))
      echo "  $id  ${sha:0:9}  $subject" >&2
    fi
  done <<< "$ids"
done <<< "$(git rev-list "$MERGE_BASE"..HEAD)"

if [ "$missing" -gt 0 ]; then
  {
    echo
    echo "Add an entry under '## [Unreleased]' in CHANGELOG.md naming the bead id,"
    echo "describing what the change does for someone using mache."
    echo
    echo "If the change genuinely has no user-facing surface — test or CI hygiene,"
    echo "a package move, bead bookkeeping — say so in the commit message instead:"
    echo
    echo "    $TRAILER"
    echo
    echo "That is a deliberate, reviewable choice. Leaving it out is a silent one,"
    echo "which is how 19 PRs' worth of work went unrecorded (mache-4fdbfd)."
  } >&2
  exit 1
fi

echo "changelog-entry-check: every bead in $BASE..HEAD is accounted for"
