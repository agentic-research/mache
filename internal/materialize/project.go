package materialize

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/leylinegraph"
)

// ExportProjection writes the projection of a data source in the requested format.
func ExportProjection(dataPath, outPath, outFormat string, schema *api.Topology) error {
	mat, err := ForFormat(outFormat)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dataPath); err != nil {
		return fmt.Errorf("stat data source: %w", err)
	}

	indexDir, err := os.MkdirTemp("", "mache-out-*")
	if err != nil {
		return fmt.Errorf("create temp index directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(indexDir) }()
	indexPath := filepath.Join(indexDir, "index.db")

	writer, err := ingest.NewSQLiteWriter(indexPath)
	if err != nil {
		return fmt.Errorf("create sqlite writer: %w", err)
	}
	eng := ingest.NewEngine(schema, writer)
	start := time.Now()
	if err := ingestForOut(eng, dataPath, schema); err != nil {
		_ = writer.Close()
		return fmt.Errorf("ingest for --out: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close sqlite writer: %w", err)
	}
	log.Printf("Ingestion complete in %v", time.Since(start))

	if err := leylinegraph.MaterializeVirtuals(indexPath, schema, false); err != nil {
		return fmt.Errorf("materialize virtuals: %w", err)
	}
	if err := mat.Materialize(indexPath, outPath); err != nil {
		return fmt.Errorf("materialize (%s): %w", outFormat, err)
	}
	log.Printf("Wrote %s (format: %s)", outPath, outFormat)
	return nil
}

func ingestForOut(eng *ingest.Engine, dataPath string, schema *api.Topology) error {
	switch filepath.Ext(dataPath) {
	case ".db":
		return eng.Ingest(dataPath)
	case ".git":
		recs, err := ingest.LoadGitCommits(dataPath)
		if err != nil {
			return fmt.Errorf("load git: %w", err)
		}
		return eng.IngestRecords(recs)
	}
	if ingest.SchemaUsesTreeSitter(schema) {
		_, cleanup, err := leylinegraph.AttachLeylineASTWalker(dataPath, eng)
		if err != nil {
			return fmt.Errorf("ley-line parse for source projection: %w", err)
		}
		defer cleanup()
	}
	return eng.Ingest(dataPath)
}
