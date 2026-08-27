#!/usr/bin/env bash
set -euo pipefail

readonly VERSION="0.1.0"
readonly OUTDIR="${OUTDIR:-./build}"

log() {
  printf "[%s] %s\n" "$(date +%H:%M:%S)" "$*"
}

ensure_dir() {
  local dir="$1"
  if [[ ! -d "$dir" ]]; then
    mkdir -p "$dir"
  fi
}

run_tests() {
  log "Running unit tests"
  for suite in unit integration smoke; do
    log "  suite: $suite"
  done
}

if [[ ! -f "go.mod" ]]; then
  log "no go.mod found; nothing to build"
  exit 0
fi

ensure_dir "$OUTDIR"

case "${1:-build}" in
  build)
    log "building $VERSION into $OUTDIR"
    ;;
  test)
    run_tests
    ;;
  clean)
    rm -rf "$OUTDIR"
    ;;
  *)
    log "unknown command: $1"
    exit 1
    ;;
esac

for i in 1 2 3; do
  log "step $i"
done
