#!/bin/bash
set -e

# Configuration
SANDBOX="/tmp/mache-torture"
REPO="$SANDBOX/repo"
MNT="$SANDBOX/mnt"
MACHE_BIN="bin/mache"

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
NC='\033[0m'

cleanup() {
    umount "$MNT" 2>/dev/null || true
    if [ -n "$PID" ]; then kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; fi
    rm -rf "$SANDBOX"
}
trap cleanup EXIT

# 1. Build
if [ ! -f "$MACHE_BIN" ]; then
    echo "Building mache..."
    task build
fi

# 2. Setup
rm -rf "$SANDBOX"
mkdir -p "$REPO" "$MNT"

# Create Initial File (Small)
cat > "$REPO/main.go" <<EOF
package main

func Calculate() {
	println("Old")
}
EOF

# 3. Mount
echo "Mounting..."
LOG_FILE="$SANDBOX/mache.log"
"$MACHE_BIN" "$MNT" --infer --writable --data "$REPO/main.go" > "$LOG_FILE" 2>&1 &
PID=$!
sleep 2

if [ ! -d "$MNT" ]; then
    echo "Mount failed"
    cat "$LOG_FILE"
    exit 1
fi

echo "Arena mounted."

# 4. Torture Test: "The Zombie Node" (Write Larger Content)
TARGET="$MNT/Calculate/source"
echo "Target: $TARGET"

# Check Initial Size
SIZE_OLD=$(ls -l "$TARGET" | awk '{print $5}')
echo "Old Size: $SIZE_OLD"

# Overwrite with LARGER content (Trigger Zombie Bug)
NEW_CONTENT_FILE="$SANDBOX/new_content.txt"
cat > "$NEW_CONTENT_FILE" <<'EOF'
// This comment makes the file significantly larger than before.
func Calculate() {
	println("New Larger Content")
}
EOF

cat "$NEW_CONTENT_FILE" > "$TARGET"

# Wait for splice/ingest
sleep 1

# Check New Size
SIZE_NEW=$(ls -l "$TARGET" | awk '{print $5}')
echo "New Size: $SIZE_NEW"

if [ "$SIZE_NEW" -le "$SIZE_OLD" ]; then
    echo -e "${RED}FAIL: Size did not increase! Zombie Node detected.${NC}"
    echo "Expected > $SIZE_OLD, Got $SIZE_NEW"
    echo "--- Mache Logs ---"
    cat "$LOG_FILE"
    echo "------------------"
    exit 1
fi

# Check Content
READ_BACK=$(cat "$TARGET")
echo "Read Back:"
echo "$READ_BACK"

EXPECTED=$(cat "$NEW_CONTENT_FILE")
if [[ "$READ_BACK" != *"$EXPECTED"* ]]; then
     echo -e "${RED}FAIL: Content mismatch/truncated.${NC}"
     echo "--- Repo Content (Physical) ---"
     cat "$REPO/main.go"
     echo "-------------------------------"
     echo "--- Mache Logs ---"
     cat "$LOG_FILE"
     echo "------------------"
     exit 1
fi

echo -e "${GREEN}PASS: Zombie Node killed. Updates visible.${NC}"
