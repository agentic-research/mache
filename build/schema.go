package build

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/fsutil"
	internalingest "github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/leylinegraph"
	publicschema "github.com/agentic-research/mache/schema"

	_ "modernc.org/sqlite"
)

// Option configures ParseWithSchema and ParseWithSchemaRef.
type Option func(*parseOptions)

type parseOptions struct {
	finalize func(path string) error
}

// WithFinalize runs fn against the finished database before it is published at
// output. fn receives the database's private path, not output: anything it
// writes lands in the same atomic publish as the projection. Writing to output
// after the build returns would modify a file readers may already have open.
//
// An error from fn abandons the build and leaves output as it was.
func WithFinalize(fn func(path string) error) Option {
	return func(o *parseOptions) { o.finalize = fn }
}

// ParseWithSchema parses source with the pinned leyline binary and projects
// the resulting AST through topology into output. It is the library equivalent
// of `mache build --schema` for callers that already hold a topology.
//
// output is published atomically: a reader sees the previous database or the
// finished one, never a partial or missing file, and a failed build leaves the
// previous database in place.
func ParseWithSchema(source, output string, topology *api.Topology, opts ...Option) error {
	return parseWithSchema(source, output, topology, nil, opts)
}

// ParseWithSchemaRef resolves a bundled preset name or schema file relative to
// baseDir, then parses and projects source into output.
func ParseWithSchemaRef(source, output, ref, baseDir string, opts ...Option) error {
	resolved, err := publicschema.Resolve(ref, baseDir)
	if err != nil {
		return fmt.Errorf("load schema: %w", err)
	}
	if resolved.Topology == nil {
		return fmt.Errorf("load schema: schema reference is empty")
	}
	return parseWithSchema(source, output, resolved.Topology, resolved.Languages, opts)
}

func parseWithSchema(source, output string, topology *api.Topology, extraLanguages []string, opts []Option) error {
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
	var o parseOptions
	for _, opt := range opts {
		opt(&o)
	}
	return publishProjection(db, topology, source, output, o.finalize)
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

// publishProjection projects into a private copy of output and publishes it
// in one rename (mache-21c210).
//
// It used to project into output itself, in place, through a writer running
// journal_mode=MEMORY. A reader opening output mid-build saw a half-projected
// database, or none at all in the window after a full rebuild removed it; and
// a crash mid-write left output corrupt, because a MEMORY journal does not
// survive the process. Publishing a finished file removes all three, and is
// what makes it sound for a reader to treat a published database as immutable.
func publishProjection(db *sql.DB, topology *api.Topology, source, output string, finalize func(string) error) error {
	fingerprint, err := projectionFingerprint(topology)
	if err != nil {
		return err
	}

	// Reuse the previous projection when an identical build wrote it, so only
	// the files that CHANGED are re-projected. Everything that makes that safe
	// landed first: a skipped file's construct IDs are seeded rather than left
	// free for a changed file to take (mache-7a7919), and a file that has since
	// been deleted has its nodes reaped (mache-31abc0).
	//
	// Reuse seeds the private copy from output. Without reuse the copy starts
	// empty: a partial merge into a db some other build wrote is the one
	// outcome worse than a slow build.
	index := reusableIndex(output, fingerprint)
	seed := ""
	if index != nil {
		seed = output
	}
	return fsutil.Publish(output, seed, func(path string) error {
		if err := projectTopology(db, topology, source, path, index); err != nil {
			return err
		}
		if err := writeFingerprint(path, fingerprint); err != nil {
			return err
		}
		if finalize != nil {
			return finalize(path)
		}
		return nil
	})
}

// projectTopology runs the projection into output, reusing index when it is
// non-nil.
func projectTopology(db *sql.DB, topology *api.Topology, source, output string, index map[string]internalingest.FileIndexEntry) error {
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
	return nil
}
