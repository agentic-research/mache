package ingest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentic-research/mache/internal/graph"
)

// ingestFile dispatches to the appropriate ingestor based on the file's
// extension and schema type. Single-file entry point used by both the
// initial walk (when the schema is non-tree-sitter) and ReIngestFile.
func (e *Engine) ingestFile(path string, modTime time.Time) error {
	ext := filepath.Ext(path)

	switch ext {
	case ".db":
		return e.ingestSQLiteStreaming(path)
	case ".json":
		return e.ingestJSON(path, modTime)
	default:
		if langName, ok := langForExt(ext); ok {
			return e.ingestSourceFile(path, langName, modTime)
		}
		if isBinaryFile(path) { // coverage:ignore
			return nil // coverage:ignore
		} // coverage:ignore
		return e.ingestRawFile(path, modTime) // coverage:ignore
	}
}

func (e *Engine) ingestJSON(path string, modTime time.Time) error {
	if _, err := ensureFile(path, "a JSON file"); err != nil {
		return err // coverage:ignore
	} // coverage:ignore

	content, err := os.ReadFile(path)
	if err != nil {
		return err // coverage:ignore
	} // coverage:ignore

	var data any
	if err := json.Unmarshal(content, &data); err != nil {
		return fmt.Errorf("failed to parse json %s: %w", path, err) // coverage:ignore
	} // coverage:ignore

	// Clear old nodes from this file (if any)
	absPath, _ := filepath.Abs(path)
	realPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		realPath = absPath // coverage:ignore
	} // coverage:ignore
	e.Store.DeleteFileNodes(realPath)

	walker := NewJsonWalker()
	for _, nodeSchema := range e.Schema.Nodes {
		if err := e.processNode(nodeSchema, walker, data, "", "", "", modTime, e.Store, nil, nil, nil, nil); err != nil {
			return fmt.Errorf("failed to process schema node %s: %w", nodeSchema.Name, err) // coverage:ignore
		} // coverage:ignore
	}
	return nil
}

func (e *Engine) ingestRawFile(path string, modTime time.Time) error { // coverage:ignore
	return e.ingestRawFileUnder(path, "", modTime) // coverage:ignore
} // coverage:ignore

// ingestRawFileUnder copies a raw (non-parsed) file into the graph,
// creating intermediate directory nodes as needed. When prefix is set
// the file is mounted under that subtree (e.g. "_project_files").
func (e *Engine) ingestRawFileUnder(path, prefix string, modTime time.Time) error {
	rel, err := filepath.Rel(e.RootPath, path)
	if err != nil { // coverage:ignore
		return err // coverage:ignore
	} // coverage:ignore
	rel = filepath.ToSlash(rel)
	parts := strings.Split(rel, "/")

	// When a prefix is set, lazily create the prefix root node on first use.
	parentID := prefix
	if prefix != "" {
		if _, err := e.Store.GetNode(prefix); err != nil {
			pfNode := &graph.Node{ID: prefix, Mode: os.ModeDir | 0o555}
			e.Store.AddNode(pfNode)
			e.Store.AddRoot(pfNode)
		}
	}

	// 1. Create/Ensure intermediate directories
	for i := 0; i < len(parts)-1; i++ {
		part := parts[i]
		var currentID string
		if parentID != "" {
			currentID = parentID + "/" + part
		} else { // coverage:ignore
			currentID = part // coverage:ignore
		} // coverage:ignore

		if _, err := e.Store.GetNode(currentID); err != nil {
			// Create directory node
			node := &graph.Node{
				ID:   currentID,
				Mode: os.ModeDir | 0o555,
			}
			e.Store.AddNode(node)

			// Link to parent
			if parentID == "" { // coverage:ignore
				e.Store.AddRoot(node) // coverage:ignore
			} else {
				parent, err := e.Store.GetNode(parentID)
				if err == nil {
					if e.childSeen[parentID] == nil {
						e.childSeen[parentID] = make(map[string]bool, len(parent.Children))
						for _, c := range parent.Children {
							e.childSeen[parentID][c] = true
						}
					}
					if !e.childSeen[parentID][currentID] {
						e.childSeen[parentID][currentID] = true
						parent.Children = append(parent.Children, currentID)
						e.Store.AddNode(parent)
					}
				}
			}
		}
		parentID = currentID
	}

	// 2. Create file node
	var fileID string
	if prefix != "" {
		fileID = prefix + "/" + rel
	} else { // coverage:ignore
		fileID = rel // coverage:ignore
	} // coverage:ignore

	info, err := ensureFile(path, "a raw file")
	if err != nil {
		return err // coverage:ignore
	} // coverage:ignore
	if ShouldSkipFile(path, info.Size()) {
		return nil // coverage:ignore
	} // coverage:ignore

	content, err := os.ReadFile(path)
	if err != nil {
		return err // coverage:ignore
	} // coverage:ignore

	absPath, _ := filepath.Abs(path)
	e.Store.DeleteFileNodes(absPath)

	fileNode := &graph.Node{
		ID:      fileID,
		Mode:    0o444,
		ModTime: modTime,
		Data:    content,
		Origin: &graph.SourceOrigin{
			FilePath:  absPath,
			StartByte: 0,
			EndByte:   uint32(len(content)),
		},
	}
	e.Store.AddNode(fileNode)

	// Link to parent
	if parentID == "" {
		e.Store.AddRoot(fileNode) // coverage:ignore
	} else {
		parent, err := e.Store.GetNode(parentID)
		if err == nil {
			parent.Children = append(parent.Children, fileID)
			e.Store.AddNode(parent)
		}
	}

	return nil
}
