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
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var walker Walker
	var root any

	switch ext {
	case ".json":
		var data any
		if err := json.Unmarshal(content, &data); err != nil {
			return fmt.Errorf("failed to parse json %s: %w", path, err)
		}
		walker = NewJsonWalker()
		root = data
	case ".py":
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
		id := filepath.ToSlash(currentPath)
		if strings.HasPrefix(id, "/") {
		    id = id[1:] // Remove leading slash for consistency with MemoryStore
		}

		// Create/Update Node
		// Check if it already exists (could be merged from multiple files?)
		// For now, assume fresh or overwrite.
		node := &graph.Node{
			ID:       id,
			Mode:     os.ModeDir | 0555, // Read-only dir
			Children: []string{},
		}
		e.Store.AddNode(node)

		// Link to parent
		if parentPath == "" {
			e.Store.AddRoot(node)
		} else {
			parentId := filepath.ToSlash(parentPath)
            if strings.HasPrefix(parentId, "/") {
                parentId = parentId[1:]
            }
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
            fileId := filepath.ToSlash(filePath)
            if strings.HasPrefix(fileId, "/") {
                fileId = fileId[1:]
            }

			// Render content
			content, err := renderTemplate(fileSchema.ContentTemplate, match.Values())
			if err != nil {
				continue
			}

			fileNode := &graph.Node{
				ID:   fileId,
				Mode: 0444, // Read-only file
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

func renderTemplate(tmpl string, values map[string]string) (string, error) {
	t, err := template.New("").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, values); err != nil {
		return "", err
	}
	return buf.String(), nil
}
