package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	// Phase 1: lazy loading context (set during SQLite streaming)
	currentDBPath   string
	currentRecordID string
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

// ingestSQLiteStreaming processes a SQLite database one record at a time.
// This keeps memory usage constant regardless of database size.
func (e *Engine) ingestSQLiteStreaming(dbPath string) error {
	walker := NewJsonWalker()

	// Pre-create root directory nodes from schema
	for _, nodeSchema := range e.Schema.Nodes {
		rootNode := &graph.Node{
			ID:   nodeSchema.Name,
			Mode: os.ModeDir | 0o555,
		}
		e.Store.AddNode(rootNode)
		e.Store.AddRoot(rootNode)
	}

	e.currentDBPath = dbPath
	defer func() {
		e.currentDBPath = ""
		e.currentRecordID = ""
	}()

	return StreamSQLite(dbPath, func(recordID string, record any) error {
		e.currentRecordID = recordID

		// Wrap single record so JSONPath $[*] extracts it
		wrapper := []any{record}
		for _, nodeSchema := range e.Schema.Nodes {
			for _, childSchema := range nodeSchema.Children {
				if err := e.processNode(childSchema, walker, wrapper, nodeSchema.Name); err != nil {
					return err
				}
			}
		}
		return nil
	})
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

		// Process files
		for _, fileSchema := range schema.Files {
			fileName, err := renderTemplate(fileSchema.Name, match.Values())
			if err != nil {
				continue
			}
			filePath := filepath.Join(currentPath, fileName)
			fileId := strings.TrimPrefix(filepath.ToSlash(filePath), "/")

			// Render content to determine size
			content, err := renderTemplate(fileSchema.ContentTemplate, match.Values())
			if err != nil {
				continue
			}

			fileNode := &graph.Node{
				ID:   fileId,
				Mode: 0o444, // Read-only file
			}

			// Phase 1: classify content — inline small, lazy large
			if e.currentDBPath != "" && len(content) > inlineThreshold {
				fileNode.Ref = &graph.ContentRef{
					DBPath:     e.currentDBPath,
					RecordID:   e.currentRecordID,
					Template:   fileSchema.ContentTemplate,
					ContentLen: int64(len(content)),
				}
			} else {
				fileNode.Data = []byte(content)
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
