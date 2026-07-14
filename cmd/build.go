package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/lang"
	"github.com/agentic-research/mache/internal/lattice"
	"github.com/agentic-research/mache/internal/leyline"
	sitter "github.com/smacker/go-tree-sitter"
	"github.com/spf13/cobra"
)

// buildBackend selects the parsing backend for `mache build`.
//   - "auto" (default): prefer leyline when on PATH or at
//     ~/.mache/bin/leyline; otherwise fall back to in-process
//     tree-sitter. The detection runs once per invocation; users
//     without leyline see today's behavior unchanged.
//   - "leyline": force the leyline path. Errors if leyline is missing
//     (no silent fallback — explicit-flag misconfiguration should be loud).
//   - "tree-sitter": force the in-process path even if leyline is
//     available. Escape hatch for users debugging the legacy ingest
//     or comparing outputs.
//
// ADR-0012 step 3 — auto-detect with leyline preference. Step 4
// (CGO removal commitment point) deletes "tree-sitter" / "auto"
// fallback once leyline is bundled in mache releases (mache-33dc5f).
var buildBackend string

var buildCmd = &cobra.Command{
	Use:   "build [source] [output.db]",
	Short: "Build a Mache SQLite database from a source directory",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		source := args[0]
		output := args[1]

		// Backend dispatch (ADR-0012):
		//   leyline      → force leyline path (errors if missing)
		//   tree-sitter  → force in-process path (skip detection)
		//   auto / ""    → prefer leyline when available, else in-process
		switch buildBackend {
		case "leyline":
			return runBuildViaLeyline(source, output, true /* schemaExplicit */)
		case "tree-sitter":
			// Fall through to in-process path below.
		default: // "auto" or empty
			// Prefer leyline (ADR-0012): resolve or auto-download it, then
			// build via the LLO path (all rules, _ast-bearing .db). Fall back
			// to in-process tree-sitter only when leyline is genuinely
			// unavailable — offline, or MACHE_NO_LEYLINE set.
			if _, err := leyline.ResolveBinary(true); err == nil {
				log.Printf("--backend=auto: using leyline; pass --backend=tree-sitter to force in-process")
				return runBuildViaLeyline(source, output, false /* schemaExplicit */)
			} else {
				log.Printf("--backend=auto: leyline unavailable (%v) — using in-process tree-sitter", err)
			}
		}

		// Load or infer schema. Falls back to FCA inference when no schema file is provided.
		var schema *api.Topology
		if schemaPath != "" {
			// Explicit schema file — load it. resolveSchema joins
			// schemaRef with configDir when schemaRef is relative,
			// so passing filepath.Dir(schemaPath) double-prepends
			// the directory ("examples" + "examples/go-schema.json"
			// = "examples/examples/..."). The schemaPath flag is
			// already relative-to-cwd or absolute by user intent;
			// pass "." so resolveSchema only touches the path when
			// it's preset-ref (no slash).
			loaded, err := resolveSchema(schemaPath, ".")
			if err != nil {
				return fmt.Errorf("load schema: %w", err)
			}
			schema = loaded
		} else {
			// No schema — infer via FCA from source tree.
			//
			// AST records are dense: a single Go file produces hundreds
			// of named-node records (function/method/var/expression/...).
			// The default SampleSize of 1000 is sized for sparse JSON
			// records — for AST it under-samples low-frequency node
			// kinds. With ~32 files × ~1300 records ≈ 40k records, only
			// ~2 method_declaration records would land in the reservoir
			// and the closure-based detection of methods/ silently fails
			// (mache-5d1o). Bump to 100k so AST inference sees enough
			// of every kind. Memory is still bounded by maxFiles below.
			cfg := lattice.DefaultInferConfig()
			cfg.SampleSize = 100_000
			// Tag the inferred schema with its source language. Without
			// this, every node has Language='' which makes the engine's
			// filterNodesByLanguage match the selector against every
			// language — e.g. JS function_declarations match the
			// inferred-from-Go selector and produce orphan construct
			// dirs (no source/ast.json/doc children because the JS
			// match shape doesn't render the Go templates cleanly).
			// We currently bootstrap inference from Go files only;
			// when other-language bootstraps land, this should pick
			// up the actual language being sampled.
			cfg.Language = "go"
			inf := &lattice.Inferrer{Config: cfg}
			log.Println("Inferring schema...")

			// Walk source and parse multiple .go files. Single-file
			// inference misses node types absent from that file —
			// e.g. parsing a file with only function_declarations
			// yields no methods/, even when the project has receiver
			// methods (mache-5d1o). Cap at maxFiles to keep inference
			// fast on large repos; FCA only needs enough samples to
			// surface the closures, not exhaustive coverage.
			const maxFiles = 32
			parser := sitter.NewParser()
			parser.SetLanguage(lang.ForName("go").Grammar())
			var roots []*sitter.Node
			var trees []*sitter.Tree // keep alive for root lifetime
			if walkErr := filepath.WalkDir(source, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() && path != source && ingest.ShouldSkipDir(d.Name()) {
					return filepath.SkipDir
				}
				if d.IsDir() || filepath.Ext(path) != ".go" {
					return nil
				}
				content, readErr := os.ReadFile(path)
				if readErr != nil {
					log.Printf("schema inference: read %s: %v", path, readErr)
					return nil
				}
				tree, parseErr := parser.ParseCtx(context.Background(), nil, content)
				if parseErr != nil {
					log.Printf("schema inference: parse %s: %v", path, parseErr)
					return nil
				}
				if tree != nil {
					roots = append(roots, tree.RootNode())
					trees = append(trees, tree)
				}
				if len(roots) >= maxFiles {
					return filepath.SkipDir
				}
				return nil
			}); walkErr != nil && walkErr != filepath.SkipDir {
				return fmt.Errorf("walk source: %w", walkErr)
			}

			if len(roots) > 0 {
				inferred, inferErr := inf.InferFromTreeSitterRoots(roots...)
				if inferErr != nil {
					log.Printf("schema inference: %v", inferErr)
				} else {
					schema = inferred
				}
			}
			_ = trees // hold trees alive until inference returns

			if schema == nil {
				schema = &api.Topology{Version: "v1alpha1"}
			}
		}

		// 2. Setup Writer
		_ = os.Remove(output) // Overwrite
		writer, err := ingest.NewSQLiteWriter(output)
		if err != nil {
			return err
		}

		// 3. Setup Engine
		engine := ingest.NewEngine(schema, writer)

		// 4. Ingest
		start := time.Now()
		log.Printf("Building %s from %s...", output, source)
		if err := engine.Ingest(source); err != nil {
			_ = writer.Close()
			return err
		}
		log.Printf("Done in %v.", time.Since(start))

		// Close the writer before stamping metadata — the writer holds
		// the schema lock via its open transaction, so opening a second
		// connection here would block on it. The schema marker plus the
		// empty-build check both need fresh reads, so close, then probe.
		//
		// Close errors are FATAL here: they can signal a failed final
		// commit/flush (disk-full, partial transaction, etc.), in which
		// case the .db on disk may be corrupt or truncated. `mache build`
		// exiting 0 with a broken .db would cause every downstream tool
		// (mache serve, find_smells) to fail mysteriously. Match the
		// fatal-on-close treatment in cmd/mount.go:312 / mount.go:393.
		if err := writer.Close(); err != nil {
			return fmt.Errorf("close sqlite writer: %w", err)
		}
		_ = writeBuildMetadata(output, "tree-sitter")
		warnIfEmptyBuild(output, source, "tree-sitter")
		return nil
	},
}

func init() {
	// schemaPath is shared with rootCmd (mount path) and other subcommands.
	// `mache build` needs its own flag binding because rootCmd's --schema
	// lives on Flags(), not PersistentFlags(), so it doesn't propagate to
	// children. Without this, users can't pass an explicit schema to build
	// and have to rely on FCA inference — which doesn't yet produce a
	// methods/ root for Go (mache-5d1o).
	buildCmd.Flags().StringVarP(&schemaPath, "schema", "s", "", "Path to topology schema (defaults to FCA inference)")
	buildCmd.Flags().StringVar(&buildBackend, "backend", "auto", "Parsing backend: 'auto' (prefer leyline when on PATH, else in-process tree-sitter), 'leyline' (force leyline; errors if missing), 'tree-sitter' (force in-process even when leyline is available). Per ADR-0012; step 4 deletes the in-process fallback.")
	rootCmd.AddCommand(buildCmd)
}

// runBuildViaLeyline implements --backend=leyline: shells out to
// `leyline parse <source> -o <tmp.db>` and copies the result to
// `output`. The ADR-0012 step 3 path that future PRs will make
// the default.
//
// Errors are surfaced as-is; callers shouldn't retry with the
// in-process backend silently — that masks misconfiguration.
// If the user explicitly passed --backend=leyline, leyline being
// missing is a real error worth reporting, not a fallback trigger.
//
// schemaExplicit is retained for call-site symmetry but no longer
// gates anything: the leyline path now HONORS --schema (mache-73b885)
// by running the pure-Go Engine+ASTWalker projection over the
// leyline-parsed _ast db — the same composition the leyline parity
// gate (internal/ingest/ast_parity_test.go) asserts is byte-identical
// to the in-process tree-sitter projection. Before this, --schema was
// warn-and-ignored on the auto path and an error on the explicit
// path, which forced schema users onto the CGO backend (the composite
// action's `--backend tree-sitter` workaround).
func runBuildViaLeyline(source, output string, _ bool) error {
	if schemaPath != "" {
		schema, err := resolveSchema(schemaPath, ".")
		if err != nil {
			return fmt.Errorf("load schema: %w", err)
		}
		return runBuildViaLeylineSchema(source, output, schema)
	}
	tmpPath, cleanup, err := autoInvokeLeylineParse(source)
	if err != nil {
		return fmt.Errorf("leyline backend: %w", err)
	}
	defer cleanup()

	// Move the temp .db to the output path. copyFile (cmd/utils.go)
	// uses copy + close rather than rename so we don't fail across
	// filesystems (TMPDIR may be on a different mount than the
	// target). Pre-truncate the destination to match the prior
	// `os.Remove(output)` behavior in the auto path.
	_ = os.Remove(output)
	if err := copyFile(tmpPath, output); err != nil {
		return fmt.Errorf("copy leyline output to %s: %w", output, err)
	}
	log.Printf("Built %s from %s via leyline", output, source)
	_ = writeBuildMetadata(output, "leyline")
	warnIfEmptyBuild(output, source, "leyline")
	return nil
}

// runBuildViaLeylineSchema builds a schema-shaped .db WITHOUT the CGO
// tree-sitter backend (mache-73b885): `leyline parse` produces a temp
// _ast/_source db, and the existing pure-Go Engine+ASTWalker projection
// runs the schema over it into a fresh SQLiteWriter output — the exact
// composition internal/ingest/ast_parity_test.go proves byte-identical
// to the Engine+SitterWalker path. The output matches what
// `--backend=tree-sitter --schema X` produces today (nodes/node_defs/
// node_refs per the schema; no _ast table — the temp db is discarded),
// so downstream consumers see no shape change, only a CGO-free producer.
func runBuildViaLeylineSchema(source, output string, schema *api.Topology) error {
	tmpPath, cleanup, err := autoInvokeLeylineParse(source)
	if err != nil {
		return fmt.Errorf("leyline backend: %w", err)
	}
	defer cleanup()

	db, err := sql.Open("sqlite", tmpPath)
	if err != nil {
		return fmt.Errorf("open leyline parse output %s: %w", tmpPath, err)
	}
	defer func() { _ = db.Close() }()

	_ = os.Remove(output) // Overwrite, matching the in-process path.
	writer, err := ingest.NewSQLiteWriter(output)
	if err != nil {
		return err
	}
	engine := ingest.NewEngine(schema, writer)
	engine.SetASTWalker(ingest.NewASTWalker(db))

	start := time.Now()
	log.Printf("Building %s from %s (leyline parse + schema projection)...", output, source)
	if err := engine.Ingest(source); err != nil {
		_ = writer.Close()
		return err
	}
	log.Printf("Done in %v.", time.Since(start))

	// Close-before-stamp for the same reason as the in-process path:
	// the writer's open transaction holds the schema lock, and a close
	// failure can mean a truncated .db — fatal, not warnable.
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close sqlite writer: %w", err)
	}
	_ = writeBuildMetadata(output, "leyline+schema")
	warnIfEmptyBuild(output, source, "leyline")
	return nil
}
