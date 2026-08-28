// Package mountmeta owns the mount-metadata sidecar files: which mache
// mounts/servers exist on this machine, written beside each mount point as
// <mount>.meta.json. Re-homed from cmd/agent.go (R4, mache-96c378): the MCP
// serve path registers its sidecar here too, and serve reaching upward into
// cmd for it was the scored cmd↔mcpserve import cycle. Process/sidecar state,
// not project config — deliberately NOT part of internal/projcfg, and it
// imports neither cmd nor projcfg.
package mountmeta

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

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

// AgentMountsDir returns (creating if needed) the shared mounts directory.
func AgentMountsDir() (string, error) {
	tmpDir := os.TempDir()
	macheMountsDir := filepath.Join(tmpDir, "mache")
	if err := os.MkdirAll(macheMountsDir, 0o755); err != nil {
		return "", err
	}
	return macheMountsDir, nil
}

// SidecarPath returns the metadata sidecar path for a mount point.
// Stored beside the mount dir (not inside it) to avoid NFS conflicts.
func SidecarPath(mountPoint string) string {
	return mountPoint + ".meta.json"
}

// SaveMountMetadata writes mount metadata to a sidecar file beside the mount point.
func SaveMountMetadata(mountPoint string, meta *MountMetadata) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(SidecarPath(mountPoint), data, 0o644)
}

// LoadMountMetadata reads mount metadata from the sidecar file.
func LoadMountMetadata(mountPoint string) (*MountMetadata, error) {
	data, err := os.ReadFile(SidecarPath(mountPoint))
	if err != nil {
		return nil, err
	}
	var meta MountMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// ListActiveMounts returns metadata for every sidecar in the mounts dir.
// Unreadable or unparseable sidecars are skipped, not fatal — a stale or
// half-written file must not hide the healthy mounts.
func ListActiveMounts() ([]*MountMetadata, error) {
	mountsDir, err := AgentMountsDir()
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

// IsProcessRunning checks if a process with the given PID is running.
func IsProcessRunning(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, FindProcess always succeeds. Send signal 0 to check if alive.
	err = process.Signal(syscall.Signal(0))
	return err == nil
}
