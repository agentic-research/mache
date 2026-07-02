#!/usr/bin/env bash
# actions-pin-lint.sh — fail if any GitHub Actions `uses:` ref under
# .github/workflows/ is not pinned to a 40-hex commit SHA.
#
# Why: a bare tag (actions/checkout@v4) is a mutable, remotely-controlled
# pointer — a supply-chain foothold. Dependabot bumps these but occasionally
# leaves a bare tag behind (it did on lfs-guard.yml in #457, caught by hand).
# This makes the invariant executable so the next one can't merge unnoticed.
# Pairs with actionlint (workflow *syntax*, mache-f60571); this is *pinning*.
# Bead mache-b8900d.
#
# Exemptions:
#   - local composite refs `uses: ./...` (nothing to pin)
# Docker refs (`uses: docker://image@sha256:<64-hex>`) must be digest-pinned;
# a mutable docker tag (docker://alpine:3.19) is flagged like any other
# unpinned ref.
#
# Usage: actions-pin-lint.sh [workflows-dir]   (defaults to repo .github/workflows)
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
wf_dir="${1:-$repo_root/.github/workflows}"

if [ ! -d "$wf_dir" ]; then
	echo "actions-pin-lint: no $wf_dir — nothing to check"
	exit 0
fi

fail=0
# Anchor on step-style `uses:` lines (optional leading `- `). We use
# `find … -exec grep -H` rather than `grep -r --include` because the latter's
# flags are GNU extensions; this form is portable to BSD/macOS grep. grep -H
# emits file:line:content; we parse the file for reporting and the ref from
# the content.
while IFS= read -r hit; do
	file="${hit%%:*}"
	ref="$(printf '%s' "$hit" | sed -E 's/.*uses:[[:space:]]*//; s/[[:space:]#].*//; s/["'"'"']//g')"
	[ -n "$ref" ] || continue
	case "$ref" in
	./*) continue ;; # local composite action — nothing to pin
	docker://*)
		# Docker actions pin by digest: docker://image@sha256:<64-hex>.
		if ! printf '%s' "$ref" | grep -qE '@sha256:[0-9a-fA-F]{64}$'; then
			echo "UNPINNED: $file → $ref"
			fail=1
		fi
		continue
		;;
	esac
	# Everything after the last @ must be a 40-hex commit SHA (either case).
	after="${ref##*@}"
	if ! printf '%s' "$after" | grep -qE '^[0-9a-fA-F]{40}$'; then
		echo "UNPINNED: $file → $ref"
		fail=1
	fi
done < <(find "$wf_dir" -type f \( -name '*.yml' -o -name '*.yaml' \) -exec grep -nHE '^[[:space:]]*-?[[:space:]]*uses:' {} + 2>/dev/null || true)

if [ "$fail" -ne 0 ]; then
	echo
	echo "actions-pin-lint: unpinned action ref(s) above — pin to a 40-hex commit SHA"
	echo "  (keep a '# vX.Y.Z' trailing comment for readability). Dependabot will still bump the SHA."
	exit 1
fi

echo "actions-pin-lint: all workflow \`uses:\` refs are SHA-pinned ✓"
