package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
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
	"github.com/agentic-research/mache/internal/lang"
	"github.com/agentic-research/mache/internal/lattice"
	"github.com/agentic-research/mache/internal/leyline"
	"github.com/agentic-research/mache/internal/linter"
	"github.com/agentic-research/mache/internal/materialize"
	"github.com/agentic-research/mache/internal/nfsmount"
	"github.com/agentic-research/mache/internal/writeback"
	sitter "github.com/smacker/go-tree-sitter"
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
	outFormat   string
	nfsOpts     string
	snapshot    bool
	maxFileSize string
)

func init() {
	rootCmd.Flags().StringVarP(&schemaPath, "schema", "s", "", "Path to topology schema")
	rootCmd.Flags().StringVarP(&dataPath, "data", "d", "", "Path to data source")
	rootCmd.Flags().StringVar(&controlPath, "control", "", "Path to Leyline control block (enables hot-swap)")
	rootCmd.Flags().BoolVarP(&writable, "writable", "w", false, "Enable write-back (splice edits into source files)")
	rootCmd.Flags().BoolVar(&inferSchema, "infer", false, "Auto-infer schema from data via FCA")
	rootCmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Suppress standard output")
	rootCmd.Flags().BoolVar(&agentMode, "agent", false, "Agent mode: auto-mount to temp dir with instructions")
	rootCmd.Flags().StringVar(&outPath, "out", "", "Write to path instead of mounting; not compatible with --agent")
	rootCmd.Flags().StringVar(&outFormat, "format", "sqlite", "Output format for --out: sqlite, zip, boltdb (requires -tags boltdb)")
	rootCmd.Flags().StringVar(&nfsOpts, "nfs-opts", "", "Extra NFS mount options (comma-separated, appended to defaults)")
	rootCmd.Flags().BoolVar(&snapshot, "snapshot", false, "Copy data source to temp before mounting (true sandbox; copy is not atomic; default is zero-copy)")
	rootCmd.Flags().StringVar(&maxFileSize, "max-file-size", "100MB", "Skip files larger than this during ingestion (e.g. 100MB, 1GB, 0 to disable)")

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
			log.SetOutput(io.Discard)
			f, err := os.Open(os.DevNull)
			if err == nil {
				os.Stdout = f
			}
		}

		// Apply --max-file-size
		if maxFileSize != "" {
			mfs, err := ingest.ParseSize(maxFileSize)
			if err != nil {
				return fmt.Errorf("--max-file-size: %w", err)
			}
			ingest.MaxIngestFileSize = mfs
		}

		// Validate flag combinations
		if outPath != "" && agentMode {
			return fmt.Errorf("--out and --agent cannot be used together (--agent enables writable mode, --out requires read-only)")
		}

		// Agent mode: auto-generate mount point and configure
		if agentMode {
			if err := runAgentMode(cmd); err != nil {
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
				log.Print("Inferring schema from SQLite data via FCA...")
				start := time.Now()
				inferred, err = inf.InferFromSQLite(dataPath)
				log.Printf("Schema inference done in %v", time.Since(start))
			case ".git":
				log.Print("Loading git commits...")
				start := time.Now()
				recs, readErr := ingest.LoadGitCommits(dataPath)
				if readErr != nil {
					err = readErr
					log.Printf("Loading git commits failed in %v", time.Since(start))
				} else {
					log.Printf("Loaded %d commits in %v", len(recs), time.Since(start))
					log.Print("Inferring schema from Git history (Greedy)...")
					start = time.Now()
					// Enable Git hints
					inf.Config.Hints = ingest.GetGitHints()
					inferred, err = inf.InferFromRecords(recs)
				}
				log.Printf("Schema inference done in %v", time.Since(start))
			default:
				// Try tree-sitter language lookup from the registry
				if l := lang.ForExt(ext); l != nil {
					inferred, err = inferFromTreeSitterFile(inf, dataPath, l.Grammar(), l.DisplayName)
				} else {
					// Check if it's a directory
					info, errStat := os.Stat(dataPath)
					if errStat == nil && info.IsDir() {
						log.Printf("Inferring schema from directory %s...", dataPath)
						start := time.Now()
						inferred, err = inferDirSchema(dataPath)
						if err == nil {
							log.Printf("Schema inferred in %v", time.Since(start))
						}
					} else {
						err = fmt.Errorf("automatic inference not supported for %s", ext)
					}
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
				log.Printf("Inferred schema written to %s", schemaPath)
			}
		} else if s, err := os.ReadFile(schemaPath); err == nil {
			log.Printf("Loaded schema from %s", schemaPath)
			schema = &api.Topology{}
			if err := json.Unmarshal(s, schema); err != nil {
				return fmt.Errorf("failed to parse schema: %w", err)
			}
		} else {
			if cmd.Flags().Changed("schema") {
				return fmt.Errorf("failed to read schema file: %w", err)
			}
			log.Println("No schema found, using default empty schema.")
			schema = &api.Topology{Version: "v1alpha1"}
		}

		// 2b. Expand file_set includes before ingestion/mount.
		schema.ResolveIncludes()

		// 3. Create the Graph backend
		var g graph.Graph
		var engine *ingest.Engine // non-nil for MemoryStore paths (needed for write-back)

		if controlPath != "" {
			return mountControl(controlPath, schema, mountPoint, backend)
		}

		// Snapshot: copy data source to temp before mounting for isolation.
		var snapshotPath string // set if snapshot is active, for cleanup decision
		originalDataPath := dataPath
		if snapshot {
			if info, err := os.Stat(dataPath); err == nil {
				snapDir := filepath.Join(os.TempDir(), "mache", "snapshots")
				if err := os.MkdirAll(snapDir, 0o755); err != nil {
					return fmt.Errorf("create snapshot dir: %w", err)
				}
				snapshotPath = filepath.Join(snapDir, fmt.Sprintf("snap-%d-%s", os.Getpid(), filepath.Base(dataPath)))
				if info.IsDir() {
					// Size warning for large directories
					if size, sizeErr := dirSize(dataPath); sizeErr == nil && size > 1<<30 {
						log.Printf("Warning: source directory is %d MB — snapshot copy may take a while", size>>20)
					}
					log.Printf("Snapshot: copying %s → %s...", dataPath, snapshotPath)
					start := time.Now()
					n, err := copyDir(dataPath, snapshotPath)
					if err != nil {
						return fmt.Errorf("snapshot copy dir: %w", err)
					}
					log.Printf("Snapshot: copied %d files in %v", n, time.Since(start))
				} else {
					log.Printf("Snapshot: copying %s → %s", dataPath, snapshotPath)
					if err := copyFile(dataPath, snapshotPath); err != nil {
						return fmt.Errorf("snapshot copy: %w", err)
					}
				}
				dataPath = snapshotPath

				// Writable snapshots are preserved so the agent's edits survive.
				// Read-only snapshots are disposable and cleaned up on unmount.
				if writable {
					defer func() {
						log.Printf("Snapshot preserved at: %s", snapshotPath)
						log.Printf("Review changes:  diff -r %s %s", snapshotPath, originalDataPath)
						log.Printf("Apply changes:   rsync -av %s/ %s/", snapshotPath, originalDataPath)
						log.Printf("Discard:         rm -rf %s", snapshotPath)
					}()
				} else {
					defer func() { _ = os.RemoveAll(snapshotPath) }()
				}
			}
		}

		// Update agent metadata source to point at snapshot (not original)
		if agentMode && agentMetadata != nil && snapshotPath != "" {
			agentMetadata.Source = snapshotPath
		}

		if _, err := os.Stat(dataPath); err == nil {
			if filepath.Ext(dataPath) == ".db" {
				// SQLite source: eager scan before mount to avoid fuse-t NFS timeouts
				log.Printf("Opening %s (direct SQL backend)...", dataPath)
				sg, err := graph.OpenSQLiteGraph(dataPath, schema, ingest.RenderTemplate)
				if err != nil {
					return fmt.Errorf("open sqlite graph: %w", err)
				}
				defer func() { _ = sg.Close() }() // safe to ignore

				// Wire call extractor for callees/ resolution
				sg.SetCallExtractor(newCallExtractor())

				start := time.Now()
				log.Print("Scanning records...")
				if err := sg.EagerScan(); err != nil {
					return fmt.Errorf("scan failed: %w", err)
				}
				log.Printf("Scanning records done in %v", time.Since(start))
				g = sg
			} else if !writable && ingest.SchemaUsesTreeSitter(schema) {
				// Read-only source: ingest to SQLite index, mount via SQLiteGraph (fast path).
				// Uses persistent cache so re-mounts can skip unchanged files.
				mountName := filepath.Base(mountPoint)
				cacheDir := filepath.Join(os.TempDir(), "mache")
				if err := os.MkdirAll(cacheDir, 0o755); err != nil {
					return fmt.Errorf("create cache dir: %w", err)
				}
				// Include hash of resolved data path to avoid collisions when
				// different source directories are mounted to the same mount name.
				absDataPath, err := filepath.Abs(dataPath)
				if err != nil {
					return fmt.Errorf("resolve data path: %w", err)
				}
				sum := sha256.Sum256([]byte(absDataPath))
				hashSuffix := fmt.Sprintf("%x", sum[:8])
				indexPath := filepath.Join(cacheDir, fmt.Sprintf("%s-%s-index.db", mountName, hashSuffix))

				// Load existing file index for incremental re-ingestion.
				var fileIndex map[string]ingest.FileIndexEntry
				if _, err := os.Stat(indexPath); err == nil {
					if idx, err := ingest.LoadFileIndex(indexPath); err == nil && len(idx) > 0 {
						fileIndex = idx
						log.Printf("Loaded file index with %d entries (incremental mode)", len(idx))
					}
				}

				log.Printf("Indexing source to %s...", indexPath)
				start := time.Now()

				writer, err := ingest.NewSQLiteWriter(indexPath)
				if err != nil {
					return fmt.Errorf("create sqlite writer: %w", err)
				}

				eng := ingest.NewEngine(schema, writer)
				if fileIndex != nil {
					eng.SetFileIndex(fileIndex)
				}
				if err := eng.Ingest(dataPath); err != nil {
					_ = writer.Close()
					return fmt.Errorf("ingestion failed: %w", err)
				}
				if err := writer.Close(); err != nil {
					return fmt.Errorf("close sqlite writer: %w", err)
				}
				log.Printf("Indexing complete in %v", time.Since(start))
				eng.PrintRoutingSummary()

				// --out: materialize virtuals, write to target format, exit (no mount)
				if outPath != "" {
					if err := materializeVirtuals(indexPath, schema, agentMode); err != nil {
						return fmt.Errorf("materialize virtuals: %w", err)
					}
					mat, err := materialize.ForFormat(outFormat)
					if err != nil {
						return err
					}
					if err := mat.Materialize(indexPath, outPath); err != nil {
						return fmt.Errorf("materialize (%s): %w", outFormat, err)
					}
					_ = os.Remove(indexPath)
					log.Printf("Wrote %s (format: %s)", outPath, outFormat)
					if outFormat == "sqlite" {
						log.Printf("Load into leyline: leyline load --db %s --control /tmp/ll.ctrl", outPath)
					}
					return nil
				}

				sg, err := graph.OpenSQLiteGraph(indexPath, schema, ingest.RenderTemplate)
				if err != nil {
					return fmt.Errorf("open indexed graph: %w", err)
				}
				defer func() {
					_ = sg.Close()
					// Keep the index file for incremental re-ingestion on next mount.
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
					log.Printf("Ingesting git history from %s...", dataPath)
					start := time.Now()
					recs, err := ingest.LoadGitCommits(dataPath)
					if err != nil {
						return fmt.Errorf("load git: %w", err)
					}
					if err := engine.IngestRecords(recs); err != nil {
						return fmt.Errorf("ingest git records: %w", err)
					}
					log.Printf("Ingestion complete in %v", time.Since(start))
					engine.PrintRoutingSummary()
				} else {
					log.Printf("Ingesting data from %s...", dataPath)
					start := time.Now()
					if err := engine.Ingest(dataPath); err != nil {
						return fmt.Errorf("ingestion failed: %w", err)
					}
					log.Printf("Ingestion complete in %v", time.Since(start))
					engine.PrintRoutingSummary()
				}

				// Wire live graph refresher: re-ingest stale files on read
				store.SetRefresher(engine.ReIngestFile)

				// Enable SQL query support for MemoryStore
				if err := store.InitRefsDB(); err != nil {
					return fmt.Errorf("init refs db: %w", err)
				}
				defer func() { _ = store.Close() }() // safe to ignore
				if err := store.FlushRefs(); err != nil {
					log.Printf("Warning: refs flush failed: %v", err)
				}

				g = store
			}
		} else {
			if cmd.Flags().Changed("data") {
				return fmt.Errorf("data path not found: %s", dataPath)
			}
			log.Printf("No data found at %s, starting empty.", dataPath)
			g = graph.NewMemoryStore()
		}

		// Agent mode: save metadata sidecar and generate prompt content
		var promptContent []byte
		if agentMode && agentMetadata != nil {
			if err := saveMountMetadata(mountPoint, agentMetadata); err != nil {
				log.Printf("Warning: failed to save mount metadata: %v", err)
			}
			promptContent = generatePromptContent(agentMetadata)
			log.Printf("Agent instructions: %s/PROMPT.txt", mountPoint)
			log.Print("To start:")
			log.Printf("  cd %s", mountPoint)
			log.Print("  cat PROMPT.txt")
			log.Print("  claude  # or your preferred LLM")
			log.Print("To stop:")
			log.Printf("  mache unmount %s", filepath.Base(mountPoint))
			log.Print("  # or press Ctrl+C in this terminal")
		}

		// Clean up any auto-spawned leyline daemon when the mount exits.
		// TriggerEmbedding (below) or semantic search may auto-start one.
		defer leyline.StopManaged()

		// Fire-and-forget: push content to ley-line for embedding
		go leyline.TriggerEmbedding(g, 100)

		// 4. Mount via selected backend
		switch backend {
		case "nfs":
			return mountNFS(schema, g, engine, mountPoint, writable, promptContent)
		case "fuse":
			return mountFUSE(schema, g, engine, mountPoint, writable, promptContent)
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
	log.Printf("Control Block: Gen %d -> %s", gen, arenaPath)

	// Wait for first valid generation if empty
	if arenaPath == "" {
		log.Println("Waiting for initial arena...")
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
	log.Println("Waiting for valid arena header...")
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
	log.Println("Arena header valid. Initializing graph.")

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
				log.Printf("Hot Swap Detected: Gen %d -> %d (%s)", lastGen, currentGen, newPath)

				// Extract new DB from arena
				newDBPath, err := graph.ExtractActiveDB(newPath)
				if err != nil {
					log.Printf("Error extracting new db: %v", err)
					continue
				}

				// Open new graph
				newGraph, err := graph.OpenSQLiteGraph(newDBPath, schema, ingest.RenderTemplate)
				if err != nil {
					log.Printf("Error opening new graph %s: %v", newDBPath, err)
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
		return mountFUSE(schema, hotSwap, nil, mountPoint, false, nil)
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

	log.Println("Writable arena mode: edits write to master DB and flush to arena (100ms coalesce).")

	switch backend {
	case "nfs":
		return mountWritableNFS(schema, wg, mountPoint)
	case "fuse":
		return mountFUSE(schema, wg, nil, mountPoint, true, nil)
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

	log.Printf("Mounting mache at %s (NFS on localhost:%d)...", mountPoint, srv.Port())

	if err := nfsmount.Mount(srv.Port(), mountPoint, true, nfsOpts); err != nil {
		return err
	}
	log.Print("Mounted (writable). Press Ctrl-C to unmount.")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Printf("Unmounting %s...", mountPoint)
	if err := nfsmount.Unmount(mountPoint); err != nil {
		log.Printf("Warning: unmount failed: %v", err)
		log.Printf("Run manually: sudo umount %s", mountPoint)
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
				store.RecordFileMtime(origin.FilePath, modTime)
				store.WriteStatus.Store(filepath.Dir(nodeID), "ok")
			}

			// 5. Invalidate cached size/content
			g.Invalidate(nodeID)
			return nil
		})
		log.Println("Write-back enabled: edits will splice into source files.")
	} else if writable {
		log.Println("Warning: --writable ignored (only supported for non-.db sources)")
	}

	srv, err := nfsmount.NewServer(graphFs)
	if err != nil {
		return fmt.Errorf("start NFS server: %w", err)
	}
	defer func() { _ = srv.Close() }()

	log.Printf("Mounting mache at %s (NFS on localhost:%d)...", mountPoint, srv.Port())

	if err := nfsmount.Mount(srv.Port(), mountPoint, writable, nfsOpts); err != nil {
		return err
	}
	log.Print("Mounted. Press Ctrl-C to unmount.")

	// Block until signal
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Printf("Unmounting %s...", mountPoint)
	if err := nfsmount.Unmount(mountPoint); err != nil {
		log.Printf("Warning: unmount failed: %v", err)
		log.Printf("Run manually: sudo umount %s", mountPoint)
	}
	return nil
}

// mountFUSE starts a FUSE mount (original backend).
func mountFUSE(schema *api.Topology, g graph.Graph, engine *ingest.Engine, mountPoint string, writable bool, promptContent []byte) error {
	macheFs := machefs.NewMacheFS(schema, g)
	if len(promptContent) > 0 {
		macheFs.SetPromptContent(promptContent)
	}

	// Wire up query directory (enables /.query/ magic dir for both backends)
	if sg, ok := g.(*graph.SQLiteGraph); ok {
		macheFs.SetQueryFunc(sg.QueryRefs)
	} else if ms, ok := g.(*graph.MemoryStore); ok {
		macheFs.SetQueryFunc(ms.QueryRefs)
	}

	// Wire up semantic search for `? query` prefix in .query/
	macheFs.SetSemanticSearchFunc(func(query string, k int) ([]machefs.SemanticHit, error) {
		sockPath, err := leyline.DiscoverOrStart()
		if err != nil {
			return nil, err
		}
		sock, err := leyline.DialSocket(sockPath)
		if err != nil {
			return nil, err
		}
		defer func() { _ = sock.Close() }()
		sc := leyline.NewSemanticClient(sock)
		results, err := sc.Search(query, k)
		if err != nil {
			return nil, err
		}
		hits := make([]machefs.SemanticHit, len(results))
		for i, r := range results {
			hits[i] = machefs.SemanticHit{Path: r.ID, Distance: r.Distance}
		}
		return hits, nil
	})

	// Wire up write-back if requested (only for MemoryStore + tree-sitter sources)
	if writable && engine != nil {
		macheFs.Engine = engine
		// Wire both the exported field and the VFS diagnostics handler
		var diagStatus *sync.Map
		if ms, ok := g.(*graph.MemoryStore); ok {
			diagStatus = &ms.WriteStatus
		}
		macheFs.SetWritable(true, diagStatus)
		log.Println("Write-back enabled: edits will splice into source files.")
	} else if writable {
		log.Println("Warning: --writable ignored (only supported for non-.db sources)")
	}

	host := fuse.NewFileSystemHost(macheFs)
	host.SetCapReaddirPlus(true)

	log.Printf("Mounting mache at %s (using fuse-t/cgofuse)...", mountPoint)

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
	log.Printf("Inferring schema from %s source via Tree-sitter...", label)
	start := time.Now()
	defer func() { log.Printf("Schema inference done in %v", time.Since(start)) }()

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
		l := lang.ForName(langName)
		if l == nil {
			return nil, nil
		}
		grammar := l.Grammar()
		parser := pool.Get().(*sitter.Parser)
		defer pool.Put(parser)
		parser.SetLanguage(grammar)
		tree, _ := parser.ParseCtx(context.Background(), nil, content)
		if tree == nil {
			return nil, nil
		}
		return walker.ExtractQualifiedCalls(tree.RootNode(), content, grammar, langName)
	}
}

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List active mache instances (mounts and MCP servers)",
	RunE: func(cmd *cobra.Command, args []string) error {
		mounts, err := listActiveMounts()
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
			if !isProcessRunning(meta.PID) {
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

			log.Printf("Stopping mache process (PID %d)...", meta.PID)
			if err := process.Signal(syscall.SIGTERM); err != nil {
				return fmt.Errorf("failed to send SIGTERM: %w", err)
			}

			// Wait briefly for graceful shutdown
			time.Sleep(2 * time.Second)

			if isProcessRunning(meta.PID) {
				log.Printf("Process still running, sending SIGKILL...")
				_ = process.Signal(syscall.SIGKILL)
			}
		}

		// Clean up mount directory and sidecar
		log.Printf("Removing mount directory: %s", mountPoint)
		if err := os.RemoveAll(mountPoint); err != nil {
			return fmt.Errorf("failed to remove mount directory: %w", err)
		}
		_ = os.Remove(sidecarPath(mountPoint))

		log.Println("Mount stopped successfully.")
		return nil
	},
}

var cleanCmd = &cobra.Command{
	Use:   "clean",
	Short: "Remove stale mache mounts and orphaned snapshots",
	RunE: func(cmd *cobra.Command, args []string) error {
		mounts, err := listActiveMounts()
		if err != nil {
			return err
		}

		cleaned := 0
		for _, meta := range mounts {
			if !isProcessRunning(meta.PID) {
				log.Printf("Removing stale mount: %s (PID %d was not running)",
					filepath.Base(meta.MountPoint), meta.PID)
				if err := os.RemoveAll(meta.MountPoint); err != nil {
					log.Printf("Warning: failed to remove %s: %v", meta.MountPoint, err)
				} else {
					_ = os.Remove(sidecarPath(meta.MountPoint))
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
				if !isProcessRunning(pid) {
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

// Execute runs the root command.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
