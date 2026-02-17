package ingest

import (
	"fmt"
	"time"
)

// IngestRecords processes in-memory records (e.g. from Git).
func (e *Engine) IngestRecords(records []any) error {
	modTime := time.Now()

	// We treat 'records' as the root data object for the schema.
	// The schema usually has a root selector like "$[*]" which iterates the list.
	walker := NewJsonWalker()
	for _, nodeSchema := range e.Schema.Nodes {
		if err := e.processNode(nodeSchema, walker, records, "", "", modTime, e.Store, nil); err != nil {
			return fmt.Errorf("failed to process schema node %s: %w", nodeSchema.Name, err)
		}
	}
	return nil
}
