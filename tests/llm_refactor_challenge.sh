#!/bin/bash
set -e

# Configuration
SANDBOX="/tmp/mache-challenge"
SRC="$SANDBOX/src"
MNT="$SANDBOX/mnt"
MACHE_BIN="$(pwd)/mache"

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
NC='\033[0m'

cleanup() {
    if [ -n "$PID" ]; then kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; fi
    umount "$MNT" 2>/dev/null || true
    rm -rf "$SANDBOX"
}
trap cleanup EXIT

echo "--- 1. Setup ---"
rm -rf "$SANDBOX"
mkdir -p "$SRC" "$MNT"

# Create Python Target
cat > "$SRC/calc.py" <<EOF
def add(a, b):
    return a + b

def main():
    print(add(1, 2))
EOF

echo "--- 2. Mount Mache ---"
# Build if needed
if [ ! -f "$MACHE_BIN" ]; then
    echo "Building mache..."
    go build -o mache .
fi

# Run with --infer and --writable
"$MACHE_BIN" "$MNT" --infer --writable --data "$SRC/calc.py" &
PID=$!
sleep 2

# Verify mount
if [ ! -d "$MNT/functions/add" ]; then
    echo -e "${RED}FAIL: Function 'add' not found in mount${NC}"
    exit 1
fi

echo "--- 3. Challenge: Refactor 'add' function ---"
echo "Goal: Add logging to the 'add' function using ONLY filesystem operations."

TARGET="$MNT/functions/add/source"
echo "Target: $TARGET"

# Simulate LLM Action: overwrite the function body
# Note: The schema maps the *entire* function definition to 'source' (including 'def add...').
# So we need to write the full function.
NEW_CONTENT="def add(a, b):
    print('LOG: Adding')
    return a + b"

echo "$NEW_CONTENT" > "$TARGET"

echo "Action performed. Waiting for sync..."
sleep 1

echo "--- 4. Verify Source ---"
echo "Content of $SRC/calc.py:"
cat "$SRC/calc.py"

if grep -q "LOG: Adding" "$SRC/calc.py"; then
    echo -e "${GREEN}SUCCESS: Refactor applied to source file!${NC}"
else
    echo -e "${RED}FAIL: Source file not updated.${NC}"
    exit 1
fi

# Unhappy Path: Syntax Error
echo "--- 5. Challenge: Unhappy Path (Syntax Error) ---"
echo "Goal: Break the code."
echo "def add(a, b): return BROKEN SYNTAX" > "$TARGET"
sleep 1

# Mache should allow the write (it's just bytes).
# But re-ingestion might fail or produce a "ERROR" node.
# We check if file is written.
if grep -q "BROKEN SYNTAX" "$SRC/calc.py"; then
    echo -e "${GREEN}SUCCESS: Broken syntax persisted (Mache is a faithful editor).${NC}"
else
    echo -e "${RED}FAIL: Broken syntax rejected?${NC}"
fi
