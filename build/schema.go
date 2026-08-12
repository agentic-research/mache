package build

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/agentic-research/mache/api"
	internalingest "github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/leyline"
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

	parsedDB, cleanup, err := parseToTemp(source)
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
	if err := os.Remove(output); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove existing output %s: %w", output, err)
	}
	writer, err := internalingest.NewSQLiteWriter(output)
	if err != nil {
		return fmt.Errorf("create projection output %s: %w", output, err)
	}
	engine := internalingest.NewEngine(topology, writer)
	engine.SetASTWalker(internalingest.NewASTWalker(db))
	if err := engine.Ingest(source); err != nil {
		_ = writer.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close sqlite writer: %w", err)
	}
	return nil
}

func parseToTemp(source string) (string, func(), error) {
	leylineBinary, err := leyline.ResolveBinary(true)
	if err != nil {
		return "", nil, fmt.Errorf("resolve leyline: %w", err)
	}
	leyline.RecordResolved(leylineBinary, "resolved")

	tmpFile, err := os.CreateTemp("", "mache-leyline-*.db")
	if err != nil {
		return "", nil, fmt.Errorf("create temp .db: %w", err)
	}
	tmpPath := tmpFile.Name()
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", nil, fmt.Errorf("close temp .db: %w", err)
	}
	cleanup := func() {
		_ = os.Remove(tmpPath)
		_ = os.Remove(tmpPath + "-wal")
		_ = os.Remove(tmpPath + "-shm")
	}

	command := exec.Command(leylineBinary, "parse", source, "-o", tmpPath)
	if output, err := command.CombinedOutput(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("leyline parse: %w\n%s", err, output)
	}
	return tmpPath, cleanup, nil
}
