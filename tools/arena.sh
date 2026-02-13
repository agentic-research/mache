#!/bin/bash
set -e

# Configuration
SANDBOX="/tmp/mache-arena"
REPO="$SANDBOX/repo"
MNT="$SANDBOX/mnt"
MACHE_BIN="$(pwd)/mache"

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
BLUE='\033[0;34m'
NC='\033[0m'

cleanup() {
    echo -e "${BLUE}Cleaning up...${NC}"
    if [ -n "$PID" ]; then kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; fi
    umount "$MNT" 2>/dev/null || true
    # rm -rf "$SANDBOX" # Keep sandbox for inspection if failed?
}
trap cleanup EXIT

# 1. Build Mache
if [ ! -f "$MACHE_BIN" ]; then
    echo "Building mache..."
    task build
fi

# 2. Setup Sandbox
echo -e "${BLUE}Setting up Arena at $SANDBOX...${NC}"
rm -rf "$SANDBOX"
mkdir -p "$REPO" "$MNT"

# Create a sample Go project
cat > "$REPO/main.go" <<EOF
package main

func Calculate(a, b int) int {
	return a + b
}

func main() {
	println(Calculate(1, 2))
}
EOF

# 3. Mount Mache
echo -e "${BLUE}Mounting Mache...${NC}"
"$MACHE_BIN" "$MNT" --infer --writable --data "$REPO/main.go" &
PID=$!
sleep 2

# Check mount
if [ ! -d "$MNT/Calculate" ]; then
    echo -e "${RED}FAIL: Mount failed or schema inference failed.${NC}"
    ls -R "$MNT"
    exit 1
fi

echo -e "${GREEN}Arena Ready!${NC}"
echo "Repo: $REPO"
echo "Mount: $MNT"

# 4. Run Mission
MISSION="${1:-interactive}"

if [ "$MISSION" == "interactive" ]; then
    echo -e "${BLUE}Interactive Mode.${NC}"
    echo "You can now inspect $MNT and modify files."
    echo "Press ENTER when done to verify and exit."
    read -r
elif [ "$MISSION" == "comment" ]; then
    echo -e "${BLUE}Mission: Insert Comment${NC}"
    TARGET="$MNT/Calculate/source"
    echo "Agent modifying: $TARGET"

    # Simulate Agent: Prepend comment
    # Note: We must write valid Go source for the function
    NEW_CONTENT="// Calculate adds two numbers
func Calculate(a, b int) int {
	return a + b
}"
    echo "$NEW_CONTENT" > "$TARGET"
    sleep 1

    # Verify
    echo -e "${BLUE}Verifying...${NC}"
    if grep -q "// Calculate adds two numbers" "$REPO/main.go"; then
        echo -e "${GREEN}SUCCESS: Comment inserted!${NC}"
        cat "$REPO/main.go"
    else
        echo -e "${RED}FAIL: Comment not found.${NC}"
        cat "$REPO/main.go"
        exit 1
    fi
elif [ "$MISSION" == "syntax-error" ]; then
    echo -e "${BLUE}Mission: Unhappy Path (Syntax Error)${NC}"
    TARGET="$MNT/Calculate/source"
    echo "func Calculate() { BROKEN }" > "$TARGET"
    sleep 1

    if grep -q "BROKEN" "$REPO/main.go"; then
        echo -e "${GREEN}SUCCESS: Broken syntax persisted (Mache did its job).${NC}"
    else
        echo -e "${RED}FAIL: Write rejected?${NC}"
        exit 1
    fi
else
    echo "Unknown mission: $MISSION"
    exit 1
fi
