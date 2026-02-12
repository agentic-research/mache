package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"text/template"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/python"
)

const inlineThreshold = 4096

// IngestionTarget combines Graph reading with writing capabilities.
type IngestionTarget interface {
	graph.Graph
	AddNode(n *graph.Node)
	AddRoot(n *graph.Node)
}

// Engine drives the ingestion process.
type Engine struct {
	Schema *api.Topology
	Store  IngestionTarget
}

// --- Parallel ingestion types ---

// recordJob is sent from the SQLite reader to worker goroutines.
type recordJob struct {
	recordID string
	raw      string
}

// recordResult is the output from a worker: all nodes for one record.
type recordResult struct {
	nodes       []*graph.Node
	parentLinks []parentLink
	err         error
}

type parentLink struct {
	childID  string
	parentID string
}

func NewEngine(schema *api.Topology, store IngestionTarget) *Engine {
	return &Engine{
		Schema: schema,
		Store:  store,
	}
}

// Ingest processes a file or directory.
func (e *Engine) Ingest(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	if info.IsDir() {
		return filepath.Walk(path, func(p string, d os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				return e.ingestFile(p)
			}
			return nil
		})
	}
	return e.ingestFile(path)
}

func (e *Engine) ingestFile(path string) error {
	ext := filepath.Ext(path)

	switch ext {
	case ".db":
		return e.ingestSQLiteStreaming(path)
	case ".json":
		return e.ingestJSON(path)
	case ".py":
		return e.ingestTreeSitter(path, python.GetLanguage())
	case ".go":
		return e.ingestTreeSitter(path, golang.GetLanguage())
	default:
		return nil // Skip unsupported files
	}
}

func (e *Engine) ingestJSON(path string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var data any
	if err := json.Unmarshal(content, &data); err != nil {
		return fmt.Errorf("failed to parse json %s: %w", path, err)
	}
	walker := NewJsonWalker()
	for _, nodeSchema := range e.Schema.Nodes {
		if err := e.processNode(nodeSchema, walker, data, ""); err != nil {
			return fmt.Errorf("failed to process schema node %s: %w", nodeSchema.Name, err)
		}
	}
	return nil
}

func (e *Engine) ingestTreeSitter(path string, lang *sitter.Language) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	parser := sitter.NewParser()
	parser.SetLanguage(lang)
	tree, err := parser.ParseCtx(context.Background(), nil, content)
	if err != nil {
		return fmt.Errorf("failed to parse %s: %w", path, err)
	}
	walker := NewSitterWalker()
	root := SitterRoot{Node: tree.RootNode(), Source: content, Lang: lang}
	for _, nodeSchema := range e.Schema.Nodes {
		if err := e.processNode(nodeSchema, walker, root, ""); err != nil {
			return fmt.Errorf("failed to process schema node %s: %w", nodeSchema.Name, err)
		}
	}
	return nil
}

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

	// Workers: parse JSON, render templates, build nodes
	var workerWg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		workerWg.Add(1)
		go func() {
			defer workerWg.Done()
			w := NewJsonWalker()
			for job := range jobs {
				results <- processRecord(e.Schema, w, dbPath, job)
			}
		}()
	}

	// Collector: apply nodes to store (single goroutine, no lock contention).
	// Handles dedup for shared directory nodes (e.g. year dirs from temporal sharding)
	// and parent-child links.
	var collectErr error
	var collectWg sync.WaitGroup
	collectWg.Add(1)
	go func() {
		defer collectWg.Done()
		parentChildSeen := make(map[string]map[string]bool)
		for res := range results {
			if res.err != nil {
				if collectErr == nil {
					collectErr = res.err
				}
				continue
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
		}
	}()

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
		return collectErr
	}
	return readErr
}

// processRecord is a pure function — parses one SQLite record through the schema
// and returns all nodes to create, without touching the store.
func processRecord(schema *api.Topology, walker Walker, dbPath string, job recordJob) recordResult {
	var parsed any
	if err := json.Unmarshal([]byte(job.raw), &parsed); err != nil {
		return recordResult{err: fmt.Errorf("parse record %s: %w", job.recordID, err)}
	}

	wrapper := []any{parsed}
	var result recordResult

	for _, nodeSchema := range schema.Nodes {
		for _, childSchema := range nodeSchema.Children {
			collectNodes(&result, childSchema, walker, wrapper, nodeSchema.Name, dbPath, job.recordID)
			if result.err != nil {
				return result
			}
		}
	}

	return result
}

// collectNodes is the pure equivalent of processNode — builds node lists
// without any store access. Safe to call from multiple goroutines.
func collectNodes(result *recordResult, schema api.Node, walker Walker, ctx any, parentPath, dbPath, recordID string) {
	matches, err := walker.Query(ctx, schema.Selector)
	if err != nil {
		result.err = fmt.Errorf("query failed for %s: %w", schema.Name, err)
		return
	}

	for _, match := range matches {
		name, err := renderTemplate(schema.Name, match.Values())
		if err != nil {
			result.err = fmt.Errorf("failed to render name %s: %w", schema.Name, err)
			return
		}

		currentPath := filepath.Join(parentPath, name)
		id := strings.TrimPrefix(filepath.ToSlash(currentPath), "/")

		node := &graph.Node{
			ID:   id,
			Mode: os.ModeDir | 0o555,
		}

		// Recurse children
		nextCtx := match.Context()
		if nextCtx != nil {
			for _, childSchema := range schema.Children {
				collectNodes(result, childSchema, walker, nextCtx, currentPath, dbPath, recordID)
				if result.err != nil {
					return
				}
			}
		}

		// Process files
		for _, fileSchema := range schema.Files {
			fileName, err := renderTemplate(fileSchema.Name, match.Values())
			if err != nil {
				continue
			}
			filePath := filepath.Join(currentPath, fileName)
			fileId := strings.TrimPrefix(filepath.ToSlash(filePath), "/")

			content, err := renderTemplate(fileSchema.ContentTemplate, match.Values())
			if err != nil {
				continue
			}

			fileNode := &graph.Node{
				ID:   fileId,
				Mode: 0o444,
			}

			// Inline small content, lazy-resolve large content from SQLite
			if len(content) > inlineThreshold {
				fileNode.Ref = &graph.ContentRef{
					DBPath:     dbPath,
					RecordID:   recordID,
					Template:   fileSchema.ContentTemplate,
					ContentLen: int64(len(content)),
				}
			} else {
				fileNode.Data = []byte(content)
			}

			result.nodes = append(result.nodes, fileNode)
			node.Children = append(node.Children, fileId)
		}

		result.nodes = append(result.nodes, node)

		// Link to parent (collector will apply this)
		parentID := strings.TrimPrefix(filepath.ToSlash(parentPath), "/")
		result.parentLinks = append(result.parentLinks, parentLink{childID: id, parentID: parentID})
	}
}

func (e *Engine) processNode(schema api.Node, walker Walker, ctx any, parentPath string) error {
	matches, err := walker.Query(ctx, schema.Selector)
	if err != nil {
		return fmt.Errorf("query failed for %s: %w", schema.Name, err)
	}

	for _, match := range matches {
		name, err := renderTemplate(schema.Name, match.Values())
		if err != nil {
			return fmt.Errorf("failed to render name %s: %w", schema.Name, err)
		}

		// Normalize path
		currentPath := filepath.Join(parentPath, name)
		id := strings.TrimPrefix(filepath.ToSlash(currentPath), "/")

		// Create/Update Node — preserve existing children when merging
		// multiple files into the same node (e.g. multiple .go files in one package).
		var existingChildren []string
		if existing, err := e.Store.GetNode(id); err == nil {
			existingChildren = existing.Children
		}

		node := &graph.Node{
			ID:       id,
			Mode:     os.ModeDir | 0o555, // Read-only dir
			Children: existingChildren,
		}
		e.Store.AddNode(node)

		// Link to parent
		if parentPath == "" {
			e.Store.AddRoot(node)
		} else {
			parentId := strings.TrimPrefix(filepath.ToSlash(parentPath), "/")
			parent, err := e.Store.GetNode(parentId)
			if err == nil {
				exists := false
				for _, c := range parent.Children {
					if c == id {
						exists = true
						break
					}
				}
				if !exists {
					parent.Children = append(parent.Children, id)
					e.Store.AddNode(parent)
				}
			}
		}

		// Recurse children
		nextCtx := match.Context()
		if nextCtx != nil {
			for _, childSchema := range schema.Children {
				if err := e.processNode(childSchema, walker, nextCtx, currentPath); err != nil {
					return err
				}
			}
		}

		// Process files (JSON/tree-sitter paths — always inline content)
		for _, fileSchema := range schema.Files {
			fileName, err := renderTemplate(fileSchema.Name, match.Values())
			if err != nil {
				continue
			}
			filePath := filepath.Join(currentPath, fileName)
			fileId := strings.TrimPrefix(filepath.ToSlash(filePath), "/")

			content, err := renderTemplate(fileSchema.ContentTemplate, match.Values())
			if err != nil {
				continue
			}

			fileNode := &graph.Node{
				ID:   fileId,
				Mode: 0o444,
				Data: []byte(content),
			}

			e.Store.AddNode(fileNode)
			node.Children = append(node.Children, fileId)
			e.Store.AddNode(node)
		}
	}
	return nil
}

var tmplFuncs = template.FuncMap{
	"json": func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("<json error: %v>", err)
		}
		return string(b)
	},
	"first": func(v any) any {
		switch s := v.(type) {
		case []any:
			if len(s) > 0 {
				return s[0]
			}
		}
		return nil
	},
	// slice extracts a substring: {{slice .someField 4 8}} → characters [4:8].
	// Used for temporal sharding: {{slice .item.cve.id 4 8}} → "2024" from "CVE-2024-0001".
	"slice": func(s string, start, end int) string {
		if start < 0 {
			start = 0
		}
		if end > len(s) {
			end = len(s)
		}
		if start >= end {
			return ""
		}
		return s[start:end]
	},
}

func renderTemplate(tmpl string, values map[string]any) (string, error) {
	t, err := template.New("").Funcs(tmplFuncs).Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, values); err != nil {
		return "", err
	}
	return buf.String(), nil
}
