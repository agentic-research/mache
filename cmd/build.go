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

var buildCmd = &cobra.Command{
	Use:   "build [source] [output.db]",
	Short: "Build a Mache SQLite database from a source directory",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		source := args[0]
		output := args[1]

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
			// No schema — infer via FCA from source tree
			inf := &lattice.Inferrer{Config: lattice.DefaultInferConfig()}
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
	rootCmd.AddCommand(buildCmd)
}
