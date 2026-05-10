#!/usr/bin/env bash
# docs-lint.sh — sanity-check top-level docs/*.md frontmatter against
# the actual repo state. Run via `task docs:lint`.
#
# Checks:
#   1. Frontmatter block exists.
#   2. `covers-version` matches the latest `## [vX.Y.Z]` heading in CHANGELOG.md.
#   3. `last-verified` is within LINT_MAX_AGE_DAYS (default 90).
#   4. Prose language-count claims that explicitly reference mache (patterns
#      like "mache supports N languages", "mache's N tree-sitter languages",
#      "mache (+ leyline) N langs") match the count in internal/lang/lang.go.
#      Narrow on purpose: docs in this tree compare mache against competitors
#      with their own language counts ("Aider supports 100+"), and a broader
#      regex would false-positive on those. Use <!-- docs-lint:ignore --> on
#      a line to suppress an intentional historical quote.
#
# Scope: top-level docs/*.md only. Subdirectories (adr/, archive/, schemas/,
# superpowers/) are skipped.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LINT_MAX_AGE_DAYS="${LINT_MAX_AGE_DAYS:-90}"

# ---------- repo facts ----------

# Latest released version from CHANGELOG (skips an Unreleased heading if any).
LATEST_VERSION=$(
    grep -E "^## \[v[0-9]+\.[0-9]+\.[0-9]+\]" "$REPO_ROOT/CHANGELOG.md" \
        | head -1 \
        | sed -E 's/^## \[(v[0-9]+\.[0-9]+\.[0-9]+)\].*/\1/'
)
if [[ -z "$LATEST_VERSION" ]]; then
    echo "ERROR: could not parse latest version from CHANGELOG.md" >&2
    exit 2
fi

# Source-of-truth language count.
LANG_COUNT=$(grep -cE "^\s*\{Name:" "$REPO_ROOT/internal/lang/lang.go")
if [[ "$LANG_COUNT" -lt 1 ]]; then
    echo "ERROR: failed to count language registry entries" >&2
    exit 2
fi

# Date LINT_MAX_AGE_DAYS ago. macOS date and GNU date differ; try both.
if date -v-1d +%Y-%m-%d >/dev/null 2>&1; then
    CUTOFF=$(date -u -v-"${LINT_MAX_AGE_DAYS}d" +%Y-%m-%d)
else
    CUTOFF=$(date -u -d "-${LINT_MAX_AGE_DAYS} days" +%Y-%m-%d)
fi

# ---------- lint loop ----------

fail=0
warn=0

# Top-level docs only (Glob: docs/*.md). Excludes subdirectories.
shopt -s nullglob
for f in "$REPO_ROOT"/docs/*.md; do
    rel="${f#"$REPO_ROOT/"}"

    # 1. Frontmatter block must open on line 1.
    if [[ "$(head -1 "$f")" != "---" ]]; then
        echo "FAIL $rel — missing frontmatter (line 1 is not '---')"
        fail=$((fail + 1))
        continue
    fi

    # Extract the frontmatter block (everything between the first two --- lines).
    fm=$(awk 'NR==1 && /^---$/ {flag=1; next} flag && /^---$/ {exit} flag' "$f")

    covers=$(echo "$fm" | awk -F': *' '/^covers-version:/ {print $2; exit}')
    verified=$(echo "$fm" | awk -F': *' '/^last-verified:/ {print $2; exit}')

    # 2. covers-version must match latest CHANGELOG version.
    if [[ -z "$covers" ]]; then
        echo "FAIL $rel — frontmatter missing covers-version"
        fail=$((fail + 1))
    elif [[ "$covers" != "$LATEST_VERSION" ]]; then
        echo "FAIL $rel — covers-version=$covers but latest CHANGELOG is $LATEST_VERSION"
        fail=$((fail + 1))
    fi

    # 3. last-verified within window.
    if [[ -z "$verified" ]]; then
        echo "FAIL $rel — frontmatter missing last-verified"
        fail=$((fail + 1))
    elif [[ "$verified" < "$CUTOFF" ]]; then
        echo "WARN $rel — last-verified=$verified is older than ${LINT_MAX_AGE_DAYS} days (cutoff=$CUTOFF)"
        warn=$((warn + 1))
    fi

    # 4. Prose language-count claims that explicitly reference mache. The
    #    pattern is narrow on purpose: flag "mache supports N langs/languages"
    #    or "mache's N tree-sitter languages" with N != LANG_COUNT.
    #    Lines containing `<!-- docs-lint:ignore -->` are skipped — use this
    #    to mark intentional quotes of historical (now-wrong) claims.
    bad_lang_lines=$(
        grep -niE "mache('s| supports| has)?\s+(\(?\+?\s*leyline\)?\s+)?[0-9]{1,3}\s+(tree-sitter\s+)?(langs?|languages?)\b" "$f" \
            | grep -vE "\b${LANG_COUNT}\s+(tree-sitter\s+)?(langs?|languages?)\b" \
            | grep -vE "docs-lint:ignore" \
            || true
    )
    if [[ -n "$bad_lang_lines" ]]; then
        while IFS= read -r line; do
            echo "FAIL $rel — stale mache language count: $line"
            fail=$((fail + 1))
        done <<< "$bad_lang_lines"
    fi
done

echo
echo "docs:lint — latest version: $LATEST_VERSION | language registry count: $LANG_COUNT"
echo "docs:lint — $fail fail, $warn warn"

if [[ "$fail" -gt 0 ]]; then
    exit 1
fi
