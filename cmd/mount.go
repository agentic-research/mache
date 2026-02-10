package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/agentic-research/mache/api"
	machefs "github.com/agentic-research/mache/internal/fs"
	"github.com/spf13/cobra"
	"github.com/winfsp/cgofuse/fuse"
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
		// Mocked for now to match the "Hello World" requirement
		var schema *api.Topology
		
		// Try loading real schema (mock decoding for now as we just need the struct)
		if s, err := os.ReadFile(schemaPath); err == nil {
			fmt.Printf("Loaded schema from %s\n", schemaPath)
			schema = &api.Topology{Version: "loaded-from-file"}
			_ = s // unused until unmarshal implemented
		} else {
			if cmd.Flags().Changed("schema") {
				return fmt.Errorf("failed to read schema file: %w", err)
			}
			fmt.Println("No schema found, using mock topology.")
			schema = &api.Topology{Version: "v1alpha1"}
		}

		// 3. Create the FS
		macheFs := machefs.NewMacheFS(schema)
		
		// 4. Host it
		host := fuse.NewFileSystemHost(macheFs)
		
		fmt.Printf("Mounting mache at %s (using fuse-t/cgofuse)...\n", mountPoint)
		
		// 5. Mount passes control to the library.
		// Use -o ro (Read Only)
		// Use -o uid=N,gid=N to ensure we own the mount (critical for fuse-t/NFS)
		opts := []string{
			"-o", "ro",
			"-o", fmt.Sprintf("uid=%d", os.Getuid()),
			"-o", fmt.Sprintf("gid=%d", os.Getgid()),
		}

		if !host.Mount(mountPoint, opts) {
			return fmt.Errorf("mount failed")
		}
		
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