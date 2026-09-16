package build

import (
	"database/sql"
	"fmt"
	"os"
	"strings"

	"github.com/agentic-research/mache/api"
	internalingest "github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/leylinegraph"
	publicschema "github.com/agentic-research/mache/schema"

	_ "modernc.org/sqlite"
)

// ParseWithSchema parses source with the pinned leyline binary and projects
// the resulting AST through topology into output. It is the library equivalent
// of `mache build --schema` for callers that already hold a topology.
func ParseWithSchema(source, output string, topology *api.Topology) error {
	return parseWithSchema(source, output, topology, nil)
}

// ParseWithSchemaRef resolves a bundled preset name or schema file relative to
// baseDir, then parses and projects source into output.
func ParseWithSchemaRef(source, output, ref, baseDir string) error {
	resolved, err := publicschema.Resolve(ref, baseDir)
	if err != nil {
		return fmt.Errorf("load schema: %w", err)
	}
	if resolved.Topology == nil {
		return fmt.Errorf("load schema: schema reference is empty")
	}
	return parseWithSchema(source, output, resolved.Topology, resolved.Languages)
}

func parseWithSchema(source, output string, topology *api.Topology, extraLanguages []string) error {
	if topology == nil {
		return fmt.Errorf("build with schema: topology is nil")
	}

	// One parse implementation, not two. This used to be a local parseToTemp
	// that was a near-verbatim copy of AutoInvokeLeylineParse: same
	// os.CreateTemp("", "mache-leyline-*.db"), same -wal/-shm cleanup, same
	// `leyline parse -o` shell-out. The copy is why the persistent parse cache
	// did nothing for `mache build --schema` — the shared function grew the
	// cache and this path never called it (mache-80a851).
	parsedDB, cleanup, err := leylinegraph.AutoInvokeLeylineParse(source)
	if err != nil {
		return err
	}
	defer cleanup()

	db, err := openParsedDatabase(parsedDB)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if err := requireSchemaCoverage(db, topology, source, extraLanguages); err != nil {
		return err
	}
	return projectTopology(db, topology, source, output)
}

func openParsedDatabase(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open leyline parse output %s: %w", path, err)
	}
	internalingest.TuneReadConnForBuild(db)
	return db, nil
}

func requireSchemaCoverage(db *sql.DB, topology *api.Topology, source string, extraLanguages []string) error {
	gaps, err := schemaCoverageGaps(db, topology, source, extraLanguages)
	if err != nil {
		return fmt.Errorf("leyline schema coverage probe: %w", err)
	}
	if len(gaps) > 0 {
		return fmt.Errorf(
			"cannot project schema language(s) %s: the pinned ley-line has no grammar for %s, so it "+
				"parsed no such source. In-process tree-sitter was removed in ADR-0012 step 4, so there "+
				"is no fallback parser — wait for ley-line to add these grammars, or drop them from the schema",
			strings.Join(gaps, ", "), strings.Join(gaps, "/"))
	}
	return nil
}

func projectTopology(db *sql.DB, topology *api.Topology, source, output string) error {
	fingerprint, err := projectionFingerprint(topology)
	if err != nil {
		return err
	}

	// Reuse the previous projection when an identical build wrote it, so only
	// the files that CHANGED are re-projected. Everything that makes that safe
	// landed first: a skipped file's construct IDs are seeded rather than left
	// free for a changed file to take (mache-7a7919), and a file that has since
	// been deleted has its nodes reaped (mache-31abc0).
	index := reusableIndex(output, fingerprint)
	if index == nil {
		// No reusable projection: start clean. A partial merge into a db some
		// other build wrote is the one outcome worse than a slow build.
		if err := os.Remove(output); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove existing output %s: %w", output, err)
		}
	}

	writer, err := internalingest.NewSQLiteWriter(output)
	if err != nil {
		return fmt.Errorf("create projection output %s: %w", output, err)
	}
	engine := internalingest.NewEngine(topology, writer)
	if index != nil {
		engine.SetFileIndex(index)
	}
	engine.SetASTWalker(internalingest.NewASTWalker(db))
	if err := engine.Ingest(source); err != nil {
		_ = writer.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close sqlite writer: %w", err)
	}
	return writeFingerprint(output, fingerprint)
}
