package cmd

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/agentic-research/mache/api"
	publicbuild "github.com/agentic-research/mache/build"
	"github.com/agentic-research/mache/internal/leylinegraph"
	"github.com/spf13/cobra"
)

// buildBackend is retained as a documented no-op for backward compatibility.
// ADR-0012 step 4 removed in-process CGO tree-sitter: ley-line is now the
// universal parser, so there is no backend to select. The flag is accepted
// (so existing invocations don't error) but ignored.
var buildBackend string

var buildCmd = &cobra.Command{
	Use:   "build [source] [output.db]",
	Short: "Build a Mache SQLite database from a source directory",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		source := args[0]
		output := args[1]

		if buildBackend != "" && buildBackend != "auto" && buildBackend != "leyline" {
			log.Printf("--backend=%q is ignored: ley-line is the sole parser since ADR-0012 step 4", buildBackend)
		}

		// ley-line is the universal parser (ADR-0012 step 4). Resolve — and
		// if absent, auto-download — the pinned binary; a genuinely missing
		// leyline (offline AND MACHE_NO_LEYLINE) is a HARD ERROR, not a
		// silent fallback: in-process tree-sitter no longer exists.
		return runBuildViaLeyline(source, output)
	},
}

func init() {
	// schemaPath is shared with rootCmd (mount path) and other subcommands.
	// `mache build` needs its own flag binding because rootCmd's --schema
	// lives on Flags(), not PersistentFlags(), so it doesn't propagate to
	// children.
	buildCmd.Flags().StringVarP(&schemaPath, "schema", "s", "", "Path to topology schema (defaults to the ley-line-parsed _ast projection)")
	buildCmd.Flags().StringVar(&buildBackend, "backend", "auto", "Deprecated no-op: ley-line is the sole parser since ADR-0012 step 4. Accepted for backward compatibility and ignored.")
	rootCmd.AddCommand(buildCmd)
}

// runBuildViaLeyline runs the sole parser backend. Without a schema it copies
// leyline's raw database to output; with a schema it delegates to the public
// build package's projection pipeline.
//
// Errors are surfaced as-is; callers shouldn't retry with the
// in-process backend silently — that masks misconfiguration.
// If the user explicitly passed --backend=leyline, leyline being
// missing is a real error worth reporting, not a fallback trigger.
//
// The leyline path HONORS --schema (mache-73b885) by running the pure-Go
// Engine+ASTWalker projection over the leyline-parsed _ast db. In-process
// tree-sitter was removed in ADR-0012 step 4 (mache-37ae8b), so this is now
// the only projector; ASTWalker correctness is covered by `task test:ast`.
func runBuildViaLeyline(source, output string) error {
	if schemaPath != "" {
		start := time.Now()
		log.Printf("Building %s from %s (leyline parse + schema projection)...", output, source)
		if err := publicbuild.ParseWithSchemaRef(source, output, schemaPath, "."); err != nil {
			return fmt.Errorf("leyline backend: %w", err)
		}
		log.Printf("Done in %v.", time.Since(start))
		_ = writeBuildMetadata(output, "leyline+schema")
		warnIfEmptyBuild(output, source, "leyline")
		return nil
	}
	tmpPath, cleanup, err := leylinegraph.AutoInvokeLeylineParse(source)
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

// runBuildViaLeylineSchema builds a schema-shaped .db (mache-73b885):
// `leyline parse` produces a temp _ast/_source db, and the pure-Go
// Engine+ASTWalker projection runs the schema over it into a fresh
// SQLiteWriter output. In-process tree-sitter was removed in ADR-0012 step 4,
// so ASTWalker is the sole projector (correctness: `task test:ast`). The
// output is the standard schema shape (nodes/node_defs/node_refs per the
// schema; no _ast table — the temp db is discarded), so downstream consumers
// see no shape change, only a CGO-free producer.
func runBuildViaLeylineSchema(source, output string, schema *api.Topology) error {
	start := time.Now()
	log.Printf("Building %s from %s (leyline parse + schema projection)...", output, source)
	if err := publicbuild.ParseWithSchema(source, output, schema); err != nil {
		return fmt.Errorf("leyline backend: %w", err)
	}
	log.Printf("Done in %v.", time.Since(start))
	_ = writeBuildMetadata(output, "leyline+schema")
	warnIfEmptyBuild(output, source, "leyline")
	return nil
}
