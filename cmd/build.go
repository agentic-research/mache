package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/lattice"
	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
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
			// Explicit schema file — load it
			loaded, err := resolveSchema(schemaPath, filepath.Dir(schemaPath))
			if err != nil {
				return fmt.Errorf("load schema: %w", err)
			}
			schema = loaded
		} else {
			// No schema — infer via FCA from source tree
			inf := &lattice.Inferrer{Config: lattice.DefaultInferConfig()}
			fmt.Println("Inferring schema...")

			// Walk source to find the first .go file for bootstrap inference
			if walkErr := filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if filepath.Ext(path) == ".go" {
					content, _ := os.ReadFile(path)
					parser := sitter.NewParser()
					parser.SetLanguage(golang.GetLanguage())
					tree, _ := parser.ParseCtx(context.Background(), nil, content)
					if tree != nil {
						schema, _ = inf.InferFromTreeSitter(tree.RootNode())
					}
					return filepath.SkipDir // Stop after first
				}
				return nil
			}); walkErr != nil && walkErr != filepath.SkipDir {
				return fmt.Errorf("walk source: %w", walkErr)
			}

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
		fmt.Printf("Building %s from %s...\n", output, source)
		if err := engine.Ingest(source); err != nil {
			return err
		}
		fmt.Printf("Done in %v.\n", time.Since(start))
		return nil
	},
}

func init() {
	rootCmd.AddCommand(buildCmd)
}
