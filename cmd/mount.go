package cmd

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/buildinfo"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/lang"
	"github.com/agentic-research/mache/internal/lattice"
	"github.com/agentic-research/mache/internal/leyline"
	"github.com/agentic-research/mache/internal/materialize"
	machetmpl "github.com/agentic-research/mache/internal/template"
	"github.com/spf13/cobra"
)

var (
	// Version is the binary's build version, shown by `mache version` and
	// stamped into build artifacts (_mache_meta, the build-cache lockfile
	// producer field, the MCP server version). `task build` and release.yml
	// inject it from `git describe --tags` via -ldflags, so it tells the truth
	// about the built code: a clean tag (0.10.0) on a tagged release, a
	// git-distance string (0.9.0-9-gabc1234) between releases. A bare
	// `go build` / `go test` (no ldflags) falls back to the clean release base
	// in internal/buildinfo (version.txt).
	//
	// NOTE: server.json and melange.yaml derive from buildinfo.Version (the
	// clean, committed, drift-checked release base) — never from this string —
	// so a dev-distance build version can't break the server-json drift gate or
	// produce an invalid apk package version.
	Version = resolveBuildVersion()
	Commit  = "none"
	Date    = "unknown"
)

// buildVersion is injected via
// -ldflags "-X github.com/agentic-research/mache/cmd.buildVersion=<git describe>".
// Empty for a bare build; see Version's doc for the fallback.
var buildVersion string

func resolveBuildVersion() string {
	if v := strings.TrimSpace(buildVersion); v != "" {
		return strings.TrimPrefix(v, "v")
	}
	return buildinfo.Version
}

var (
	schemaPath  string
	dataPath    string
	controlPath string
	writable    bool
	inferSchema bool
	quiet       bool
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
	rootCmd.Flags().StringVar(&outFormat, "format", "sqlite", "Output format for --out: sqlite, zip, json")
	rootCmd.Flags().StringVar(&nfsOpts, "nfs-opts", "", "Extra NFS mount options (comma-separated, appended to defaults)")
	rootCmd.Flags().BoolVar(&snapshot, "snapshot", false, "Copy data source to temp before mounting (true sandbox; copy is not atomic; default is zero-copy)")
	rootCmd.Flags().StringVar(&maxFileSize, "max-file-size", "100MB", "Skip files larger than this during ingestion (e.g. 100MB, 1GB, 0 to disable)")

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
			return mountControl(controlPath, schema, mountPoint)
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
				// --out with .db source: ingest via SQLiteWriter, materialize, exit.
				// Skip OpenSQLiteGraph/EagerScan entirely — no mount needed.
				if outPath != "" {
					indexFile, err := os.CreateTemp("", "mache-out-*.db")
					if err != nil {
						return fmt.Errorf("create temp index: %w", err)
					}
					indexPath := indexFile.Name()
					_ = indexFile.Close() // SQLiteWriter opens it by path
					defer func() { _ = os.Remove(indexPath) }()

					writer, err := ingest.NewSQLiteWriter(indexPath)
					if err != nil {
						return fmt.Errorf("create sqlite writer: %w", err)
					}
					eng := ingest.NewEngine(schema, writer)
					start := time.Now()
					if err := eng.Ingest(dataPath); err != nil {
						_ = writer.Close()
						return fmt.Errorf("ingest for --out: %w", err)
					}
					if err := writer.Close(); err != nil {
						return fmt.Errorf("close sqlite writer: %w", err)
					}
					log.Printf("Ingestion complete in %v", time.Since(start))

					if err := materializeVirtuals(indexPath, schema, agentMode); err != nil {
						return fmt.Errorf("materialize virtuals: %w", err)
					}

					mat, mErr := materialize.ForFormat(outFormat)
					if mErr != nil {
						return mErr
					}
					if mErr = mat.Materialize(indexPath, outPath); mErr != nil {
						return fmt.Errorf("materialize (%s): %w", outFormat, mErr)
					}
					log.Printf("Wrote %s (format: %s)", outPath, outFormat)
					return nil
				}

				// SQLite source: eager scan before mount to avoid fuse-t NFS timeouts
				log.Printf("Opening %s (direct SQL backend)...", dataPath)
				sg, err := graph.OpenSQLiteGraph(dataPath, schema, machetmpl.Render)
				if err != nil {
					return fmt.Errorf("open sqlite graph: %w", err)
				}
				defer func() { _ = sg.Close() }()

				sg.SetCallExtractor(pickCallExtractor(sg.DB()))

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

				sg, err := graph.OpenSQLiteGraph(indexPath, schema, machetmpl.Render)
				if err != nil {
					return fmt.Errorf("open indexed graph: %w", err)
				}
				defer func() {
					_ = sg.Close()
					// Keep the index file for incremental re-ingestion on next mount.
				}()

				sg.SetCallExtractor(pickCallExtractor(sg.DB()))
				g = sg
			} else {
				// Writable or non-tree-sitter: MemoryStore + ingestion pipeline
				store := graph.NewMemoryStore()
				resolver := graph.NewSQLiteResolver(machetmpl.Render)
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

		return mountNFS(schema, g, engine, mountPoint, writable, promptContent)
	},
}

// Execute runs the root command.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
