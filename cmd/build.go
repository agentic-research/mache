package cmd

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/lang"
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
		return runBuildViaLeyline(source, output, schemaPath != "" /* schemaExplicit */)
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
// The leyline path HONORS --schema (mache-73b885) by running the pure-Go
// Engine+ASTWalker projection over the leyline-parsed _ast db. In-process
// tree-sitter was removed in ADR-0012 step 4 (mache-37ae8b), so this is now
// the only projector; ASTWalker correctness is covered by `task test:ast`.
//
// schemaExplicit preserves a loudness contract for the one case leyline
// genuinely can't serve: a schema language leyline has no grammar for (it
// parses most of the 28 registry languages — all but cue today) would
// otherwise project HOLLOW category dirs with zero warnings — the coverage
// guard errors on the explicit backend and warns on auto.
func runBuildViaLeyline(source, output string, schemaExplicit bool) error {
	if schemaPath != "" {
		schema, err := resolveSchema(schemaPath, ".")
		if err != nil {
			return fmt.Errorf("load schema: %w", err)
		}
		// A preset ref ("go", "sql", "terraform", ...) IS a language name,
		// and the preset JSONs carry no Node.Language hints — feed the ref
		// to the coverage guard so `--schema sql` fails loudly when the
		// pinned leyline has no sql grammar (#524 re-review: the hint-based
		// guard alone let the preset case project hollow).
		var presetLangs []string
		if _, ok := presetSchemas[schemaPath]; ok {
			if l := lang.ForName(schemaPath); l != nil {
				presetLangs = []string{l.Name}
			}
		}
		return runBuildViaLeylineSchema(source, output, schema, schemaExplicit, presetLangs)
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

// runBuildViaLeylineSchema builds a schema-shaped .db (mache-73b885):
// `leyline parse` produces a temp _ast/_source db, and the pure-Go
// Engine+ASTWalker projection runs the schema over it into a fresh
// SQLiteWriter output. In-process tree-sitter was removed in ADR-0012 step 4,
// so ASTWalker is the sole projector (correctness: `task test:ast`). The
// output is the standard schema shape (nodes/node_defs/node_refs per the
// schema; no _ast table — the temp db is discarded), so downstream consumers
// see no shape change, only a CGO-free producer.
func runBuildViaLeylineSchema(source, output string, schema *api.Topology, schemaExplicit bool, extraLangs []string) error {
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
	// One-shot build over a private temp _ast db mache exclusively owns — opt
	// into the aggressive read tuning (single conn + EXCLUSIVE lock + mmap).
	// Safe here precisely because nothing else touches tmpPath; the served
	// path deliberately does NOT do this (mache-010123).
	ingest.TuneReadConnForBuild(db)

	// Coverage guard: a schema language leyline has no grammar for
	// yields ZERO _ast rows while its source files silently land in
	// _project_files — a hollow projection warnIfEmptyBuild can't see
	// (empty category dirs still count as nodes). Keep the pre-73b885
	// loudness split: hard error when the user explicitly picked this
	// backend, loud warning on auto.
	if gaps, gerr := leylineSchemaCoverageGaps(db, schema, source, extraLangs); gerr != nil {
		return fmt.Errorf("leyline schema coverage probe: %w", gerr)
	} else if len(gaps) > 0 {
		if schemaExplicit {
			return fmt.Errorf(
				"cannot project schema language(s) %s: the pinned ley-line has no grammar for %s, so it "+
					"parsed no such source. In-process tree-sitter was removed in ADR-0012 step 4, so there "+
					"is no fallback parser — wait for ley-line to add these grammars, or drop them from the schema",
				strings.Join(gaps, ", "), strings.Join(gaps, "/"))
		}
		log.Printf("WARNING: ley-line has no grammar for schema language(s) %s — "+
			"their category dirs will be EMPTY and the files land in _project_files/. "+
			"There is no in-process fallback (ADR-0012 step 4); wait for ley-line to add these grammars.",
			strings.Join(gaps, ", "))
	}

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
