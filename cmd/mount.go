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
	"github.com/agentic-research/mache/internal/lattice"
	"github.com/spf13/cobra"
	"github.com/winfsp/cgofuse/fuse"
)

var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

var (
	schemaPath  string
	dataPath    string
	writable    bool
	inferSchema bool
	quiet       bool
)

func init() {
	rootCmd.Flags().StringVarP(&schemaPath, "schema", "s", "", "Path to topology schema")
	rootCmd.Flags().StringVarP(&dataPath, "data", "d", "", "Path to data source")
	rootCmd.Flags().BoolVarP(&writable, "writable", "w", false, "Enable write-back (splice edits into source files)")
	rootCmd.Flags().BoolVar(&inferSchema, "infer", false, "Auto-infer schema from data via FCA")
	rootCmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Suppress standard output")
	rootCmd.AddCommand(versionCmd)
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version number",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("mache version %s (commit %s, built %s)\n", Version, Commit, Date)
	},
}

var rootCmd = &cobra.Command{
	Use:   "mache [mountpoint]",
	Short: "Mache: The Universal Semantic Overlay Engine",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if quiet {
			f, err := os.Open(os.DevNull)
			if err == nil {
				os.Stdout = f
			}
		}

		mountPoint := args[0]

		// 1. Resolve Configuration Paths
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("failed to get home dir: %w", err)
		}
		defaultDir := filepath.Join(home, ".mache")

		if schemaPath == "" {
			schemaPath = filepath.Join(defaultDir, "mache.json")
		}
		if dataPath == "" {
			dataPath = filepath.Join(defaultDir, "data.json")
		}

		// 2. Load Schema (or infer from data)
		var schema *api.Topology
		if inferSchema && filepath.Ext(dataPath) == ".db" {
			fmt.Print("Inferring schema from data via FCA...")
			start := time.Now()
			inf := &lattice.Inferrer{Config: lattice.DefaultInferConfig()}
			inferred, err := inf.InferFromSQLite(dataPath)
			if err != nil {
				return fmt.Errorf("schema inference failed: %w", err)
			}
			schema = inferred
			fmt.Printf(" done in %v\n", time.Since(start))
			// Write inferred schema if --schema path was provided
			if cmd.Flags().Changed("schema") {
				data, _ := json.MarshalIndent(schema, "", "  ")
				if err := os.WriteFile(schemaPath, data, 0o644); err != nil {
					return fmt.Errorf("write inferred schema: %w", err)
				}
				fmt.Printf("Inferred schema written to %s\n", schemaPath)
			}
		} else if s, err := os.ReadFile(schemaPath); err == nil {
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
		var engine *ingest.Engine // non-nil for MemoryStore paths (needed for write-back)

		if _, err := os.Stat(dataPath); err == nil {
			if filepath.Ext(dataPath) == ".db" {
				// SQLite source: eager scan before mount to avoid fuse-t NFS timeouts
				fmt.Printf("Opening %s (direct SQL backend)...\n", dataPath)
				sg, err := graph.OpenSQLiteGraph(dataPath, schema, ingest.RenderTemplate)
				if err != nil {
					return fmt.Errorf("open sqlite graph: %w", err)
				}
				defer func() { _ = sg.Close() }() // safe to ignore
				start := time.Now()
				fmt.Print("Scanning records...")
				if err := sg.EagerScan(); err != nil {
					return fmt.Errorf("scan failed: %w", err)
				}
				fmt.Printf(" done in %v\n", time.Since(start))
				g = sg
			} else {
				// Non-DB source: use MemoryStore + ingestion pipeline
				store := graph.NewMemoryStore()
				resolver := ingest.NewSQLiteResolver()
				defer resolver.Close()
				store.SetResolver(resolver.Resolve)

				fmt.Printf("Ingesting data from %s...\n", dataPath)
				start := time.Now()
				engine = ingest.NewEngine(schema, store)
				if err := engine.Ingest(dataPath); err != nil {
					return fmt.Errorf("ingestion failed: %w", err)
				}
				fmt.Printf("Ingestion complete in %v\n", time.Since(start))

				// Enable SQL query support for MemoryStore
				if err := store.InitRefsDB(); err != nil {
					return fmt.Errorf("init refs db: %w", err)
				}
				defer func() { _ = store.Close() }() // safe to ignore
				if err := store.FlushRefs(); err != nil {
					fmt.Printf("Warning: refs flush failed: %v\n", err)
				}

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

		// Wire up query directory (enables /.query/ magic dir for both backends)
		if sg, ok := g.(*graph.SQLiteGraph); ok {
			macheFs.SetQueryFunc(sg.QueryRefs)
		} else if ms, ok := g.(*graph.MemoryStore); ok {
			macheFs.SetQueryFunc(ms.QueryRefs)
		}

		// Wire up write-back if requested (only for MemoryStore + tree-sitter sources)
		if writable && engine != nil {
			macheFs.Writable = true
			macheFs.Engine = engine
			fmt.Println("Write-back enabled: edits will splice into source files.")
		} else if writable {
			fmt.Println("Warning: --writable ignored (only supported for non-.db sources)")
		}

		// 5. Host it
		host := fuse.NewFileSystemHost(macheFs)
		host.SetCapReaddirPlus(true)

		fmt.Printf("Mounting mache at %s (using fuse-t/cgofuse)...\n", mountPoint)

		// 6. Mount passes control to the library.
		// nobrowse: hide from Finder sidebar & prevent Spotlight auto-indexing
		opts := []string{
			"-o", fmt.Sprintf("uid=%d", os.Getuid()),
			"-o", fmt.Sprintf("gid=%d", os.Getgid()),
			"-o", "fsname=mache",
			"-o", "subtype=mache",
			"-o", "nobrowse",
			"-o", "entry_timeout=300.0",
			"-o", "attr_timeout=300.0",
			"-o", "negative_timeout=300.0",
		}
		if !macheFs.Writable {
			opts = append([]string{"-o", "ro"}, opts...)
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
