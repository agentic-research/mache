package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/control"
	machefs "github.com/agentic-research/mache/internal/fs"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/lattice"
	"github.com/agentic-research/mache/internal/linter"
	"github.com/agentic-research/mache/internal/nfsmount"
	"github.com/agentic-research/mache/internal/writeback"
	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/hcl"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/rust"
	"github.com/smacker/go-tree-sitter/sql"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
	"github.com/smacker/go-tree-sitter/yaml"
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
	controlPath string
	writable    bool
	inferSchema bool
	quiet       bool
	backend     string
)

func init() {
	rootCmd.Flags().StringVarP(&schemaPath, "schema", "s", "", "Path to topology schema")
	rootCmd.Flags().StringVarP(&dataPath, "data", "d", "", "Path to data source")
	rootCmd.Flags().StringVar(&controlPath, "control", "", "Path to Leyline control block (enables hot-swap)")
	rootCmd.Flags().BoolVarP(&writable, "writable", "w", false, "Enable write-back (splice edits into source files)")
	rootCmd.Flags().BoolVar(&inferSchema, "infer", false, "Auto-infer schema from data via FCA")
	rootCmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Suppress standard output")

	defaultBackend := "fuse"
	if runtime.GOOS == "darwin" {
		defaultBackend = "nfs"
	}
	rootCmd.Flags().StringVar(&backend, "backend", defaultBackend, "Mount backend: nfs or fuse")

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
		if inferSchema {
			inf := &lattice.Inferrer{Config: lattice.DefaultInferConfig()}
			var inferred *api.Topology
			var err error
			ext := filepath.Ext(dataPath)

			switch ext {
			case ".db":
				fmt.Print("Inferring schema from SQLite data via FCA...")
				start := time.Now()
				inferred, err = inf.InferFromSQLite(dataPath)
				fmt.Printf(" done in %v\n", time.Since(start))
			case ".js":
				inferred, err = inferFromTreeSitterFile(inf, dataPath, javascript.GetLanguage(), "JavaScript")
			case ".ts", ".tsx":
				inferred, err = inferFromTreeSitterFile(inf, dataPath, typescript.GetLanguage(), "TypeScript")
			case ".sql":
				inferred, err = inferFromTreeSitterFile(inf, dataPath, sql.GetLanguage(), "SQL")
			case ".py":
				inferred, err = inferFromTreeSitterFile(inf, dataPath, python.GetLanguage(), "Python")
			case ".go":
				inferred, err = inferFromTreeSitterFile(inf, dataPath, golang.GetLanguage(), "Go")
			case ".tf", ".hcl":
				inferred, err = inferFromTreeSitterFile(inf, dataPath, hcl.GetLanguage(), "HCL/Terraform")
			case ".yaml", ".yml":
				inferred, err = inferFromTreeSitterFile(inf, dataPath, yaml.GetLanguage(), "YAML")
			case ".rs":
				inferred, err = inferFromTreeSitterFile(inf, dataPath, rust.GetLanguage(), "Rust")
			case ".git":
				fmt.Print("Loading git commits...")
				start := time.Now()
				recs, readErr := ingest.LoadGitCommits(dataPath)
				if readErr != nil {
					err = readErr
					fmt.Printf(" failed in %v\n", time.Since(start))
				} else {
					fmt.Printf(" done (%d commits) in %v\n", len(recs), time.Since(start))
					fmt.Print("Inferring schema from Git history (Greedy)...")
					start = time.Now()
					// Enable Git hints
					inf.Config.Hints = ingest.GetGitHints()
					inferred, err = inf.InferFromRecords(recs)
				}
				fmt.Printf(" done in %v\n", time.Since(start))
			default:
				// Check if it's a directory
				info, errStat := os.Stat(dataPath)
				if errStat == nil && info.IsDir() {
					fmt.Printf("Inferring schema from directory %s...\n", dataPath)
					start := time.Now()
					var allRecords []any

					walkErr := filepath.Walk(dataPath, func(path string, info os.FileInfo, err error) error {
						if err != nil {
							return err
						}
						if info.IsDir() {
							if strings.HasPrefix(filepath.Base(path), ".") && path != dataPath {
								return filepath.SkipDir // Skip hidden dirs like .git
							}
							return nil
						}

						ext := filepath.Ext(path)
						var lang *sitter.Language

						switch ext {
						case ".go":
							lang = golang.GetLanguage()
						case ".js":
							lang = javascript.GetLanguage()
						case ".py":
							lang = python.GetLanguage()
						case ".ts", ".tsx":
							lang = typescript.GetLanguage()
						case ".sql":
							lang = sql.GetLanguage()
						case ".tf", ".hcl":
							lang = hcl.GetLanguage()
						case ".yaml", ".yml":
							lang = yaml.GetLanguage()
						case ".rs":
							lang = rust.GetLanguage()
						default:
							return nil // Skip unsupported files
						}

						content, readErr := os.ReadFile(path)
						if readErr != nil {
							fmt.Printf("Warning: skipping unreadable file %s: %v\n", path, readErr)
							return nil
						}

						parser := sitter.NewParser()
						parser.SetLanguage(lang)
						tree, _ := parser.ParseCtx(context.Background(), nil, content)
						if tree != nil {
							records := ingest.FlattenAST(tree.RootNode())
							allRecords = append(allRecords, records...)
						}
						return nil
					})

					if walkErr != nil {
						err = fmt.Errorf("walk failed: %w", walkErr)
					} else if len(allRecords) == 0 {
						err = fmt.Errorf("no supported source files found in %s", dataPath)
					} else {
						// Use FCA for ASTs (same logic as InferFromTreeSitter)
						saved := inf.Config.Method
						inf.Config.Method = "fca"
						inferred, err = inf.InferFromRecords(allRecords)
						inf.Config.Method = saved
						fmt.Printf(" done (%d records) in %v\n", len(allRecords), time.Since(start))
					}
				} else {
					err = fmt.Errorf("automatic inference not supported for %s", ext)
				}
			}

			if err != nil {
				return fmt.Errorf("schema inference failed: %w", err)
			}
			schema = inferred

			// Write inferred schema if --schema path was provided explicitly (not default)
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

		if controlPath != "" {
			return mountControl(controlPath, schema, mountPoint, backend)
		}

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

				engine = ingest.NewEngine(schema, store)

				if filepath.Ext(dataPath) == ".git" {
					fmt.Printf("Ingesting git history from %s...\n", dataPath)
					start := time.Now()
					recs, err := ingest.LoadGitCommits(dataPath)
					if err != nil {
						return fmt.Errorf("load git: %w", err)
					}
					if err := engine.IngestRecords(recs); err != nil {
						return fmt.Errorf("ingest git records: %w", err)
					}
					fmt.Printf("Ingestion complete in %v\n", time.Since(start))
				} else {
					fmt.Printf("Ingesting data from %s...\n", dataPath)
					start := time.Now()
					if err := engine.Ingest(dataPath); err != nil {
						return fmt.Errorf("ingestion failed: %w", err)
					}
					fmt.Printf("Ingestion complete in %v\n", time.Since(start))
				}

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

		// 4. Mount via selected backend
		switch backend {
		case "nfs":
			return mountNFS(schema, g, engine, mountPoint, writable)
		case "fuse":
			return mountFUSE(schema, g, engine, mountPoint, writable)
		default:
			return fmt.Errorf("unknown backend %q (use nfs or fuse)", backend)
		}
	},
}

// mountControl starts Mache in hot-swap mode using the Control Block.
func mountControl(path string, schema *api.Topology, mountPoint, backend string) error {
	ctrl, err := control.OpenOrCreate(path)
	if err != nil {
		return fmt.Errorf("open control: %w", err)
	}
	defer func() { _ = ctrl.Close() }()

	// Initial Load
	gen := ctrl.GetGeneration()
	arenaPath := ctrl.GetArenaPath()
	fmt.Printf("Control Block: Gen %d -> %s\n", gen, arenaPath)

	// Wait for first valid generation if empty?
	if arenaPath == "" {
		fmt.Println("Waiting for initial arena...")
		for {
			if p := ctrl.GetArenaPath(); p != "" {
				arenaPath = p
				gen = ctrl.GetGeneration()
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	initialGraph, err := graph.OpenSQLiteGraph(arenaPath, schema, ingest.RenderTemplate)
	if err != nil {
		return fmt.Errorf("open initial graph %s: %w", arenaPath, err)
	}
	// Note: We don't defer Close() here because HotSwapGraph owns it (mostly)
	// But HotSwapGraph.Swap closes the old one. We should close the FINAL one on exit.

	hotSwap := graph.NewHotSwapGraph(initialGraph)

	// Start Watcher
	go func() {
		lastGen := gen
		for {
			time.Sleep(100 * time.Millisecond)
			currentGen := ctrl.GetGeneration()
			if currentGen > lastGen {
				newPath := ctrl.GetArenaPath()
				fmt.Printf("Hot Swap Detected: Gen %d -> %d (%s)\n", lastGen, currentGen, newPath)

				// Open new graph
				newGraph, err := graph.OpenSQLiteGraph(newPath, schema, ingest.RenderTemplate)
				if err != nil {
					fmt.Printf("Error opening new graph %s: %v\n", newPath, err)
					continue // Skip update
				}

				// Atomic Swap
				hotSwap.Swap(newGraph)
				lastGen = currentGen
			}
		}
	}()

	// Mount
	switch backend {
	case "nfs":
		return mountNFS(schema, hotSwap, nil, mountPoint, false)
	case "fuse":
		return mountFUSE(schema, hotSwap, nil, mountPoint, false)
	default:
		return fmt.Errorf("unknown backend %q", backend)
	}
}

// mountNFS starts an NFS server backed by GraphFS and mounts it.
func mountNFS(schema *api.Topology, g graph.Graph, engine *ingest.Engine, mountPoint string, writable bool) error {
	graphFs := nfsmount.NewGraphFS(g, schema)

	// Wire write-back if requested (validate → format → splice → surgical update → invalidate)
	if writable && engine != nil {
		store, isMemStore := g.(*graph.MemoryStore)
		graphFs.SetWriteBack(func(nodeID string, origin graph.SourceOrigin, content []byte) error {
			// Retrieve node to update DraftData
			node, err := g.GetNode(nodeID)
			if err != nil {
				return fmt.Errorf("node not found: %w", err)
			}

			// 1. Validate syntax before touching source file
			if err := writeback.Validate(content, origin.FilePath); err != nil {
				log.Printf("writeback: validation failed for %s: %v (saving draft)", origin.FilePath, err)
				// Store diagnostic for _diagnostics/ virtual dir
				if isMemStore {
					store.WriteStatus.Store(filepath.Dir(nodeID), err.Error())
					// Save as Draft
					draft := make([]byte, len(content))
					copy(draft, content)
					node.DraftData = draft
				}
				return nil
			}

			// 2. Format in-process (gofumpt for Go, hclwrite for HCL/Terraform)
			formatted := writeback.FormatBuffer(content, origin.FilePath)

			// Linting (Warning only)
			if strings.HasSuffix(origin.FilePath, ".go") {
				if diags, err := linter.Lint(formatted, "go"); err == nil && len(diags) > 0 {
					var sb strings.Builder
					for _, d := range diags {
						sb.WriteString(d.String() + "\n")
					}
					store.WriteStatus.Store(filepath.Dir(nodeID)+"/lint", sb.String())
				} else {
					store.WriteStatus.Delete(filepath.Dir(nodeID) + "/lint")
				}
			}

			// 3. Splice formatted content into source file
			oldLen := origin.EndByte - origin.StartByte
			if err := writeback.Splice(origin, formatted); err != nil {
				return err
			}

			// 4. Surgical node update — no re-ingest
			newOrigin := &graph.SourceOrigin{
				FilePath:  origin.FilePath,
				StartByte: origin.StartByte,
				EndByte:   origin.StartByte + uint32(len(formatted)),
			}
			if isMemStore {
				delta := int32(len(formatted)) - int32(oldLen)
				if delta != 0 {
					store.ShiftOrigins(origin.FilePath, origin.EndByte, delta)
				}
				_ = store.UpdateNodeContent(nodeID, formatted, newOrigin, time.Now())
				store.WriteStatus.Store(filepath.Dir(nodeID), "ok")
			}

			// 5. Invalidate cached size/content
			g.Invalidate(nodeID)
			return nil
		})
		fmt.Println("Write-back enabled: edits will splice into source files.")
	} else if writable {
		fmt.Println("Warning: --writable ignored (only supported for non-.db sources)")
	}

	srv, err := nfsmount.NewServer(graphFs)
	if err != nil {
		return fmt.Errorf("start NFS server: %w", err)
	}
	defer func() { _ = srv.Close() }()

	fmt.Printf("Mounting mache at %s (NFS on localhost:%d)...\n", mountPoint, srv.Port())

	if err := nfsmount.Mount(srv.Port(), mountPoint, writable); err != nil {
		return err
	}
	fmt.Printf("Mounted. Press Ctrl-C to unmount.\n")

	// Block until signal
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	fmt.Printf("\nUnmounting %s...\n", mountPoint)
	if err := nfsmount.Unmount(mountPoint); err != nil {
		fmt.Printf("Warning: unmount failed: %v\n", err)
		fmt.Printf("Run manually: sudo umount %s\n", mountPoint)
	}
	return nil
}

// mountFUSE starts a FUSE mount (original backend).
func mountFUSE(schema *api.Topology, g graph.Graph, engine *ingest.Engine, mountPoint string, writable bool) error {
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

	host := fuse.NewFileSystemHost(macheFs)
	host.SetCapReaddirPlus(true)

	fmt.Printf("Mounting mache at %s (using fuse-t/cgofuse)...\n", mountPoint)

	opts := []string{
		"-o", fmt.Sprintf("uid=%d", os.Getuid()),
		"-o", fmt.Sprintf("gid=%d", os.Getgid()),
		"-o", "fsname=mache",
		"-o", "subtype=mache",
		"-o", "entry_timeout=0.0",
		"-o", "attr_timeout=0.0",
		"-o", "negative_timeout=0.0",
		"-o", "direct_io",
	}

	if runtime.GOOS == "darwin" {
		opts = append(opts, "-o", "nobrowse")
		opts = append(opts, "-o", "noattrcache")
	}

	if !macheFs.Writable {
		opts = append([]string{"-o", "ro"}, opts...)
	}

	if !host.Mount(mountPoint, opts) {
		return fmt.Errorf("mount failed")
	}

	return nil
}

// inferFromTreeSitterFile reads a source file, parses it with tree-sitter,
// and infers a topology schema. Returns an error if parsing fails.
func inferFromTreeSitterFile(inf *lattice.Inferrer, path string, lang *sitter.Language, label string) (*api.Topology, error) {
	fmt.Printf("Inferring schema from %s source via Tree-sitter...", label)
	start := time.Now()
	defer func() { fmt.Printf(" done in %v\n", time.Since(start)) }()

	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	parser := sitter.NewParser()
	parser.SetLanguage(lang)
	tree, parseErr := parser.ParseCtx(context.Background(), nil, content)
	if parseErr != nil {
		return nil, fmt.Errorf("tree-sitter parse failed for %s: %w", path, parseErr)
	}
	if tree == nil {
		return nil, fmt.Errorf("tree-sitter returned nil tree for %s", path)
	}
	return inf.InferFromTreeSitter(tree.RootNode())
}

// Execute runs the root command.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
