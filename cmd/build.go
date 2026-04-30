package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/lang"
	"github.com/agentic-research/mache/internal/lattice"
	sitter "github.com/smacker/go-tree-sitter"
	"github.com/spf13/cobra"
)

// buildBackend selects the parsing backend for `mache build`.
//   - "auto" (default): in-process tree-sitter via SitterWalker — current behavior.
//   - "leyline": shell out to `leyline parse`. Requires leyline on PATH or
//     ~/.mache/bin/leyline. Produces a richer .db (carries `_ast`, `_source`,
//     `_imports`, `_lsp*`) that all 9 find_smells rules can run against.
//     Schema projection at query time (via SQLiteGraph) — the --schema flag
//     is ignored on this path because leyline doesn't apply it.
//
// Opt-in for now per ADR-0012 step 3. A future PR makes this auto-detect
// and prefer leyline when available.
var buildBackend string

var buildCmd = &cobra.Command{
	Use:   "build [source] [output.db]",
	Short: "Build a Mache SQLite database from a source directory",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		source := args[0]
		output := args[1]

		// Backend dispatch (ADR-0012). When --backend=leyline, shell out
		// to `leyline parse` and copy the result to `output`. Schema
		// flag is ignored on this path — leyline produces raw _ast /
		// _source / nodes / node_refs / node_defs; schema projection
		// happens at serve/mount time via SQLiteGraph.
		if buildBackend == "leyline" {
			return runBuildViaLeyline(source, output)
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
		defer func() { _ = writer.Close() }()

		// 3. Setup Engine
		engine := ingest.NewEngine(schema, writer)

		// 4. Ingest
		start := time.Now()
		log.Printf("Building %s from %s...", output, source)
		if err := engine.Ingest(source); err != nil {
			return err
		}
		log.Printf("Done in %v.", time.Since(start))
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
	buildCmd.Flags().StringVar(&buildBackend, "backend", "auto", "Parsing backend: 'auto' (in-process tree-sitter, current default) or 'leyline' (delegate to ley-line-open's `leyline parse`; produces richer .db with _ast/_source/_lsp* and is the future default per ADR-0012)")
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
func runBuildViaLeyline(source, output string) error {
	if schemaPath != "" {
		log.Printf("--backend=leyline: --schema=%q is ignored on this path; schema projection happens at serve/mount time", schemaPath)
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
	return nil
}
