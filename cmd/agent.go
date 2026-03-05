package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	GitRepo    string    `json:"git_repo,omitempty"`   // org/repo
	GitBranch  string    `json:"git_branch,omitempty"` // branch name
	GitRemote  string    `json:"git_remote,omitempty"` // full remote URL
	Timestamp  time.Time `json:"timestamp"`
	Writable   bool      `json:"writable"`
}

// agentPromptTemplate is the instruction file generated for LLM agents.
const agentPromptTemplate = `# Mache Agent Environment

This filesystem is a **semantic projection** of your codebase. Every function,
type, and class is a directory. You navigate code by meaning, not by file path.

**This is how you should read and write code in this environment.**

## Mount Info

**Source:** %s
**Git:** %s
**Mount:** %s
**Mode:** %s
%s

## How This Filesystem Works

Each code construct (function, type, method, class) is a **directory** containing:

| File | What it contains | Writable? |
|------|-----------------|-----------|
| source | The actual code — just this construct, nothing else | Yes (if writable mode) |
| context | Imports, types, globals visible to this scope | No |
| callers/ | Directory of functions that **call** this one | No |
| callees/ | Directory of functions this one **calls** | No |
| _diagnostics/ | Write status, AST errors, lint output | No |

## Do This, Not That

**Do:** Read individual constructs via their source file.
` + "```" + `
ls                           # See top-level categories
ls functions/                # List all functions
cat functions/HandleRequest/source   # Read just this function
cat functions/HandleRequest/context  # See its imports and types
` + "```" + `

**Don't:** Try to cat entire .go/.py files — they don't exist here. The
codebase has been decomposed into semantic units. Each source file contains
exactly the code for one construct.

**Do:** Use callers/ and callees/ to understand relationships.
` + "```" + `
ls functions/HandleRequest/callers/  # Who calls this?
ls functions/HandleRequest/callees/  # What does it call?
cat functions/HandleRequest/callers/* # Read all calling code
` + "```" + `

**Do:** Start with ls at the root to understand the structure.
` + "```" + `
ls /            # Top-level: _schema.json, PROMPT.txt, and category dirs
cat _schema.json   # See the full schema driving this projection
` + "```" + `

## Writing Code

%s

## Editing Workflow

1. Read the function: ` + "`cat functions/Foo/source`" + `
2. Read its context: ` + "`cat functions/Foo/context`" + ` (imports, types it uses)
3. Edit the source file (only the function body — not a full file)
4. Check the result: ` + "`cat functions/Foo/_diagnostics/last-write-status`" + `
5. If it failed: ` + "`cat functions/Foo/_diagnostics/ast-errors`" + ` to see why

Writes are validated by tree-sitter before they touch any file. If your edit
has a syntax error, it saves as a draft and the original code is untouched.
The path to the construct never changes — no re-navigation needed after edits.

## Quick Reference

| Task | Command |
|------|---------|
| List all functions | ` + "`ls functions/`" + ` |
| Read a function | ` + "`cat functions/Foo/source`" + ` |
| See what it imports | ` + "`cat functions/Foo/context`" + ` |
| Find callers | ` + "`ls functions/Foo/callers/`" + ` |
| Find callees | ` + "`ls functions/Foo/callees/`" + ` |
| Check write status | ` + "`cat functions/Foo/_diagnostics/last-write-status`" + ` |
| View schema | ` + "`cat _schema.json`" + ` |

This is a real POSIX filesystem. Use cd, ls, cat, find, grep — whatever you
normally use. No SDK, no special commands.
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

	fmt.Printf("Agent Mode\n")
	fmt.Printf("----------\n")
	fmt.Printf("Source: %s%s\n", absDataPath, gitInfo)
	fmt.Printf("Mount: %s\n", agentMountPoint)
	fmt.Printf("Writable: %v\n", writable)
	fmt.Printf("PID: %d\n\n", os.Getpid())

	// Return nil to continue with normal mount flow
	// The actual mount will happen in rootCmd, and we'll save metadata after success
	return nil
}
