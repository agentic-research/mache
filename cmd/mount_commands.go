package cmd

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/agentic-research/mache/internal/mountmeta"
	"github.com/agentic-research/mache/internal/nfsmount"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List active mache instances (mounts and MCP servers)",
	RunE: func(cmd *cobra.Command, args []string) error {
		mounts, err := mountmeta.ListActiveMounts()
		if err != nil {
			return err
		}

		if len(mounts) == 0 {
			fmt.Println("No active mache instances found.")
			return nil
		}

		fmt.Printf("%-20s %-12s %-10s %-40s %s\n", "NAME", "TYPE", "PID", "SOURCE", "STATUS")
		fmt.Println(strings.Repeat("-", 100))

		for _, meta := range mounts {
			name := filepath.Base(meta.MountPoint)
			status := "running"
			if !mountmeta.IsProcessRunning(meta.PID) {
				status = "stale"
			}
			typ := meta.Type
			if typ == "" {
				typ = "mount" // backwards compat for old sidecars
			}
			source := meta.Source
			if meta.Addr != "" {
				source = meta.Addr + " " + source
			}
			fmt.Printf("%-20s %-12s %-10d %-40s %s\n", name, typ, meta.PID, source, status)
		}

		return nil
	},
}

var unmountCmd = &cobra.Command{
	Use:   "unmount <mount-name>",
	Short: "Unmount and stop a mache mount",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		mountName := args[0]

		// Resolve mount point (support both short name and full path)
		var mountPoint string
		if filepath.IsAbs(mountName) {
			mountPoint = mountName
		} else {
			mountsDir, err := mountmeta.AgentMountsDir()
			if err != nil {
				return err
			}
			mountPoint = filepath.Join(mountsDir, mountName)
		}

		// Load metadata. The sidecar exists for agent-mode and serve-
		// mode mounts; direct `mache <path>` and ley-line --control
		// mounts have no sidecar. Don't fail when it's missing —
		// unmount the kernel-side NFS first regardless, then clean up
		// what we can (mache-fsi: 'No agent metadata' case).
		meta, metaErr := mountmeta.LoadMountMetadata(mountPoint)

		isMCPServe := meta != nil && (meta.Type == "mcp-http" || meta.Type == "mcp-stdio")

		// Step 1: kernel-side unmount FIRST (for NFS mounts only).
		// SIGTERM-then-RemoveAll without an explicit umount races —
		// if the daemon doesn't finish its own unmount before
		// RemoveAll runs, we leave a stuck NFS mount that only
		// `umount -f` can clear (mache-fsi: 'Race' + 'Orphaned mount'
		// cases). Unmount is idempotent — repeating on an already-
		// unmounted point logs a warning and we continue.
		if !isMCPServe {
			if err := nfsmount.Unmount(mountPoint); err != nil {
				log.Printf("Note: nfsmount.Unmount(%s): %v (continuing)", mountPoint, err)
			}
		}

		// Step 2: SIGTERM the owning process (only when we have its
		// PID via the sidecar). For control / sidecar-less mounts
		// the user is expected to stop the daemon separately.
		if meta != nil && mountmeta.IsProcessRunning(meta.PID) {
			process, err := os.FindProcess(meta.PID)
			if err == nil {
				log.Printf("Stopping mache process (PID %d)...", meta.PID)
				_ = process.Signal(syscall.SIGTERM)

				// Wait briefly for graceful shutdown
				time.Sleep(2 * time.Second)

				if mountmeta.IsProcessRunning(meta.PID) {
					log.Printf("Process still running, sending SIGKILL...")
					_ = process.Signal(syscall.SIGKILL)
				}
			} else {
				log.Printf("Note: failed to find process %d: %v", meta.PID, err)
			}
		}

		// Step 3: clean up mount directory and sidecar.
		log.Printf("Removing mount directory: %s", mountPoint)
		if err := os.RemoveAll(mountPoint); err != nil {
			return fmt.Errorf("failed to remove mount directory: %w", err)
		}
		_ = os.Remove(mountmeta.SidecarPath(mountPoint))

		if metaErr != nil {
			log.Printf("Mount stopped (no sidecar metadata; resolved by direct unmount).")
		} else {
			log.Println("Mount stopped successfully.")
		}
		return nil
	},
}

var cleanCmd = &cobra.Command{
	Use:   "clean",
	Short: "Remove stale mache mounts and orphaned snapshots",
	RunE: func(cmd *cobra.Command, args []string) error {
		mounts, err := mountmeta.ListActiveMounts()
		if err != nil {
			return err
		}

		cleaned := 0
		for _, meta := range mounts {
			if !mountmeta.IsProcessRunning(meta.PID) {
				log.Printf("Removing stale mount: %s (PID %d was not running)",
					filepath.Base(meta.MountPoint), meta.PID)
				if err := os.RemoveAll(meta.MountPoint); err != nil {
					log.Printf("Warning: failed to remove %s: %v", meta.MountPoint, err)
				} else {
					_ = os.Remove(mountmeta.SidecarPath(meta.MountPoint))
					cleaned++
				}
			}
		}

		// Clean orphaned snapshots (snap-<PID>-* where PID is dead)
		snapDir := filepath.Join(os.TempDir(), "mache", "snapshots")
		if entries, err := os.ReadDir(snapDir); err == nil {
			for _, entry := range entries {
				name := entry.Name()
				if !strings.HasPrefix(name, "snap-") {
					continue
				}
				// Parse PID from snap-<PID>-<name>
				parts := strings.SplitN(strings.TrimPrefix(name, "snap-"), "-", 2)
				if len(parts) < 2 {
					continue
				}
				var pid int
				if _, err := fmt.Sscanf(parts[0], "%d", &pid); err != nil {
					continue
				}
				if !mountmeta.IsProcessRunning(pid) {
					snapPath := filepath.Join(snapDir, name)
					log.Printf("Removing orphaned snapshot: %s (PID %d was not running)", name, pid)
					if err := os.RemoveAll(snapPath); err != nil {
						log.Printf("Warning: failed to remove %s: %v", snapPath, err)
					} else {
						cleaned++
					}
				}
			}
		}

		if cleaned == 0 {
			log.Println("No stale mounts or orphaned snapshots found.")
		} else {
			log.Printf("Cleaned %d stale item(s).", cleaned)
		}

		return nil
	},
}
