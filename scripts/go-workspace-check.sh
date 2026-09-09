#!/usr/bin/env bash
# Fail fast, and legibly, when go.work and go.mod disagree about the `go`
# directive.
#
# When they disagree, EVERY go command aborts before doing anything —
# build, vet, test, and therefore every gate built on them. In CI that
# surfaces as a wall of unrelated red checks (lint, smells, test on both
# runners, install-verify, server-json-drift), none of which is the cause,
# with the real message appearing once in the first line of a log behind
# an aggregator job. It reads exactly like infrastructure flake; it was
# misdiagnosed as leyline-provisioning flake once already, and re-running
# the jobs changed nothing (mache-49b87e).
#
# The mismatch arrives on its own: a dependency whose go.mod requires a
# patch-level Go version raises OURS when it is bumped, and dependabot
# edits go.mod and go.sum only — it has no concept of a workspace file.
#
# This check cannot be a Go test. The condition it detects stops `go test`
# from running at all, so a test could never observe it. Hence shell, and
# hence running before anything else in the gate.
#
# Detection is delegated to `go list -m` rather than reimplemented: Go's
# ordering puts `1.26` BEFORE `1.26.0`, which is not what a plain version
# comparison (or sort -V) would say, and the toolchain is the only
# authority on its own rule.
set -uo pipefail

cd "${1:-.}" || exit 1

# No workspace, nothing to disagree about.
[ -f go.work ] || exit 0

if err="$(go list -m 2>&1)"; then
  exit 0
fi

case "$err" in
*"go.work file requires go >="*)
  {
    echo "go.work and go.mod disagree about the go directive."
    echo
    grep -E '^go ' go.mod  | sed 's/^/  go.mod:  /'
    grep -E '^go ' go.work | sed 's/^/  go.work: /'
    echo
    echo "Go reports:"
    printf '%s\n' "$err" | sed 's/^/  /'
    echo
    echo "Every go command fails here, so this would otherwise surface as many"
    echo "unrelated failing checks, none of them the real cause."
    echo
    echo "Fix:  go work use"
  } >&2
  exit 1
  ;;
esac

# Any other module error is real too — surface it rather than swallow it.
printf '%s\n' "$err" >&2
exit 1
