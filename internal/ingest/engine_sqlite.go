package ingest

import (
	"fmt"
	"log"
	"os"
	"runtime"
	"sync"

	"github.com/agentic-research/mache/graph"
)

// ingestSQLiteStreaming processes a SQLite database using a parallel worker pool.
// Reader goroutine streams rows, workers parse JSON + render templates,
// collector applies nodes to the store. Saturates all CPU cores.
func (e *Engine) ingestSQLiteStreaming(dbPath string) error {
	// Pre-create root directory nodes from schema
	for _, nodeSchema := range e.Schema.Nodes {
		rootNode := &graph.Node{
			ID:   nodeSchema.Name,
			Mode: os.ModeDir | 0o555,
		}
		e.Store.AddNode(rootNode)
		e.Store.AddRoot(rootNode)
	}

	numWorkers := runtime.NumCPU()
	jobs := make(chan recordJob, numWorkers*2)
	results := make(chan recordResult, numWorkers*2)

	// Workers: parse JSON, render templates, build nodes.
	// DiagramFuncMap is safe for concurrent reads (built once, then shared).
	diagramFuncs := e.DiagramFuncMap()
	diagramCache := &e.diagramTmplCache

	var workerWg sync.WaitGroup
	for range numWorkers {
		workerWg.Go(func() {
			w := NewJsonWalker()
			for job := range jobs {
				results <- processRecord(e.Schema, w, dbPath, job, diagramFuncs, diagramCache)
			}
		})
	}

	// Collector: apply nodes to store (single goroutine, no lock contention).
	// Handles dedup for shared directory nodes (e.g. year dirs from temporal sharding)
	// and parent-child links.
	var collectErr error
	var collectWg sync.WaitGroup
	collectWg.Go(func() {
		parentChildSeen := make(map[string]map[string]bool)
		count := 0
		for res := range results {
			count++
			if count%50000 == 0 {
				log.Printf("Processed %d records...", count) // coverage:ignore
			} // coverage:ignore
			if res.err != nil {
				if collectErr == nil { // coverage:ignore
					collectErr = res.err // coverage:ignore
				} // coverage:ignore
				continue // coverage:ignore
			}
			for _, node := range res.nodes {
				// For directory nodes, only create if it doesn't exist yet.
				// Multiple workers may produce the same intermediate dir (e.g. "by-cve/2024").
				// Children are managed exclusively via parentLinks below.
				if node.Mode.IsDir() {
					if _, err := e.Store.GetNode(node.ID); err != nil {
						e.Store.AddNode(node)
					}
				} else {
					e.Store.AddNode(node)
				}
			}
			for _, link := range res.parentLinks {
				if parentChildSeen[link.parentID] == nil {
					parentChildSeen[link.parentID] = make(map[string]bool)
				}
				if !parentChildSeen[link.parentID][link.childID] {
					parentChildSeen[link.parentID][link.childID] = true
					parent, err := e.Store.GetNode(link.parentID)
					if err == nil {
						parent.Children = append(parent.Children, link.childID)
					}
				}
			}
			for _, ref := range res.refLinks {
				if err := e.Store.AddRef(ref.token, ref.nodeID); err != nil {
					if collectErr == nil { // coverage:ignore
						collectErr = fmt.Errorf("add ref %s -> %s: %w", ref.token, ref.nodeID, err) // coverage:ignore
					} // coverage:ignore
				}
			}
		}
		log.Printf("Processed %d records total.", count)
	})

	// Reader: stream raw rows from SQLite (I/O bound, single goroutine)
	readErr := StreamSQLiteRaw(dbPath, func(id, raw string) error {
		jobs <- recordJob{recordID: id, raw: raw}
		return nil
	})

	close(jobs)     // signal workers: no more jobs
	workerWg.Wait() // wait for all workers to finish
	close(results)  // signal collector: no more results
	collectWg.Wait()

	if collectErr != nil {
		return collectErr // coverage:ignore
	} // coverage:ignore
	return readErr
}
