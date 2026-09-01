#!/usr/bin/env bash
#
# Render SVG flamegraphs from a directory of pprof profiles.
#
# Extracted from Taskfile.yml's `flamegraphs` task so the failure handling
# below can be tested (mache-77e389). The Taskfile still owns WHEN this runs;
# this owns HOW, and is the thing a test can drive against fixtures.
#
# THE BUG THIS EXISTS TO FIX. The inline version discarded both stderr streams
# (`2>/dev/null` twice) and treated an empty result as proof of a specific,
# benign cause — it wrote an SVG saying the tool was "too fast for the 100Hz
# sampler". A corrupt profile, a bad flag, or a missing stackcollapse produced
# an artifact affirmatively telling the reader nothing was wrong, and the task
# exited 0 with INDEX.md linking the result. The whole point of the artifact
# is to be evidence, and that failure mode manufactured reassuring evidence
# out of a failure.
#
# THE DISCRIMINATOR is the tool's EXIT STATUS, not the emptiness of its
# output. Measured on go1.26.2: a corrupt profile exits 2 with "unrecognized
# profile format" on stderr, while a VALID profile containing zero samples
# exits 0 and emits header-only raw output. So a non-zero exit surfaces stderr
# and fails the run, while a clean exit with nothing to draw still gets a
# placeholder — and that placeholder now reports only what is known (parsed,
# no samples) rather than guessing at a cause.
set -uo pipefail

dir=${1:?usage: flamegraphs.sh <pprof-dir>}
cd "$dir" || exit 1

failed=0

placeholder() {
	printf "<svg xmlns='http://www.w3.org/2000/svg'><text y='20'>%s</text></svg>\n" "$2" >"$1"
}

# collapse runs `pprof ... | [prefilter] | stackcollapse-go.pl` and writes the
# collapsed stacks to $out. Returns non-zero — after printing the failing
# tool's own stderr — if either stage genuinely failed, as distinct from
# succeeding with nothing to say.
#
# prefilter exists because the heap path needs one and the CPU path must NOT
# have it: pprof's raw heap output has four numeric columns and
# stackcollapse-go.pl's regex expects two, so heap runs through a sed that
# drops columns 3-4. Applying that sed to CPU output would be rewriting lines
# nobody asked it to touch.
collapse() {
	local prof=$1 out=$2 prefilter=$3
	shift 3
	local err raw
	err=$(mktemp) || return 1

	if ! raw=$("$@" 2>"$err"); then
		printf 'ERROR: %s: go tool pprof failed\n' "$prof" >&2
		sed 's/^/    /' <"$err" >&2
		rm -f "$err"
		return 1
	fi

	if ! printf '%s\n' "$raw" | eval "$prefilter" | stackcollapse-go.pl >"$out" 2>"$err"; then
		printf 'ERROR: %s: stackcollapse-go.pl failed\n' "$prof" >&2
		sed 's/^/    /' <"$err" >&2
		rm -f "$err"
		return 1
	fi
	rm -f "$err"
}

collapsed=$(mktemp) || exit 1
trap 'rm -f "$collapsed"' EXIT

for prof in *.cpu.pprof; do
	[ -f "$prof" ] || continue
	tool=$(basename "$prof" .cpu.pprof)
	if ! collapse "$prof" "$collapsed" cat go tool pprof -raw "$prof"; then
		failed=1
		continue
	fi
	if [ ! -s "$collapsed" ]; then
		placeholder "$tool.cpu.svg" "$tool: profile parsed, zero CPU samples recorded"
		continue
	fi
	flamegraph.pl --title="$tool CPU" --colors=go <"$collapsed" >"$tool.cpu.svg"
done

for prof in *.heap.pprof; do
	[ -f "$prof" ] || continue
	case "$prof" in *.heap.baseline.pprof) continue ;; esac
	tool=$(basename "$prof" .heap.pprof)
	baseline="$tool.heap.baseline.pprof"
	if [ ! -f "$baseline" ]; then
		# A missing baseline is a setup problem, not a benign outcome: the
		# delta cannot be computed, so the artifact must not imply it was.
		printf 'ERROR: %s: missing baseline %s; cannot compute heap delta\n' "$prof" "$baseline" >&2
		failed=1
		continue
	fi
	if ! collapse "$prof" "$collapsed" \
		"sed -E 's/^( *[0-9]+ +[0-9]+) +[0-9]+ +[0-9]+:/\\1:/'" \
		go tool pprof -alloc_space -base="$baseline" -raw "$prof"; then
		failed=1
		continue
	fi
	if [ ! -s "$collapsed" ]; then
		placeholder "$tool.heap.svg" "$tool: profile parsed, zero allocation delta over the baseline"
		continue
	fi
	flamegraph.pl --title="$tool heap (alloc delta)" --colors=go <"$collapsed" >"$tool.heap.svg"
done

exit "$failed"
