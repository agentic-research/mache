#!/bin/bash
set -e

# Mache Arena — 6-level agent benchmark
# Tests whether an LLM can operate on code through a filesystem abstraction.
# The code is intentionally custom/weird so memorized patterns don't help.
#
# Usage:
#   bash scripts/arena.sh              # Run interactively (keep sandbox)
#   bash scripts/arena.sh --verify     # Run automated verification after agent finishes

SANDBOX="/tmp/mache-arena"
REPO="$SANDBOX/repo"
MNT="$SANDBOX/mnt"
SCOREBOARD="$SANDBOX/scoreboard.md"
MACHE_BIN="$(pwd)/bin/mache"

GREEN='\033[0;32m'
RED='\033[0;31m'
BLUE='\033[0;34m'
YELLOW='\033[0;33m'
NC='\033[0m'

VERIFY=false
if [ "$1" == "--verify" ]; then
    VERIFY=true
fi

cleanup() {
    echo -e "${BLUE}Cleaning up...${NC}"
    umount "$MNT" 2>/dev/null || true
    sleep 0.5
    if [ -n "$PID" ]; then kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; fi
    echo -e "${GREEN}Sandbox kept at $SANDBOX${NC}"
    echo -e "${GREEN}Scoreboard: $SCOREBOARD${NC}"
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Source code — intentionally non-standard, no common libraries
# ---------------------------------------------------------------------------
setup_repo() {
    rm -rf "$REPO"
    mkdir -p "$REPO"

    # File 1: signal_codec.go — custom bit-packing protocol
    cat > "$REPO/signal_codec.go" <<'GOEOF'
package sigwire

// Preamble is the magic header for the sigwire binary format.
var Preamble = []byte{0xCA, 0xFE, 0xD0, 0x0D}

// PackReading encodes a sensor reading into a 4-byte wire frame.
// Layout: [tag:8][value_hi:8][value_lo:8][checksum:8]
// The checksum is tag XOR value_hi XOR value_lo.
func PackReading(tag byte, value uint16) [4]byte {
	hi := byte(value >> 8)
	lo := byte(value & 0xFF)
	return [4]byte{tag, hi, lo, tag ^ hi ^ lo}
}

// UnpackReading decodes a wire frame back to tag + value.
// Returns ok=false if the checksum fails.
func UnpackReading(frame [4]byte) (tag byte, value uint16, ok bool) {
	chk := frame[0] ^ frame[1] ^ frame[2]
	if chk != frame[3] {
		return 0, 0, false
	}
	return frame[0], uint16(frame[1])<<8 | uint16(frame[2]), true
}

// BatchEncode encodes multiple readings into a byte slice with preamble.
func BatchEncode(readings []struct{ Tag byte; Value uint16 }) []byte {
	buf := make([]byte, 0, len(Preamble)+len(readings)*4)
	buf = append(buf, Preamble...)
	for _, r := range readings {
		frame := PackReading(r.Tag, r.Value)
		buf = append(buf, frame[:]...)
	}
	return buf
}

// Accumulate sums all values for a given tag across frames.
// BUG: off-by-one — skips the last frame in the slice.
func Accumulate(frames [][4]byte, targetTag byte) uint64 {
	var total uint64
	for i := 0; i < len(frames)-1; i++ {
		tag, val, ok := UnpackReading(frames[i])
		if ok && tag == targetTag {
			total += uint64(val)
		}
	}
	return total
}

// Threshold returns indices of frames whose value exceeds limit.
func Threshold(frames [][4]byte, limit uint16) []int {
	var hits []int
	for i, f := range frames {
		_, val, ok := UnpackReading(f)
		if ok && val > limit {
			hits = append(hits, i)
		}
	}
	return hits
}
GOEOF

    # File 2: route_table.go — custom routing with priority queue
    cat > "$REPO/route_table.go" <<'GOEOF'
package sigwire

// Route maps a destination prefix to a handler index and priority.
type Route struct {
	Prefix   string
	Handler  int
	Priority int
}

// RouteTable holds an ordered set of routes.
// Higher priority routes are matched first.
type RouteTable struct {
	routes []Route
}

// NewRouteTable creates an empty table.
func NewRouteTable() *RouteTable {
	return &RouteTable{}
}

// Insert adds a route, maintaining descending priority order.
func (rt *RouteTable) Insert(r Route) {
	pos := len(rt.routes)
	for i, existing := range rt.routes {
		if r.Priority > existing.Priority {
			pos = i
			break
		}
	}
	rt.routes = append(rt.routes, Route{})
	copy(rt.routes[pos+1:], rt.routes[pos:])
	rt.routes[pos] = r
}

// Match returns the handler for the first route whose prefix matches addr.
// Returns -1 if no route matches.
func (rt *RouteTable) Match(addr string) int {
	for _, r := range rt.routes {
		if len(addr) >= len(r.Prefix) && addr[:len(r.Prefix)] == r.Prefix {
			return r.Handler
		}
	}
	return -1
}

// Remove deletes all routes with the given prefix. Returns count removed.
func (rt *RouteTable) Remove(prefix string) int {
	n := 0
	kept := rt.routes[:0]
	for _, r := range rt.routes {
		if r.Prefix == prefix {
			n++
		} else {
			kept = append(kept, r)
		}
	}
	rt.routes = kept
	return n
}

// Len returns the number of routes.
func (rt *RouteTable) Len() int {
	return len(rt.routes)
}
GOEOF
}

# ---------------------------------------------------------------------------
# Mount
# ---------------------------------------------------------------------------
setup_mount() {
    mkdir -p "$MNT"
    echo -e "${BLUE}Mounting Mache (writable, NFS)...${NC}"
    "$MACHE_BIN" "$MNT" --infer --writable --data "$REPO" --quiet &
    PID=$!
    sleep 3

    if ! ls "$MNT" >/dev/null 2>&1; then
        echo -e "${RED}FAIL: Mount failed.${NC}"
        exit 1
    fi
    echo -e "${GREEN}Mount ready at $MNT${NC}"
}

# ---------------------------------------------------------------------------
# PROMPT.txt — the agent's briefing
# ---------------------------------------------------------------------------
write_prompt() {
    cat > "$SANDBOX/PROMPT.txt" <<PROMPT
You are an autonomous software engineer in the **Mache Arena** benchmark.

## Environment

The directory \`$MNT\` is a virtual filesystem that exposes a Go codebase
as an **AST overlay**. Instead of seeing .go files, you see the code's
structure:

    \$ ls $MNT/
    PackReading/   UnpackReading/   BatchEncode/   Accumulate/
    Threshold/     Route/           RouteTable/    NewRouteTable/
    Insert/        Match/           Remove/        Len/

Each directory is a code construct (function, type, method). Inside each:

    \$ ls $MNT/PackReading/
    source          # The full source code of this construct
    context         # (Read-only) Imports, types, and globals visible to this scope
    callers/        # (Read-only) Functions that reference this construct
    \$ ls $MNT/PackReading/_diagnostics/
    last-write-status # Write feedback (check after failed writes)
    lint              # (Read-only) Static analysis warnings

**Reverse navigation** — the \`callers/\` directory is a virtual cross-reference
index. It lists every construct that references the parent function/type:

    \$ ls $MNT/UnpackReading/callers/
    functions_Accumulate_source    functions_Threshold_source

    \$ cat $MNT/UnpackReading/callers/functions_Accumulate_source
    # → shows the full source of Accumulate (a caller of UnpackReading)

\`callers/\` is **self-gating** — it only appears when a construct actually has
callers. If a function is never referenced, no \`callers/\` directory is shown.

**To edit code**: overwrite the \`source\` file with the COMPLETE new
implementation. The engine splices your changes back into the real .go files.
Implicit truncation is handled — you do not need to pad writes.

**Writes are always accepted.** If your code has syntax errors, it is saved
as a **Draft** (visible to you but not committed to the source file).
ALWAYS check \`<node>/_diagnostics/last-write-status\` after writing to
confirm it was "ok" (committed) or if there was an error.

## Rules

- Work ONLY through the mount at \`$MNT\`. Do NOT touch \`$REPO\` directly.
- Use shell commands: \`ls\`, \`cat\`, \`printf\`, \`echo\` on mount paths.
- After completing (or failing) each level, append your notes to
  \`$SANDBOX/agent-notes.md\` — what you tried, what worked, what didn't.
- You may attempt each level as many times as you want. Move on when ready.

## Levels

### Level 1: Orientation
Explore the mount. List every construct. Read the source of \`PackReading\`
and \`Accumulate\`. Write a brief summary of what this codebase does to
\`$SANDBOX/agent-notes.md\`.

### Level 2: Bug Hunt
The \`Accumulate\` function has a bug — it produces incorrect results.
Find the bug by reading its source. Fix it by overwriting
\`$MNT/Accumulate/source\` with corrected code. Describe the bug in your notes.

### Level 3: Needle in a Haystack
One function in this codebase has a subtle correctness issue: it works
for most inputs but fails on an edge case when the input is empty.
Find it. Fix it. Explain in your notes which function it was and why.

### Level 4: Cross-Function Refactor
The \`PackReading\` function hardcodes the checksum algorithm (XOR).
Refactor it: extract the checksum into a new local computation that uses
addition modulo 256 instead of XOR. Then update \`UnpackReading\` to match.
Both functions must stay syntactically valid or the write will be rejected.

### Level 5: Adversarial Write
Intentionally write BROKEN syntax to any \`source\` file. Observe what
happens (the write should fail). Then read \`_diagnostics/last-write-status\`
to see the error. Document the full round-trip in your notes.

### Level 6: Reverse Call-Chain Navigation
Without reading every construct, use the \`callers/\` virtual directory to
answer: **which functions depend on \`UnpackReading\`?**

1. List \`$MNT/UnpackReading/callers/\` to discover all callers.
2. Read each caller's source **through the callers/ directory** (e.g.
   \`cat $MNT/UnpackReading/callers/<entry>\`), not by navigating to the
   caller's own directory.
3. Then find a construct that has **zero callers** (no \`callers/\` directory).
4. Document the full dependency chain and the zero-caller construct in your notes.

## Scoring

After you finish, append a self-assessment to \`$SANDBOX/agent-notes.md\`:
- Which levels you completed
- Which you skipped or failed
- What surprised you about this environment

Good luck.
PROMPT
    echo -e "${GREEN}PROMPT.txt written to $SANDBOX/PROMPT.txt${NC}"
}

# ---------------------------------------------------------------------------
# Automated verification (--verify mode)
# ---------------------------------------------------------------------------
verify() {
    echo ""
    echo -e "${BLUE}═══ SCOREBOARD ═══${NC}"
    local score=0
    local total=6

    # Level 1: agent-notes.md exists and mentions the codebase
    echo -n "Level 1 (Orientation): "
    if [ -f "$SANDBOX/agent-notes.md" ] && grep -qi "pack\|codec\|sensor\|wire\|frame" "$SANDBOX/agent-notes.md" 2>/dev/null; then
        echo -e "${GREEN}PASS${NC}"
        score=$((score + 1))
    else
        echo -e "${RED}FAIL${NC} — agent-notes.md missing or no codebase summary"
    fi

    # Level 2: Accumulate bug fixed (off-by-one: len(frames)-1 → len(frames))
    echo -n "Level 2 (Bug Hunt):    "
    if grep -q 'i < len(frames)' "$REPO/signal_codec.go" 2>/dev/null && \
       ! grep -q 'i < len(frames)-1' "$REPO/signal_codec.go" 2>/dev/null; then
        echo -e "${GREEN}PASS${NC}"
        score=$((score + 1))
    else
        echo -e "${RED}FAIL${NC} — Accumulate still has off-by-one"
    fi

    # Level 3: Edge case fix (Threshold, Remove, or Match — empty input)
    # Match has a subtle issue: empty prefix "" matches everything.
    # Remove has: if input routes is empty, kept slice reuses backing array (benign).
    # Threshold: nil frames → nil hits (fine).
    # The most testable: Match("") with prefix="" matches everything (arguably correct).
    # Actually the real needle: Remove mutates the slice header but if no routes match,
    # the function is fine. Let's check if agent found and documented something.
    echo -n "Level 3 (Needle):      "
    if [ -f "$SANDBOX/agent-notes.md" ] && grep -qi "empty\|nil\|edge\|zero\|length 0" "$SANDBOX/agent-notes.md" 2>/dev/null; then
        echo -e "${GREEN}PASS${NC} (documented edge case)"
        score=$((score + 1))
    else
        echo -e "${YELLOW}SKIP${NC} — no edge case documented in notes"
    fi

    # Level 4: Checksum changed from XOR to addition mod 256
    echo -n "Level 4 (Refactor):    "
    if grep -q '+.*% *256\|%256\|& 0xFF\|& 0xff' "$REPO/signal_codec.go" 2>/dev/null && \
       ! grep -q 'tag \^ hi \^ lo' "$REPO/signal_codec.go" 2>/dev/null; then
        echo -e "${GREEN}PASS${NC}"
        score=$((score + 1))
    else
        echo -e "${RED}FAIL${NC} — checksum not refactored to mod-256 addition"
    fi

    # Level 5: Notes mention diagnostics / EIO / write failure
    echo -n "Level 5 (Adversarial): "
    if [ -f "$SANDBOX/agent-notes.md" ] && grep -qi "diagnostic\|draft\|syntax error\|last-write-status" "$SANDBOX/agent-notes.md" 2>/dev/null; then
        echo -e "${GREEN}PASS${NC}"
        score=$((score + 1))
    else
        echo -e "${YELLOW}SKIP${NC} — no adversarial write/draft documented"
    fi

    # Level 6: Notes mention callers + UnpackReading dependency chain + a zero-caller construct
    echo -n "Level 6 (Callers):     "
    if [ -f "$SANDBOX/agent-notes.md" ] && \
       grep -qi "caller" "$SANDBOX/agent-notes.md" 2>/dev/null && \
       grep -qi "UnpackReading\|Accumulate\|Threshold" "$SANDBOX/agent-notes.md" 2>/dev/null && \
       grep -qi "zero.caller\|no.caller\|no callers\|0 caller\|not.called\|never.called\|leaf" "$SANDBOX/agent-notes.md" 2>/dev/null; then
        echo -e "${GREEN}PASS${NC}"
        score=$((score + 1))
    else
        echo -e "${YELLOW}SKIP${NC} — no callers/ navigation documented"
    fi

    echo ""
    echo -e "${BLUE}Score: $score / $total${NC}"

    # Write scoreboard
    cat > "$SCOREBOARD" <<EOF
# Mache Arena Scoreboard

Date: $(date -Iseconds)

| Level | Name | Result |
|-------|------|--------|
| 1 | Orientation | $([ $score -ge 1 ] && echo PASS || echo FAIL) |
| 2 | Bug Hunt | $(grep -q 'i < len(frames)' "$REPO/signal_codec.go" 2>/dev/null && ! grep -q 'i < len(frames)-1' "$REPO/signal_codec.go" 2>/dev/null && echo PASS || echo FAIL) |
| 3 | Needle | $(grep -qi "empty\|nil\|edge" "$SANDBOX/agent-notes.md" 2>/dev/null && echo PASS || echo SKIP) |
| 4 | Refactor | $(grep -q '%.*256\|& 0xFF' "$REPO/signal_codec.go" 2>/dev/null && echo PASS || echo FAIL) |
| 5 | Adversarial | $(grep -qi "diagnostic\|EIO\|reject" "$SANDBOX/agent-notes.md" 2>/dev/null && echo PASS || echo SKIP) |
| 6 | Callers | $(grep -qi "caller" "$SANDBOX/agent-notes.md" 2>/dev/null && grep -qi "zero.caller\|no.caller\|no callers\|not.called\|never.called\|leaf" "$SANDBOX/agent-notes.md" 2>/dev/null && echo PASS || echo SKIP) |

**Total: $score / $total**
EOF
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

# 1. Build if needed
if [ ! -f "$MACHE_BIN" ]; then
    echo "Building mache..."
    task build
fi

# 2. Setup
echo -e "${BLUE}Setting up Arena at $SANDBOX...${NC}"
if [ -d "$SANDBOX" ]; then
    # Preserve the directory itself to avoid "stale file handle" for the user
    find "$SANDBOX" -mindepth 1 -delete
else
    mkdir -p "$SANDBOX"
fi
setup_repo
setup_mount
write_prompt

# 3. Initialize empty agent notes
cat > "$SANDBOX/agent-notes.md" <<EOF
# Mache Arena — Agent Notes

> Append your observations after each level.

EOF

echo ""
echo -e "${GREEN}╔═══════════════════════════════════════════╗${NC}"
echo -e "${GREEN}║         MACHE ARENA READY                 ║${NC}"
echo -e "${GREEN}╠═══════════════════════════════════════════╣${NC}"
echo -e "${GREEN}║  Repo:   $REPO${NC}"
echo -e "${GREEN}║  Mount:  $MNT${NC}"
echo -e "${GREEN}║  Prompt: $SANDBOX/PROMPT.txt${NC}"
echo -e "${GREEN}║  Notes:  $SANDBOX/agent-notes.md${NC}"
echo -e "${GREEN}╚═══════════════════════════════════════════╝${NC}"
echo ""
echo "Point your LLM at $SANDBOX/PROMPT.txt"
echo "When done, press ENTER to run verification."
read -r

verify
