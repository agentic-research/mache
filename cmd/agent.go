package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// agentMetadata holds metadata for agent mode mounts (set by runAgentMode).
var agentMetadata *MountMetadata

// MountMetadata stores information about an agent-mode mount.
type MountMetadata struct {
	PID        int       `json:"pid"`
	Source     string    `json:"source"`
	MountPoint string    `json:"mount_point"`
	Type       string    `json:"type,omitempty"`       // "nfs", "fuse", "mcp-http", "mcp-stdio"
	GitRepo    string    `json:"git_repo,omitempty"`   // org/repo
	GitBranch  string    `json:"git_branch,omitempty"` // branch name
	GitRemote  string    `json:"git_remote,omitempty"` // full remote URL
	Timestamp  time.Time `json:"timestamp"`
	Writable   bool      `json:"writable"`
	Addr       string    `json:"addr,omitempty"` // listen address for MCP HTTP servers
}

// agentPromptTemplate is the instruction file generated for LLM agents.
const agentPromptTemplate = `# Mache — Semantic Filesystem

Source: %s | Git: %s | Mount: %s | Mode: %s
%s

## What This Is

Your codebase has been projected into a **semantic filesystem**. Source files
do not exist here. Instead, every function, method, type, constant, and
variable is a directory you can ls, cat, and write to by name.

You do NOT need to know file paths. Navigate by symbol name.

## Filesystem Layout

Root directories are **packages** (Go) or **languages** (inferred schema):
` + "```" + `
ls /
# api/  cartographer/  navigator/  iterm/  _project_files/  _schema.json  PROMPT.txt
` + "```" + `

Each package contains these subdirectories:
` + "```" + `
ls api/
# constants/  functions/  methods/  types/  variables/  imports/  _diagnostics/
` + "```" + `

Each symbol is a directory with these files:

| File | Contents | Use |
|------|----------|-----|
| source | The code for this ONE construct | Read and write |
| context | Imports, surrounding types, and globals from the same file | Read to understand dependencies |
| callers/ | Directory listing functions that call this one | Read to trace usage |
| callees/ | Directory listing functions this one calls | Read to trace dependencies |
| _diagnostics/ | lint, ast-errors, last-write-status | Read after writes |

## Discovery (finding what you need)

**Step 1: Understand the codebase structure.**
` + "```" + `
ls /                       # List packages (or languages)
ls api/                    # List symbol categories in a package
ls api/types/              # List all types in the api package
` + "```" + `

**Step 2: Find a specific symbol.** Use find or grep across the mount:
` + "```" + `
find . -maxdepth 3 -name "HandleRequest" -type d    # Find by exact name
find . -path "*/methods/*" -name "*.Execute" -type d # Find all Execute methods
grep -rl "somePattern" --include="source" .          # Search inside source code
` + "```" + `

**Step 3: Understand a symbol.**
` + "```" + `
cat api/methods/Doer.executeInteraction/source    # Read the code
cat api/methods/Doer.executeInteraction/context   # See imports, types, struct definition
ls  api/methods/Doer.executeInteraction/callers/  # Who calls this?
ls  api/methods/Doer.executeInteraction/callees/  # What does this call?
` + "```" + `

**Step 4: Cross-reference.** Callers and callees link to other symbols:
` + "```" + `
ls api/functions/NewDoer/callers/
# Handler.getOrCreateDoer  TestBug_DoerTeleportationTab0  newDoerTestHarness  ...
# These are names you can look up in their own package:
cat api/methods/Handler.getOrCreateDoer/source
` + "```" + `

## Anti-Patterns (things that waste your iterations)

- **Do NOT cat a directory.** Use ls. Directories are symbol containers, not files.
- **Do NOT look for .go/.py/.ts files.** They do not exist. Code is in source files inside symbol directories.
- **Do NOT re-read context if you already have it.** The context file can be large. Read it once per symbol.
- **Do NOT ignore _project_files/.** Non-code files (configs, scripts, YAML) land here. If you need a config file, check _project_files/.
- **Do NOT guess paths.** Always ls a directory before cat-ing into it. Symbol names are case-sensitive and may include dots (e.g., Doer.Run).
- **Do NOT read _schema.json unless you need to understand the projection rules.** It describes HOW the filesystem was built, not WHAT is in it.

## Writing Code

%s

## Write-Back Workflow

1. **Read** the symbol: ` + "`cat {pkg}/functions/Foo/source`" + `
2. **Read context** if you need imports/types: ` + "`cat {pkg}/functions/Foo/context`" + `
3. **Write** the new source (the ENTIRE construct, not a diff):
   ` + "```" + `
   cat > {pkg}/functions/Foo/source << 'EOF'
   func Foo(ctx context.Context) error {
       // your new implementation
       return nil
   }
   EOF
   ` + "```" + `
4. **Check result**: ` + "`cat {pkg}/functions/Foo/_diagnostics/last-write-status`" + `
5. **If failed**: ` + "`cat {pkg}/functions/Foo/_diagnostics/ast-errors`" + ` — fix the syntax error and retry step 3.

**Key rules:**
- Write the COMPLETE construct (full function/method/type), not a partial diff.
- Tree-sitter validates syntax before the write lands. Bad syntax = draft saved, original untouched.
- The path never changes after a write. No need to re-navigate.
- Use printf or heredoc (cat << 'EOF') to avoid shell escaping issues with backticks and dollar signs.
- Trailing newlines in your write are normal — the splice handles them.

## Package-Level Diagnostics

Each package has a _diagnostics/ directory:
` + "```" + `
cat api/_diagnostics/lint        # "clean" or lint warnings
cat api/_diagnostics/ast-errors  # "no errors" or parse failures
` + "```" + `

## Quick Reference

| Task | Command |
|------|---------|
| List packages | ` + "`ls /`" + ` |
| List functions in a package | ` + "`ls {pkg}/functions/`" + ` |
| List methods in a package | ` + "`ls {pkg}/methods/`" + ` |
| List types in a package | ` + "`ls {pkg}/types/`" + ` |
| Read a function | ` + "`cat {pkg}/functions/Foo/source`" + ` |
| Read its imports and context | ` + "`cat {pkg}/functions/Foo/context`" + ` |
| Find who calls it | ` + "`ls {pkg}/functions/Foo/callers/`" + ` |
| Find what it calls | ` + "`ls {pkg}/functions/Foo/callees/`" + ` |
| Search all source code | ` + "`grep -rl 'pattern' --include=source .`" + ` |
| Find a symbol by name | ` + "`find . -maxdepth 3 -name 'SymbolName' -type d`" + ` |
| Check write status | ` + "`cat {pkg}/functions/Foo/_diagnostics/last-write-status`" + ` |
| View projection schema | ` + "`cat _schema.json`" + ` |

This is a POSIX filesystem. ls, cat, find, grep, and standard redirects all work.
`

// generateMountName creates a human-readable mount directory name.
// Format: basename-hash (e.g., "mono-a1b2c3" or "myorg-myrepo-a1b2c3")
func generateMountName(sourcePath, gitRepo string) string {
	var baseName string

	if gitRepo != "" {
		// Use org/repo from git if available
		baseName = strings.ReplaceAll(gitRepo, "/", "-")
	} else {
		// Use basename of source path
		baseName = filepath.Base(sourcePath)
	}

	// Add short hash for uniqueness
	hash := sha256.Sum256([]byte(sourcePath))
	shortHash := hex.EncodeToString(hash[:3])

	return fmt.Sprintf("%s-%s", baseName, shortHash)
}

// detectGitInfo extracts git repository information from a path.
// Returns (org/repo, branch, remoteURL, error).
func detectGitInfo(path string) (string, string, string, error) {
	// Check if path is in a git repo
	cmd := exec.Command("git", "-C", path, "rev-parse", "--show-toplevel")
	output, err := cmd.Output()
	if err != nil {
		return "", "", "", nil // Not a git repo, not an error
	}

	repoRoot := strings.TrimSpace(string(output))

	// Get current branch
	cmd = exec.Command("git", "-C", repoRoot, "rev-parse", "--abbrev-ref", "HEAD")
	branchOutput, err := cmd.Output()
	branch := "unknown"
	if err == nil {
		branch = strings.TrimSpace(string(branchOutput))
	}

	// Get remote URL
	cmd = exec.Command("git", "-C", repoRoot, "remote", "get-url", "origin")
	remoteOutput, err := cmd.Output()
	remoteURL := ""
	if err == nil {
		remoteURL = strings.TrimSpace(string(remoteOutput))
	}

	// Extract org/repo from remote URL
	// Handle both SSH (git@github.com:org/repo.git) and HTTPS (https://github.com/org/repo.git)
	orgRepo := ""
	if remoteURL != "" {
		if strings.HasPrefix(remoteURL, "git@") {
			// SSH format: git@github.com:org/repo.git
			parts := strings.Split(remoteURL, ":")
			if len(parts) == 2 {
				orgRepo = strings.TrimSuffix(parts[1], ".git")
			}
		} else if strings.Contains(remoteURL, "://") {
			// HTTPS format: https://github.com/org/repo.git
			parts := strings.Split(remoteURL, "/")
			if len(parts) >= 2 {
				org := parts[len(parts)-2]
				repo := strings.TrimSuffix(parts[len(parts)-1], ".git")
				orgRepo = fmt.Sprintf("%s/%s", org, repo)
			}
		}
	}

	return orgRepo, branch, remoteURL, nil
}

// getAgentMountsDir returns the directory where agent mounts are stored.
func getAgentMountsDir() (string, error) {
	tmpDir := os.TempDir()
	macheMountsDir := filepath.Join(tmpDir, "mache")
	if err := os.MkdirAll(macheMountsDir, 0o755); err != nil {
		return "", err
	}
	return macheMountsDir, nil
}

// sidecarPath returns the metadata sidecar path for a mount point.
// Stored beside the mount dir (not inside it) to avoid NFS conflicts.
func sidecarPath(mountPoint string) string {
	return mountPoint + ".meta.json"
}

// saveMountMetadata writes mount metadata to a sidecar file beside the mount point.
func saveMountMetadata(mountPoint string, meta *MountMetadata) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(sidecarPath(mountPoint), data, 0o644)
}

// loadMountMetadata reads mount metadata from the sidecar file.
func loadMountMetadata(mountPoint string) (*MountMetadata, error) {
	data, err := os.ReadFile(sidecarPath(mountPoint))
	if err != nil {
		return nil, err
	}
	var meta MountMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// generatePromptContent creates the PROMPT.txt content for agents.
func generatePromptContent(meta *MountMetadata) []byte {
	gitInfo := "Not a git repository"
	if meta.GitRepo != "" {
		gitInfo = fmt.Sprintf("%s (branch: %s)", meta.GitRepo, meta.GitBranch)
	}

	mode := "Read-only"
	if meta.Writable {
		mode = "Writable (snapshot sandbox)"
	}

	snapshotInfo := ""
	if meta.Writable {
		snapshotInfo = `
**Sandbox:** You are working on an isolated snapshot copy. Edits here do NOT
touch the original source. On unmount, you will be shown how to apply or
discard your changes.`
	}

	writeInfo := `**Read-only mode.** This mount is not writable. You can read and navigate
but cannot edit source files.`
	if meta.Writable {
		writeInfo = `**Write-back enabled.** Edit source files and your changes will:
1. Validate via tree-sitter (syntax check — broken code never lands)
2. Format automatically (gofumpt for Go, hclwrite for HCL/Terraform)
3. Splice into the source file (only the changed construct, not the whole file)
4. Update the graph immediately (no re-ingestion, path stays stable)

If your edit has a syntax error, it saves as a draft. Check
_diagnostics/ast-errors to see the parse error, fix it, and retry.`
	}

	content := fmt.Sprintf(agentPromptTemplate,
		meta.Source,
		gitInfo,
		meta.MountPoint,
		mode,
		snapshotInfo,
		writeInfo,
	)

	return []byte(content)
}

// listActiveMounts finds all active mache mounts by scanning sidecar files in /tmp/mache.
func listActiveMounts() ([]*MountMetadata, error) {
	mountsDir, err := getAgentMountsDir()
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(mountsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var mounts []*MountMetadata
	for _, entry := range entries {
		name := entry.Name()
		// Look for sidecar files: <name>.meta.json
		if !strings.HasSuffix(name, ".meta.json") {
			continue
		}

		metaPath := filepath.Join(mountsDir, name)
		data, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}
		var meta MountMetadata
		if err := json.Unmarshal(data, &meta); err != nil {
			continue
		}

		mounts = append(mounts, &meta)
	}

	return mounts, nil
}

// isProcessRunning checks if a process with the given PID is running.
func isProcessRunning(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, FindProcess always succeeds. Send signal 0 to check if alive.
	err = process.Signal(syscall.Signal(0))
	return err == nil
}

// runAgentMode handles the --agent flag workflow.
// Returns the mount point and metadata that should be used.
func runAgentMode(cmd *cobra.Command) error {
	// Validate data path
	if dataPath == "" {
		return fmt.Errorf("--data/-d required in agent mode")
	}

	// Auto-enable inference, writable, and snapshot in agent mode.
	// Snapshot ensures the agent operates on a copy, not the live source.
	// User can override with explicit --snapshot=false.
	if !inferSchema && schemaPath == "" {
		inferSchema = true
	}
	if !writable {
		writable = true
	}
	if !snapshot && !cmd.Flags().Changed("snapshot") {
		snapshot = true
	}

	// Resolve data path
	absDataPath, err := filepath.Abs(dataPath)
	if err != nil {
		return fmt.Errorf("failed to resolve data path: %w", err)
	}

	// Update dataPath to absolute
	dataPath = absDataPath

	// Detect git info
	gitRepo, gitBranch, gitRemote, _ := detectGitInfo(absDataPath)

	// Generate mount directory name
	mountsDir, err := getAgentMountsDir()
	if err != nil {
		return err
	}
	mountName := generateMountName(absDataPath, gitRepo)
	agentMountPoint := filepath.Join(mountsDir, mountName)

	// Create metadata that will be saved after mount succeeds
	agentMetadata = &MountMetadata{
		PID:        os.Getpid(),
		Source:     absDataPath,
		MountPoint: agentMountPoint,
		GitRepo:    gitRepo,
		GitBranch:  gitBranch,
		GitRemote:  gitRemote,
		Timestamp:  time.Now(),
		Writable:   writable,
	}

	// Print what we're about to do
	gitInfo := ""
	if gitRepo != "" {
		gitInfo = fmt.Sprintf(" (%s@%s)", gitRepo, gitBranch)
	}

	log.Printf("Agent Mode")
	log.Printf("----------")
	log.Printf("Source: %s%s", absDataPath, gitInfo)
	log.Printf("Mount: %s", agentMountPoint)
	log.Printf("Writable: %v", writable)
	log.Printf("PID: %d", os.Getpid())

	// Return nil to continue with normal mount flow
	// The actual mount will happen in rootCmd, and we'll save metadata after success
	return nil
}
