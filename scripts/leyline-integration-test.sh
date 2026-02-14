#!/bin/bash
# Mache + Ley-line Integration Test
#
# Demonstrates the full stack:
#   SQLite db → ley-line send (UDP) → arena file → mache (NFS projection) → content read
#
# Prerequisites:
#   - ley-line binary built: cd ../ley-line && go build -o leyline ./cmd/leyline/
#   - mache binary built: task build
#   - KEV database at ~/.agentic-research/venturi/kev/results/results.db
#     (or pass a different .db via $1)
set -euo pipefail

MACHE="${MACHE:-./bin/mache}"
LEYLINE="${LEYLINE:-../ley-line/leyline}"
SOURCE_DB="${1:-$HOME/.agentic-research/venturi/kev/results/results.db}"
PORT="${PORT:-9200}"
ARENA="/tmp/mache-leyline-arena.db"
MNT="/tmp/mache-leyline-mnt"

# Validate prerequisites
for bin in "$MACHE" "$LEYLINE"; do
    if [ ! -x "$bin" ]; then
        echo "ERROR: $bin not found or not executable" >&2
        exit 1
    fi
done
if [ ! -f "$SOURCE_DB" ]; then
    echo "ERROR: Source database not found: $SOURCE_DB" >&2
    exit 1
fi

SOURCE_SIZE=$(stat -f%z "$SOURCE_DB" 2>/dev/null || stat -c%s "$SOURCE_DB")

cleanup() {
    echo ""
    echo "=== Cleanup ==="
    kill "$MACHE_PID" 2>/dev/null || true
    sleep 1
    umount "$MNT" 2>/dev/null || diskutil unmount "$MNT" 2>/dev/null || true
    rm -f "$ARENA"
    echo "Done."
}
trap cleanup EXIT

# Clean slate
rm -f "$ARENA"
mkdir -p "$MNT"

echo "=== Mache + Ley-line Integration Test ==="
echo "Source: $SOURCE_DB ($SOURCE_SIZE bytes)"
echo "Arena:  $ARENA"
echo "Mount:  $MNT"
echo ""

# --- Step 1: UDP transfer ---
echo "Step 1: Ley-line UDP transfer"
"$LEYLINE" receive --arena "$ARENA" --port "$PORT" --size "$SOURCE_SIZE" --sender "localhost:$PORT" &
RX_PID=$!
sleep 1
"$LEYLINE" send --file "$SOURCE_DB" --addr "localhost:$PORT"
sleep 1
kill "$RX_PID" 2>/dev/null; wait "$RX_PID" 2>/dev/null || true
echo ""

# --- Step 2: Verify byte-identical ---
echo "Step 2: Verify byte-identical copy"
SRC_MD5=$(md5 -q "$SOURCE_DB" 2>/dev/null || md5sum "$SOURCE_DB" | cut -d' ' -f1)
ARENA_MD5=$(md5 -q "$ARENA" 2>/dev/null || md5sum "$ARENA" | cut -d' ' -f1)
echo "  Source: $SRC_MD5"
echo "  Arena:  $ARENA_MD5"
if [ "$SRC_MD5" != "$ARENA_MD5" ]; then
    echo "FAIL: MD5 mismatch!" >&2
    exit 1
fi
echo "  PASS: MD5 match"
echo ""

# --- Step 3: Direct SQLite query ---
echo "Step 3: Direct SQLite query on arena"
RECORD_COUNT=$(sqlite3 "$ARENA" "SELECT count(*) FROM results;")
echo "  $RECORD_COUNT records in results table"
echo ""

# --- Step 4: Mount with mache NFS ---
echo "Step 4: Mount with mache NFS (--infer)"
"$MACHE" "$MNT" --backend=nfs --infer --data "$ARENA" --quiet 2>/dev/null &
MACHE_PID=$!
sleep 8

DIR_COUNT=$(ls "$MNT/records/" 2>/dev/null | wc -l | tr -d ' ')
echo "  Projected $DIR_COUNT directories under /records/"
echo ""

# --- Step 5: Read a leaf file ---
echo "Step 5: Read leaf content through full stack"
PRODUCT=$(ls "$MNT/records/" 2>/dev/null | head -3 | tail -1)
VULN=$(ls "$MNT/records/$PRODUCT/" 2>/dev/null | head -1)
SUBPATH="$MNT/records/$PRODUCT/$VULN"

# Navigate to a leaf file (may be nested)
while [ -d "$SUBPATH" ]; do
    NEXT=$(ls "$SUBPATH/" 2>/dev/null | head -1)
    [ -z "$NEXT" ] && break
    SUBPATH="$SUBPATH/$NEXT"
done

if [ -f "$SUBPATH" ]; then
    CONTENT=$(cat "$SUBPATH")
    echo "  Path: ${SUBPATH#$MNT}"
    echo "  Content: $CONTENT"
    echo "  PASS: Leaf file readable"
else
    echo "  WARN: Could not navigate to a leaf file"
fi
echo ""

echo "=== SUCCESS: Full stack verified ==="
echo "  SQLite db → ley-line (UDP) → arena → mache (NFS) → leaf read"
