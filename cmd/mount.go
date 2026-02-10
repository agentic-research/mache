package cmd

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/agentic-research/mache/api"
	machefs "github.com/agentic-research/mache/internal/fs"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/spf13/cobra"
)

var (
	schemaPath string
	dataPath   string
)

func init() {
	rootCmd.Flags().StringVarP(&schemaPath, "schema", "s", "", "Path to topology schema")
	rootCmd.Flags().StringVarP(&dataPath, "data", "d", "", "Path to data source")
}

var rootCmd = &cobra.Command{
	Use:   "mache [mountpoint]",
	Short: "Mache: The Universal Semantic Overlay Engine",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		mountPoint := args[0]

		// 1. Resolve Configuration Paths
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("failed to get home dir: %w", err)
		}
		defaultDir := filepath.Join(home, ".agentic-research", "mache")

		if schemaPath == "" {
			schemaPath = filepath.Join(defaultDir, "mache.json")
		}
		if dataPath == "" {
			dataPath = filepath.Join(defaultDir, "data.json")
		}

		// 2. Load Data & Schema
		var source []byte
		var schema *api.Topology

		// Try loading real data
		if d, err := os.ReadFile(dataPath); err == nil {
			fmt.Printf("Loaded data from %s\n", dataPath)
			source = d
		} else {
			if cmd.Flags().Changed("data") {
				// User explicitly provided a flag that failed
				return fmt.Errorf("failed to read data file: %w", err)
			}
			// Fallback
			fmt.Println("No data found, using mock source.")
			source = []byte(`{"message": "hello"}`)
		}

		// Try loading real schema (mock decoding for now as we just need the struct)
		if s, err := os.ReadFile(schemaPath); err == nil {
			fmt.Printf("Loaded schema from %s\n", schemaPath)
			// TODO: Actual JSON unmarshaling when we implement the full engine.
			// For now, we just acknowledge the file exists.
			schema = &api.Topology{Version: "loaded-from-file"}
			_ = s // unused until unmarshal implemented
		} else {
			if cmd.Flags().Changed("schema") {
				return fmt.Errorf("failed to read schema file: %w", err)
			}
			fmt.Println("No schema found, using mock topology.")
			schema = &api.Topology{Version: "v1alpha1"}
		}

		// 3. Create the Root Inode
		root := machefs.NewMacheRoot(source, schema)

		// 4. Configure FUSE Options
		// We prioritize performance and correct behavior on macOS/FUSE-T.
		opts := &fs.Options{
			MountOptions: fuse.MountOptions{
				// ReadOnly: Mache is strictly a projection engine. Writing is not supported.
				// This also allows the kernel to aggressively cache reads.
				Options: []string{"ro"},
				
				// Set the filesystem name for display in `df` or Finder.
				FsName: "mache",
				
				// SyncRead: standard for read-only.
				// Debug: enable via flag if needed, kept off for performance.
			},
			// CacheDuration: Aggressively cache attributes for performance since content is immutable during mount.
			// 1 second is a safe default; can be increased for static datasets.
			EntryTimeout: &[]time.Duration{time.Second}[0],
			AttrTimeout:  &[]time.Duration{time.Second}[0],
		}

		// 4. Mount
		// We use fs.Mount which sets up the Node API server.
		fmt.Printf("Mounting mache at %s...\n", mountPoint)
		server, err := fs.Mount(mountPoint, root, opts)
		if err != nil {
			return fmt.Errorf("mount failed: %w", err)
		}

		// 5. Handle Signals (Graceful Unmount)
		// Crucial for FUSE to avoid "endpoint not connected" or phantom mounts on macOS.
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-c
			fmt.Println("\nReceived signal, unmounting...")
			if err := server.Unmount(); err != nil {
				log.Printf("Unmount failed: %v", err)
			}
		}()

		// 6. Block until unmount
		server.Wait()
		fmt.Println("Unmounted successfully.")
		return nil
	},
}

// Execute runs the root command.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
