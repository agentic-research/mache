package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/agentic-research/mache/api"
	machefs "github.com/agentic-research/mache/internal/fs"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/ingest"
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

		// 2. Load Schema
		var schema *api.Topology
		if s, err := os.ReadFile(schemaPath); err == nil {
			fmt.Printf("Loaded schema from %s\n", schemaPath)
			schema = &api.Topology{}
			if err := json.Unmarshal(s, schema); err != nil {
				return fmt.Errorf("failed to parse schema: %w", err)
			}
		} else {
			if cmd.Flags().Changed("schema") {
				return fmt.Errorf("failed to read schema file: %w", err)
			}
			fmt.Println("No schema found, using default empty schema.")
			schema = &api.Topology{Version: "v1alpha1"}
		}

		// 3. Create the Graph backend
		var g graph.Graph

		if _, err := os.Stat(dataPath); err == nil {
			if filepath.Ext(dataPath) == ".db" {
				// SQLite source: direct read, no ingestion, instant mount
				fmt.Printf("Opening %s (direct SQL backend)...\n", dataPath)
				sg, err := graph.OpenSQLiteGraph(dataPath, schema, ingest.RenderTemplate)
				if err != nil {
					return fmt.Errorf("open sqlite graph: %w", err)
				}
				defer func() { _ = sg.Close() }()
				g = sg
			} else {
				// Non-DB source: use MemoryStore + ingestion pipeline
				store := graph.NewMemoryStore()
				resolver := ingest.NewSQLiteResolver()
				defer resolver.Close()
				store.SetResolver(resolver.Resolve)

				fmt.Printf("Ingesting data from %s...\n", dataPath)
				start := time.Now()
				engine := ingest.NewEngine(schema, store)
				if err := engine.Ingest(dataPath); err != nil {
					return fmt.Errorf("ingestion failed: %w", err)
				}
				fmt.Printf("Ingestion complete in %v\n", time.Since(start))
				g = store
			}
		} else {
			if cmd.Flags().Changed("data") {
				return fmt.Errorf("data path not found: %s", dataPath)
			}
			fmt.Printf("No data found at %s, starting empty.\n", dataPath)
			g = graph.NewMemoryStore()
		}

		// 4. Create the FS, injecting the Graph backend
		macheFs := machefs.NewMacheFS(schema, g)

		// 6. Host it
		host := fuse.NewFileSystemHost(macheFs)
		host.SetCapReaddirPlus(true)

		fmt.Printf("Mounting mache at %s (using fuse-t/cgofuse)...\n", mountPoint)

		// 7. Mount passes control to the library.
		// nobrowse: hide from Finder sidebar & prevent Spotlight auto-indexing
		opts := []string{
			"-o", "ro",
			"-o", fmt.Sprintf("uid=%d", os.Getuid()),
			"-o", fmt.Sprintf("gid=%d", os.Getgid()),
			"-o", "fsname=mache",
			"-o", "subtype=mache",
			"-o", "nobrowse",
			"-o", "entry_timeout=60.0",
			"-o", "attr_timeout=60.0",
			"-o", "negative_timeout=60.0",
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
