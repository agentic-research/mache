#!/bin/bash
set -e

# Configuration
SANDBOX="/tmp/mache-arena"
REPO="$SANDBOX/repo"
MNT="$SANDBOX/mnt"
MACHE_BIN="$(pwd)/bin/mache"

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
BLUE='\033[0;34m'
NC='\033[0m'

KEEP_SANDBOX=false
if [ "$1" == "--keep" ]; then
    KEEP_SANDBOX=true
    shift
fi

cleanup() {
    echo -e "${BLUE}Cleaning up...${NC}"
    umount "$MNT" 2>/dev/null || true
    sleep 0.5
    if [ -n "$PID" ]; then kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; fi

    if [ "$KEEP_SANDBOX" = true ]; then
        echo -e "${GREEN}Sandbox kept at $SANDBOX${NC}"
    else
        echo -e "${BLUE}Removing sandbox...${NC}"
        rm -rf "$SANDBOX"
    fi
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

# Mission Setup Logic
setup_mission() {
    local mission=$1
    echo "Setting up mission: $mission"
    case $mission in
        "comment"|"interactive"|"syntax-error")
            cat > "$REPO/main.go" <<EOF
package main

func Calculate(a, b int) int {
	return a + b
}

func main() {
	println(Calculate(1, 2))
}
EOF
            ;;
        "imports")
            # Missing "fmt" import
            cat > "$REPO/main.go" <<EOF
package main

func main() {
	fmt.Println("Hello")
}
EOF
            ;;
        "complexity")
            cat > "$REPO/main.go" <<EOF
package main

func Complex(a int) {
	if a > 0 {
		if a > 10 {
			if a > 100 {
				println("Big")
			}
		}
	}
}
EOF
            ;;
        "variable")
            cat > "$REPO/main.go" <<EOF
package main

func Connect() {
	println("Connecting to postgres://localhost:5432")
}
EOF
            ;;
        *)
            echo "Unknown mission type: $mission"
            exit 1
            ;;
    esac
}

MISSION="${1:-interactive}"
SUB_MISSION="${2:-comment}" # Default sub-mission for interactive

if [ "$MISSION" == "interactive" ]; then
    setup_mission "$SUB_MISSION"
else
    setup_mission "$MISSION"
fi

# 3. Mount Mache
echo -e "${BLUE}Mounting Mache...${NC}"
"$MACHE_BIN" "$MNT" --infer --writable --data "$REPO/main.go" &
PID=$!
sleep 2

# Check mount
if [ ! -d "$MNT" ]; then
    echo -e "${RED}FAIL: Mount failed completely.${NC}"
    exit 1
fi

echo -e "${GREEN}Arena Ready!${NC}"
echo "Repo: $REPO"
echo "Mount: $MNT"

# 4. Execute Mission
if [ "$MISSION" == "interactive" ]; then
    echo -e "${BLUE}Interactive Mode ($SUB_MISSION).${NC}"

    # Create PROMPT.txt for the Agent
    cat > "$SANDBOX/PROMPT.txt" <<EOF
You are an autonomous software engineer participating in the **Mache Arena** refactoring challenge.

**Your Environment:**
1.  You are operating inside a sandbox.
2.  The target codebase is mounted as a virtual filesystem at:
    \`MOUNT_POINT="$MNT"\`
3.  This filesystem exposes the code's **Abstract Syntax Tree (AST)**.
    *   Top-level functions are directories (e.g., \`\$MOUNT_POINT/Calculate\`).
    *   The implementation of a node is in a file named \`source\`.

**Your Mission:**
The \`Calculate\` function in the codebase is undocumented. You must add a Go-style comment to it.

**Instructions:**
1.  **Explore:** List the contents of \`\$MOUNT_POINT\` to confirm the function exists.
2.  **Read:** Read the current implementation of \`Calculate\` from its \`source\` file.
3.  **Refactor:** Overwrite the \`source\` file with the **full new content**, which must include:
    *   The comment: \`// Calculate adds two numbers\`
    *   The original function signature and body.

**Constraints:**
*   Do NOT attempt to find or edit \`.go\` files directly.
*   Use ONLY standard shell commands (\`ls\`, \`cat\`, \`echo\`, \`printf\`) on the paths within \`\$MOUNT_POINT\`.
*   Your goal is to trigger the Mache engine to splice your changes back into the source.
EOF

    echo "PROMPT.txt has been created at $SANDBOX/PROMPT.txt"
    echo "You can now inspect $MNT and modify files."
    echo "Press ENTER when done to verify and exit."
    read -r
elif [ "$MISSION" == "comment" ]; then
    echo -e "${BLUE}Mission: Insert Comment${NC}"
    TARGET="$MNT/Calculate/source"
    NEW_CONTENT="// Calculate adds two numbers
func Calculate(a, b int) int {
	return a + b
}"
    echo "$NEW_CONTENT" > "$TARGET"
    sleep 1

    if grep -q "// Calculate adds two numbers" "$REPO/main.go"; then
        echo -e "${GREEN}SUCCESS${NC}"
    else
        echo -e "${RED}FAIL${NC}"
        exit 1
    fi
elif [ "$MISSION" == "syntax-error" ]; then
    echo -e "${BLUE}Mission: Unhappy Path${NC}"
    TARGET="$MNT/Calculate/source"
    echo "func Calculate() { BROKEN }" > "$TARGET"
    sleep 1
    if grep -q "BROKEN" "$REPO/main.go"; then
        echo -e "${GREEN}SUCCESS${NC}"
    else
        echo -e "${RED}FAIL${NC}"
        exit 1
    fi
elif [ "$MISSION" == "imports" ]; then
    echo -e "${BLUE}Mission: Fix Imports (Manual Simulation)${NC}"
    # Mache can't structurally add imports yet without manual schema magic or raw editing.
    # Agent must edit source of `source_file`? Or `main` function?
    # If agent edits `main` function to valid code, that works.
    # But adding import requires editing file scope.
    # Is file scope exposed?
    # /source_file/source ? No, SkipSelfMatch skips it.
    # So Agent CANNOT edit imports via structure if root is skipped!
    echo -e "${RED}Limitation: Cannot edit root node via structure yet.${NC}"
else
    echo "Automated verification not implemented for $MISSION"
fi
