#!/bin/bash

# test_mache_hybrid.sh
# Comprehensive Integration Test for Mache Filesystem
# Covers: Inference, Context, Truncation, Diagnostics, Write-Back

set -e

# Configuration
SANDBOX_DIR="/tmp/mache-integration"
SRC_DIR="${SANDBOX_DIR}/src"
MNT_DIR="${SANDBOX_DIR}/mnt"
# Use the binary built by task build
MACHE_BIN="$(pwd)/bin/mache"

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
NC='\033[0m'

cleanup() {
    echo "Cleaning up..."
    if [ -n "$MACHE_PID" ]; then
        kill "$MACHE_PID" 2>/dev/null || true
        wait "$MACHE_PID" 2>/dev/null || true
    fi
    if mount | grep -q "$MNT_DIR"; then
        echo "Unmounting $MNT_DIR..."
        umount "$MNT_DIR" 2>/dev/null || true
    fi
    # Don't delete sandbox on failure for debugging
    # rm -rf "$SANDBOX_DIR"
}
trap cleanup EXIT

echo "--- 1. Setup Sandbox ---"
rm -rf "$SANDBOX_DIR"
mkdir -p "$SRC_DIR"
mkdir -p "$MNT_DIR"

# Create a Go file with imports and logic
cat > "${SRC_DIR}/main.go" <<EOF
package main

import "fmt"

const Version = "1.0"

func HelloWorld() {
	fmt.Println("Original Content Long Long Long")
}
EOF

echo "--- 2. Mount with Inference ---"
if [ ! -f "$MACHE_BIN" ]; then
    echo "Building mache..."
    task build
fi

LOG_FILE="${SANDBOX_DIR}/mache.log"
# Test Directory Inference (--infer on directory)
"$MACHE_BIN" "$MNT_DIR" --infer -d "$SRC_DIR" -w --quiet > "$LOG_FILE" 2>&1 &
MACHE_PID=$!

echo "Waiting for mount..."
sleep 3

if ! mount | grep -q "$MNT_DIR"; then
    echo -e "${RED}Mount failed!${NC}"
    cat "$LOG_FILE"
    exit 1
fi

echo "--- 3. Verify Context Awareness ---"
# Check if context file exists and contains imports
CTX_FILE="${MNT_DIR}/HelloWorld/context"
if [ -f "$CTX_FILE" ]; then
    if grep -q 'import "fmt"' "$CTX_FILE"; then
        echo -e "${GREEN}Context: PASS${NC}"
    else
        echo -e "${RED}Context: FAIL - Content mismatch${NC}"
        cat "$CTX_FILE"
        exit 1
    fi
else
    echo -e "${RED}Context: FAIL - File not found${NC}"
    ls -R "$MNT_DIR"
    exit 1
fi

echo "--- 4. Verify Implicit Truncation ---"
# Overwrite with SHORTER content. If truncation fails, old tail remains.
# Original: fmt.Println("Original Content Long Long Long")
# New:      fmt.Println("Short")
TARGET_NODE="${MNT_DIR}/HelloWorld/source"
cat > "$TARGET_NODE" <<EOF
func HelloWorld() {
	fmt.Println("Short")
}
EOF

# Give write-back a moment
sleep 1

# Check source file directly
if grep -q 'Long' "${SRC_DIR}/main.go"; then
    echo -e "${RED}Truncation: FAIL - Found old content tail${NC}"
    cat "${SRC_DIR}/main.go"
    exit 1
else
    echo -e "${GREEN}Truncation: PASS${NC}"
fi

echo "--- 5. Verify Diagnostics (Semantic Firewall) ---"
# Write broken code
echo "func HelloWorld() { BROKEN SYNTAX " > "$TARGET_NODE" || true
# Write should return EIO, but shell might mask it unless checked.
# We check the diagnostics file.

sleep 1
DIAG_FILE="${MNT_DIR}/HelloWorld/_diagnostics/last-write-status"
if [ -f "$DIAG_FILE" ]; then
    if grep -q "syntax error" "$DIAG_FILE" || grep -q "expected" "$DIAG_FILE"; then
        echo -e "${GREEN}Diagnostics: PASS${NC}"
    else
        echo -e "${RED}Diagnostics: FAIL - No error reported${NC}"
        cat "$DIAG_FILE"
        exit 1
    fi
else
    echo -e "${RED}Diagnostics: FAIL - File not found${NC}"
    exit 1
fi

echo "--- 6. Verify Recovery (Valid Write) ---"
cat > "$TARGET_NODE" <<EOF
func HelloWorld() {
	fmt.Println("Fixed")
}
EOF
sleep 1
if grep -q "Fixed" "${SRC_DIR}/main.go"; then
    echo -e "${GREEN}Recovery: PASS${NC}"
else
    echo -e "${RED}Recovery: FAIL${NC}"
    exit 1
fi

echo -e "${GREEN}ALL TESTS PASSED${NC}"
