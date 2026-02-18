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
	"sync"
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
	agentMode   bool
	outPath     string
)

func init() {
	rootCmd.Flags().StringVarP(&schemaPath, "schema", "s", "", "Path to topology schema")
	rootCmd.Flags().StringVarP(&dataPath, "data", "d", "", "Path to data source")
	rootCmd.Flags().StringVar(&controlPath, "control", "", "Path to Leyline control block (enables hot-swap)")
	rootCmd.Flags().BoolVarP(&writable, "writable", "w", false, "Enable write-back (splice edits into source files)")
	rootCmd.Flags().BoolVar(&inferSchema, "infer", false, "Auto-infer schema from data via FCA")
	rootCmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Suppress standard output")
	rootCmd.Flags().BoolVar(&agentMode, "agent", false, "Agent mode: auto-mount to temp dir with instructions")
	rootCmd.Flags().StringVar(&outPath, "out", "", "Write .db to path instead of mounting (for leyline load); not compatible with --agent")

	defaultBackend := "fuse"
	if runtime.GOOS == "darwin" {
		defaultBackend = "nfs"
	}
	rootCmd.Flags().StringVar(&backend, "backend", defaultBackend, "Mount backend: nfs or fuse")

	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(unmountCmd)
	rootCmd.AddCommand(cleanCmd)
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version number",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("mache version %s (commit %s, built %s)\n", Version, Commit, Date)
	},
}

var rootCmd = &cobra.Command{
	Use:     "mache [mountpoint]",
	Short:   "Mache: The Universal Semantic Overlay Engine",
	Args:    cobra.MaximumNArgs(1),
	Version: fmt.Sprintf("%s (commit %s, built %s)", Version, Commit, Date),
	RunE: func(cmd *cobra.Command, args []string) error {
		if quiet {
			f, err := os.Open(os.DevNull)
			if err == nil {
				os.Stdout = f
			}
		}

		// Validate flag combinations
		if outPath != "" && agentMode {
			return fmt.Errorf("--out and --agent cannot be used together (--agent enables writable mode, --out requires read-only)")
		}

		// Agent mode: auto-generate mount point and configure
		if agentMode {
			if err := runAgentMode(); err != nil {
				return err
			}
			// agentMetadata is now set, including the mount point
		}

		// Determine mount point
		var mountPoint string
		if agentMode {
			mountPoint = agentMetadata.MountPoint
		} else {
			// Normal mode: require mountpoint argument
			if len(args) == 0 {
				return fmt.Errorf("mountpoint required (or use --agent for auto mode)")
			}
			mountPoint = args[0]
		}

		// 0. Ensure mount point exists (create if needed)
		if err := os.MkdirAll(mountPoint, 0o755); err != nil {
			return fmt.Errorf("create mount point %s: %w", mountPoint, err)
		}

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

					// Pass 1: Quick language detection scan (no parsing)
					languageCounts := make(map[string]int)
					quickScanErr := filepath.Walk(dataPath, func(path string, info os.FileInfo, err error) error {
						if err != nil {
							return err
						}
						if info.IsDir() {
							base := filepath.Base(path)
							if (strings.HasPrefix(base, ".") && path != dataPath) || base == "target" || base == "node_modules" || base == "dist" || base == "build" {
								return filepath.SkipDir
							}
							return nil
						}
						ext := filepath.Ext(path)
						if langName, _, ok := ingest.DetectLanguageFromExt(ext); ok {
							languageCounts[langName]++
						}
						return nil
					})

					if quickScanErr != nil {
						err = fmt.Errorf("language scan failed: %w", quickScanErr)
					} else if len(languageCounts) == 0 {
						fmt.Printf("No source files found, using passthrough schema\n")
						inferred = &api.Topology{Version: "v1", Nodes: []api.Node{}}
					} else {
						fmt.Printf("Detected languages: ")
						for lang, count := range languageCounts {
							fmt.Printf("%s(%d) ", lang, count)
						}
						fmt.Printf("\n")

						// Pass 2: Parse and infer per language in parallel
						type langResult struct {
							lang    string
							records []any
							err     error
						}

						resultsChan := make(chan langResult, len(languageCounts))
						var wg sync.WaitGroup

						for targetLang := range languageCounts {
							wg.Add(1)
							go func(lang string) {
								defer wg.Done()

								var records []any
								fileCount := 0
								sampleLimit := 200 // Only parse first 200 files per language for FCA

								// Walk and parse SAMPLE of files for this language (for schema inference only)
								walkErr := filepath.Walk(dataPath, func(path string, info os.FileInfo, err error) error {
									if err != nil {
										return err
									}
									if info.IsDir() {
										base := filepath.Base(path)
										if (strings.HasPrefix(base, ".") && path != dataPath) || base == "target" || base == "node_modules" || base == "dist" || base == "build" {
											return filepath.SkipDir
										}
										return nil
									}

									// Stop early once we've collected enough samples
									if fileCount >= sampleLimit {
										return nil
									}

									ext := filepath.Ext(path)
									if ext == ".o" || ext == ".a" {
										return nil
									}

									langName, treeLang, ok := ingest.DetectLanguageFromExt(ext)
									if !ok || langName != lang {
										return nil // Skip files not for this language
									}

									// Parse with tree-sitter
									content, readErr := os.ReadFile(path)
									if readErr != nil {
										return nil // Skip unreadable files
									}

									parser := sitter.NewParser()
									parser.SetLanguage(treeLang)
									tree, _ := parser.ParseCtx(context.Background(), nil, content)
									if tree != nil {
										records = append(records, ingest.FlattenASTWithLanguage(tree.RootNode(), langName)...)
									}

									fileCount++
									return nil
								})

								fmt.Printf("  %s: sampled %d/%d files for schema inference\n", lang, fileCount, languageCounts[lang])

								if walkErr != nil {
									resultsChan <- langResult{lang: lang, err: walkErr}
									return
								}

								resultsChan <- langResult{lang: lang, records: records}
							}(targetLang)
						}

						// Wait for all goroutines and close channel
						go func() {
							wg.Wait()
							close(resultsChan)
						}()

						// Collect results
						recordsByLang := make(map[string][]any)
						for result := range resultsChan {
							if result.err != nil {
								err = fmt.Errorf("scan %s: %w", result.lang, result.err)
								break
							}
							if len(result.records) > 0 {
								recordsByLang[result.lang] = result.records
							}
						}

						if err == nil {
							if len(recordsByLang) == 0 {
								// No records parsed
								fmt.Printf("No parseable source files found, using passthrough schema\n")
								inferred = &api.Topology{Version: "v1", Nodes: []api.Node{}}
							} else if len(recordsByLang) == 1 {
								// Single language - flatten (backward compat)
								for langName, records := range recordsByLang {
									saved := inf.Config.Method
									savedLang := inf.Config.Language
									inf.Config.Method = "fca"
									inf.Config.Language = langName
									inferred, err = inf.InferFromRecords(records)
									inf.Config.Method = saved
									inf.Config.Language = savedLang
									if err == nil {
										fmt.Printf("Schema inferred from %d records in %v\n", len(records), time.Since(start))
									}
								}
							} else {
								// Multi-language - create namespaced schema
								inferred, err = inf.InferMultiLanguage(recordsByLang)
								if err == nil {
									totalRecords := 0
									for _, recs := range recordsByLang {
										totalRecords += len(recs)
									}
									fmt.Printf("Multi-language schema inferred from %d sampled records (%d languages) in %v\n", totalRecords, len(recordsByLang), time.Since(start))
								}
							}
						}
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

				// Wire call extractor for callees/ resolution
				sg.SetCallExtractor(newCallExtractor())

				start := time.Now()
				fmt.Print("Scanning records...")
				if err := sg.EagerScan(); err != nil {
					return fmt.Errorf("scan failed: %w", err)
				}
				fmt.Printf(" done in %v\n", time.Since(start))
				g = sg
			} else if !writable && ingest.SchemaUsesTreeSitter(schema) {
				// Read-only source: ingest to SQLite index, mount via SQLiteGraph (fast path).
				mountName := filepath.Base(mountPoint)
				tmpDir := filepath.Join(os.TempDir(), "mache")
				if err := os.MkdirAll(tmpDir, 0o755); err != nil {
					return fmt.Errorf("create temp dir: %w", err)
				}
				indexPath := filepath.Join(tmpDir, mountName+"-index.db")

				fmt.Printf("Indexing source to %s...\n", indexPath)
				start := time.Now()

				writer, err := ingest.NewSQLiteWriter(indexPath)
				if err != nil {
					return fmt.Errorf("create sqlite writer: %w", err)
				}

				eng := ingest.NewEngine(schema, writer)
				if err := eng.Ingest(dataPath); err != nil {
					_ = writer.Close()
					return fmt.Errorf("ingestion failed: %w", err)
				}
				if err := writer.Close(); err != nil {
					return fmt.Errorf("close sqlite writer: %w", err)
				}
				fmt.Printf("Indexing complete in %v\n", time.Since(start))
				eng.PrintRoutingSummary()

				// --out: materialize virtuals, copy .db, exit (no mount)
				if outPath != "" {
					if err := materializeVirtuals(indexPath, schema, agentMode); err != nil {
						return fmt.Errorf("materialize virtuals: %w", err)
					}
					if err := copyFile(indexPath, outPath); err != nil {
						return fmt.Errorf("copy db: %w", err)
					}
					_ = os.Remove(indexPath)
					fmt.Printf("Wrote %s\n", outPath)
					fmt.Printf("Load into leyline: leyline load --db %s --control /tmp/ll.ctrl\n", outPath)
					return nil
				}

				sg, err := graph.OpenSQLiteGraph(indexPath, schema, ingest.RenderTemplate)
				if err != nil {
					return fmt.Errorf("open indexed graph: %w", err)
				}
				defer func() {
					_ = sg.Close()
					_ = os.Remove(indexPath)
				}()

				sg.SetCallExtractor(newCallExtractor())
				g = sg
			} else {
				// Writable or non-tree-sitter: MemoryStore + ingestion pipeline
				store := graph.NewMemoryStore()
				resolver := ingest.NewSQLiteResolver()
				defer resolver.Close()
				store.SetResolver(resolver.Resolve)

				// Wire call extractor for callees/ resolution
				store.SetCallExtractor(newCallExtractor())

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
					engine.PrintRoutingSummary()
				} else {
					fmt.Printf("Ingesting data from %s...\n", dataPath)
					start := time.Now()
					if err := engine.Ingest(dataPath); err != nil {
						return fmt.Errorf("ingestion failed: %w", err)
					}
					fmt.Printf("Ingestion complete in %v\n", time.Since(start))
					engine.PrintRoutingSummary()
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

		// Agent mode: save metadata sidecar and generate prompt content
		var promptContent []byte
		if agentMode && agentMetadata != nil {
			if err := saveMountMetadata(mountPoint, agentMetadata); err != nil {
				fmt.Printf("Warning: failed to save mount metadata: %v\n", err)
			}
			promptContent = generatePromptContent(agentMetadata)
			fmt.Printf("\nAgent instructions: %s/PROMPT.txt\n", mountPoint)
			fmt.Printf("\nTo start:\n")
			fmt.Printf("  cd %s\n", mountPoint)
			fmt.Printf("  cat PROMPT.txt\n")
			fmt.Printf("  claude  # or your preferred LLM\n")
			fmt.Printf("\nTo stop:\n")
			fmt.Printf("  mache unmount %s\n", filepath.Base(mountPoint))
			fmt.Printf("  # or press Ctrl+C in this terminal\n\n")
		}

		// 4. Mount via selected backend
		switch backend {
		case "nfs":
			return mountNFS(schema, g, engine, mountPoint, writable, promptContent)
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

	// Wait for first valid generation if empty
	if arenaPath == "" {
		fmt.Println("Waiting for initial arena...")
		deadline := time.After(30 * time.Second)
		for {
			select {
			case <-deadline:
				return fmt.Errorf("timed out waiting for initial arena (30s)")
			default:
			}
			if p := ctrl.GetArenaPath(); p != "" {
				arenaPath = p
				gen = ctrl.GetGeneration()
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	// Wait for valid arena header
	fmt.Println("Waiting for valid arena header...")
	var dbPath string
	deadline := time.After(30 * time.Second)
	for {
		dbPath, err = graph.ExtractActiveDB(arenaPath)
		if err == nil {
			break
		}
		select {
		case <-deadline:
			return fmt.Errorf("timed out waiting for valid arena header (30s): %w", err)
		default:
		}
		// Retry until header is valid (written by Leyline atomic swap)
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Println("Arena header valid. Initializing graph.")

	// Writable arena mode: mache IS the writer, no hot-swap watcher.
	if writable {
		return mountControlWritable(dbPath, arenaPath, schema, ctrl, mountPoint, backend)
	}

	// Read-only hot-swap mode (existing logic)
	initialGraph, err := graph.OpenSQLiteGraph(dbPath, schema, ingest.RenderTemplate)
	if err != nil {
		return fmt.Errorf("open initial graph %s: %w", dbPath, err)
	}

	hotSwap := graph.NewHotSwapGraph(initialGraph)

	// Start Watcher
	go func() {
		lastGen := gen
		prevDBPath := dbPath // track for cleanup
		for {
			time.Sleep(100 * time.Millisecond)
			currentGen := ctrl.GetGeneration()
			if currentGen > lastGen {
				newPath := ctrl.GetArenaPath()
				fmt.Printf("Hot Swap Detected: Gen %d -> %d (%s)\n", lastGen, currentGen, newPath)

				// Extract new DB from arena
				newDBPath, err := graph.ExtractActiveDB(newPath)
				if err != nil {
					fmt.Printf("Error extracting new db: %v\n", err)
					continue
				}

				// Open new graph
				newGraph, err := graph.OpenSQLiteGraph(newDBPath, schema, ingest.RenderTemplate)
				if err != nil {
					fmt.Printf("Error opening new graph %s: %v\n", newDBPath, err)
					_ = os.Remove(newDBPath)
					continue
				}

				// Atomic Swap
				hotSwap.Swap(newGraph)
				lastGen = currentGen

				// Clean up previous temp DB
				if prevDBPath != "" {
					_ = os.Remove(prevDBPath)
				}
				prevDBPath = newDBPath
			}
		}
	}()

	// Mount
	switch backend {
	case "nfs":
		return mountNFS(schema, hotSwap, nil, mountPoint, false, nil)
	case "fuse":
		return mountFUSE(schema, hotSwap, nil, mountPoint, false)
	default:
		return fmt.Errorf("unknown backend %q", backend)
	}
}

// mountControlWritable opens the extracted DB in read-write mode and
// wires a WritableGraph + ArenaFlusher for arena write-back.
func mountControlWritable(masterDBPath, arenaPath string, schema *api.Topology, ctrl *control.Controller, mountPoint, backend string) error {
	flusher := graph.NewArenaFlusher(arenaPath, masterDBPath, ctrl)
	flusher.Start(100 * time.Millisecond)
	defer func() { _ = flusher.Close() }() // final flush on unmount

	wg, err := graph.OpenWritableGraph(masterDBPath, schema, ingest.RenderTemplate, flusher)
	if err != nil {
		return fmt.Errorf("open writable graph: %w", err)
	}
	defer func() { _ = wg.Close() }()

	fmt.Println("Writable arena mode: edits write to master DB and flush to arena (100ms coalesce).")

	switch backend {
	case "nfs":
		return mountWritableNFS(schema, wg, mountPoint)
	case "fuse":
		return mountFUSE(schema, wg, nil, mountPoint, true)
	default:
		return fmt.Errorf("unknown backend %q", backend)
	}
}

// mountWritableNFS mounts a WritableGraph via NFS with arena write-back.
func mountWritableNFS(schema *api.Topology, wg *graph.WritableGraph, mountPoint string) error {
	graphFs := nfsmount.NewGraphFS(wg, schema)

	graphFs.SetWriteBack(func(nodeID string, origin graph.SourceOrigin, content []byte) error {
		// Update DB record, then request coalesced arena flush (non-blocking).
		if err := wg.UpdateRecord(nodeID, content); err != nil {
			return fmt.Errorf("update record: %w", err)
		}
		wg.Flush() // coalesced — actual I/O on next tick
		return nil
	})

	srv, err := nfsmount.NewServer(graphFs)
	if err != nil {
		return fmt.Errorf("start NFS server: %w", err)
	}
	defer func() { _ = srv.Close() }()

	fmt.Printf("Mounting mache at %s (NFS on localhost:%d)...\n", mountPoint, srv.Port())

	if err := nfsmount.Mount(srv.Port(), mountPoint, true); err != nil {
		return err
	}
	fmt.Printf("Mounted (writable). Press Ctrl-C to unmount.\n")

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

// mountNFS starts an NFS server backed by GraphFS and mounts it.
func mountNFS(schema *api.Topology, g graph.Graph, engine *ingest.Engine, mountPoint string, writable bool, promptContent []byte) error {
	graphFs := nfsmount.NewGraphFS(g, schema)
	if len(promptContent) > 0 {
		graphFs.SetPromptContent(promptContent)
	}

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
				// Use source file mtime for deterministic timestamps
				modTime := time.Now()
				if fi, err := os.Stat(origin.FilePath); err == nil {
					modTime = fi.ModTime()
				}
				_ = store.UpdateNodeContent(nodeID, formatted, newOrigin, modTime)
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

// newCallExtractor creates a CallExtractor that uses a sync.Pool of tree-sitter
// parsers to reduce allocation overhead. Safe for concurrent use.
func newCallExtractor() graph.CallExtractor {
	walker := ingest.NewSitterWalker()
	pool := &sync.Pool{
		New: func() any { return sitter.NewParser() },
	}
	return func(content []byte, path, langName string) ([]graph.QualifiedCall, error) {
		lang := ingest.GetLanguage(langName)
		if lang == nil {
			return nil, nil
		}
		parser := pool.Get().(*sitter.Parser)
		defer pool.Put(parser)
		parser.SetLanguage(lang)
		tree, _ := parser.ParseCtx(context.Background(), nil, content)
		if tree == nil {
			return nil, nil
		}
		return walker.ExtractQualifiedCalls(tree.RootNode(), content, lang, langName)
	}
}

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List active mache mounts",
	RunE: func(cmd *cobra.Command, args []string) error {
		mounts, err := listActiveMounts()
		if err != nil {
			return err
		}

		if len(mounts) == 0 {
			fmt.Println("No active mache mounts found.")
			return nil
		}

		fmt.Printf("%-20s %-10s %-50s %s\n", "MOUNT", "PID", "SOURCE", "STATUS")
		fmt.Println(strings.Repeat("-", 100))

		for _, meta := range mounts {
			name := filepath.Base(meta.MountPoint)
			status := "running"
			if !isProcessRunning(meta.PID) {
				status = "stale"
			}
			fmt.Printf("%-20s %-10d %-50s %s\n", name, meta.PID, meta.Source, status)
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
			mountsDir, err := getAgentMountsDir()
			if err != nil {
				return err
			}
			mountPoint = filepath.Join(mountsDir, mountName)
		}

		// Load metadata
		meta, err := loadMountMetadata(mountPoint)
		if err != nil {
			return fmt.Errorf("failed to load mount metadata: %w", err)
		}

		// Kill the process
		if isProcessRunning(meta.PID) {
			process, err := os.FindProcess(meta.PID)
			if err != nil {
				return fmt.Errorf("failed to find process %d: %w", meta.PID, err)
			}

			fmt.Printf("Stopping mache process (PID %d)...\n", meta.PID)
			if err := process.Signal(syscall.SIGTERM); err != nil {
				return fmt.Errorf("failed to send SIGTERM: %w", err)
			}

			// Wait briefly for graceful shutdown
			time.Sleep(2 * time.Second)

			if isProcessRunning(meta.PID) {
				fmt.Printf("Process still running, sending SIGKILL...\n")
				_ = process.Signal(syscall.SIGKILL)
			}
		}

		// Clean up mount directory and sidecar
		fmt.Printf("Removing mount directory: %s\n", mountPoint)
		if err := os.RemoveAll(mountPoint); err != nil {
			return fmt.Errorf("failed to remove mount directory: %w", err)
		}
		_ = os.Remove(sidecarPath(mountPoint))

		fmt.Println("Mount stopped successfully.")
		return nil
	},
}

var cleanCmd = &cobra.Command{
	Use:   "clean",
	Short: "Remove stale mache mounts (where process has died)",
	RunE: func(cmd *cobra.Command, args []string) error {
		mounts, err := listActiveMounts()
		if err != nil {
			return err
		}

		cleaned := 0
		for _, meta := range mounts {
			if !isProcessRunning(meta.PID) {
				fmt.Printf("Removing stale mount: %s (PID %d was not running)\n",
					filepath.Base(meta.MountPoint), meta.PID)
				if err := os.RemoveAll(meta.MountPoint); err != nil {
					fmt.Printf("Warning: failed to remove %s: %v\n", meta.MountPoint, err)
				} else {
					_ = os.Remove(sidecarPath(meta.MountPoint))
					cleaned++
				}
			}
		}

		if cleaned == 0 {
			fmt.Println("No stale mounts found.")
		} else {
			fmt.Printf("Cleaned %d stale mount(s).\n", cleaned)
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
