#!/bin/bash
set -e

# Configuration
SANDBOX="/tmp/mache-git-test"
REPO="$SANDBOX/repo"
MNT="$SANDBOX/mnt"
MACHE_BIN="$(pwd)/mache"

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
NC='\033[0m'

cleanup() {
    echo "Cleaning up..."
    umount "$MNT" 2>/dev/null || true
    if [ -n "$PID" ]; then kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; fi
    rm -rf "$SANDBOX"
}
trap cleanup EXIT

# 1. Build Mache
if [ ! -f "$MACHE_BIN" ]; then
    echo "Building mache..."
    go build -o mache main.go
fi

# 2. Setup Repo
echo "Setting up Repo at $REPO..."
rm -rf "$SANDBOX"
mkdir -p "$REPO" "$MNT"

cd "$REPO"
git init -q
git config user.name "Tester"
git config user.email "test@example.com"

# Commits with explicit dates (Loop to create enough records)
for i in {1..4}; do
    GIT_AUTHOR_DATE="2023-01-15T12:00:00" GIT_COMMITTER_DATE="2023-01-15T12:00:00" git commit --allow-empty -m "Commit 2023-01-$i" -q
    GIT_AUTHOR_DATE="2023-12-31T23:59:00" GIT_COMMITTER_DATE="2023-12-31T23:59:00" git commit --allow-empty -m "Commit 2023-12-$i" -q
    GIT_AUTHOR_DATE="2024-01-01T09:00:00" GIT_COMMITTER_DATE="2024-01-01T09:00:00" git commit --allow-empty -m "Commit 2024-01-$i" -q
done

cd - > /dev/null

# 3. Mount
echo "Mounting Mache (Inferring Git)..."
# We mount the .git directory directly
# Capture output for debugging
"$MACHE_BIN" "$MNT" --data "$REPO/.git" --infer > "$SANDBOX/mache.log" 2>&1 &
PID=$!
sleep 2

# 4. Verify
echo "Verifying Structure..."

# Identify Root Directory (should be 'records')
ROOT_DIR="$MNT/records"
if [ ! -d "$ROOT_DIR" ]; then
    echo "Warning: $ROOT_DIR not found. Listing mount:"
    ls -F "$MNT"
    echo "--- Mache Log ---"
    cat "$SANDBOX/mache.log"
    # Fallback to detect actual root
    ROOT_DIR=$(find "$MNT" -maxdepth 1 -type d | grep -v "^$MNT$" | head -n 1)
    echo "Using Root: $ROOT_DIR"
fi

# Check Year Directories
if [ -d "$ROOT_DIR/2023" ] && [ -d "$ROOT_DIR/2024" ]; then
    echo -e "${GREEN}✅ Temporal Sharding (Years) works${NC}"
else
    echo -e "${RED}❌ Failed: Missing year directories in $ROOT_DIR${NC}"
    echo "Found:"
    ls -F "$ROOT_DIR" 2>/dev/null
    echo "--- Mache Log ---"
    cat "$SANDBOX/mache.log"
    exit 1
fi

# Check Month Directories
if [ -d "$ROOT_DIR/2023/01" ] && [ -d "$ROOT_DIR/2023/12" ]; then
    echo -e "${GREEN}✅ Temporal Sharding (Months) works${NC}"
else
    echo -e "${RED}❌ Failed: Missing month directories in 2023${NC}"
    ls -R "$ROOT_DIR/2023"
    echo "--- Mache Log ---"
    cat "$SANDBOX/mache.log"
    exit 1
fi

# Check ID Suppression
if ls "$ROOT_DIR" | grep -qE "^[0-9a-f]{40}$"; then
    echo -e "${RED}❌ Failed: Raw SHAs found at root${NC}"
    ls "$ROOT_DIR"
    exit 1
else
    echo -e "${GREEN}✅ ID Suppression works${NC}"
fi

echo -e "${GREEN}✨ Git Projection Verification Passed${NC}"
