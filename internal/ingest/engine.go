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

	var walker Walker
	var root any

	switch ext {
	case ".json":
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var data any
		if err := json.Unmarshal(content, &data); err != nil {
			return fmt.Errorf("failed to parse json %s: %w", path, err)
		}
		walker = NewJsonWalker()
		root = data
	case ".db":
		records, err := LoadSQLite(path)
		if err != nil {
			return fmt.Errorf("failed to load sqlite %s: %w", path, err)
		}
		walker = NewJsonWalker()
		root = records
	case ".py":
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lang := python.GetLanguage()
		parser := sitter.NewParser()
		parser.SetLanguage(lang)
		tree, err := parser.ParseCtx(context.Background(), nil, content)
		if err != nil {
			return fmt.Errorf("failed to parse python %s: %w", path, err)
		}
		walker = NewSitterWalker()
		root = SitterRoot{Node: tree.RootNode(), Source: content, Lang: lang}
	case ".go":
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lang := golang.GetLanguage()
		parser := sitter.NewParser()
		parser.SetLanguage(lang)
		tree, err := parser.ParseCtx(context.Background(), nil, content)
		if err != nil {
			return fmt.Errorf("failed to parse go %s: %w", path, err)
		}
		walker = NewSitterWalker()
		root = SitterRoot{Node: tree.RootNode(), Source: content, Lang: lang}
	default:
		return nil // Skip unsupported files
	}

	// Process schema roots
	for _, nodeSchema := range e.Schema.Nodes {
		if err := e.processNode(nodeSchema, walker, root, ""); err != nil {
			return fmt.Errorf("failed to process schema node %s: %w", nodeSchema.Name, err)
		}
	}
	return nil
}

func (e *Engine) processNode(schema api.Node, walker Walker, ctx any, parentPath string) error {
	matches, err := walker.Query(ctx, schema.Selector)
	if err != nil {
		// If query fails, maybe just log and continue? Or fail?
		// For now fail.
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
				// Check if already child
				exists := false
				for _, c := range parent.Children {
					if c == id {
						exists = true
						break
					}
				}
				if !exists {
					parent.Children = append(parent.Children, id)
					e.Store.AddNode(parent) // Save parent
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
			// Render filename
			fileName, err := renderTemplate(fileSchema.Name, match.Values())
			if err != nil {
				continue
			}
			filePath := filepath.Join(currentPath, fileName)
			fileId := strings.TrimPrefix(filepath.ToSlash(filePath), "/")

			// Render content
			content, err := renderTemplate(fileSchema.ContentTemplate, match.Values())
			if err != nil {
				continue
			}

			fileNode := &graph.Node{
				ID:   fileId,
				Mode: 0o444, // Read-only file
				Data: []byte(content),
			}
			e.Store.AddNode(fileNode)

			// Add to parent children
			node.Children = append(node.Children, fileId)
			e.Store.AddNode(node) // Update current node
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
