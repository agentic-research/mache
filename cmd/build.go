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
			// Explicit schema file — load it
			loaded, err := resolveSchema(schemaPath, filepath.Dir(schemaPath))
			if err != nil {
				return fmt.Errorf("load schema: %w", err)
			}
			schema = loaded
		} else {
			// No schema — infer via FCA from source tree
			inf := &lattice.Inferrer{Config: lattice.DefaultInferConfig()}
			log.Println("Inferring schema...")

			// Walk source to find the first .go file for bootstrap inference
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
					return nil // try next file
				}
				parser := sitter.NewParser()
				parser.SetLanguage(lang.ForName("go").Grammar())
				tree, parseErr := parser.ParseCtx(context.Background(), nil, content)
				if parseErr != nil {
					log.Printf("schema inference: parse %s: %v", path, parseErr)
					return nil // try next file
				}
				if tree != nil {
					var inferErr error
					schema, inferErr = inf.InferFromTreeSitter(tree.RootNode())
					if inferErr != nil {
						log.Printf("schema inference: infer from %s: %v", path, inferErr)
					}
				}
				if schema != nil {
					return filepath.SkipDir // Stop after first success
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
		log.Printf("Building %s from %s...", output, source)
		if err := engine.Ingest(source); err != nil {
			return err
		}
		log.Printf("Done in %v.", time.Since(start))
		return nil
	},
}

func init() {
	rootCmd.AddCommand(buildCmd)
}
