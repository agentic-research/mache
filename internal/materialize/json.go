package materialize

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// JSONMaterializer reads the node tree from a mache SQLite DB and writes it
// as a hierarchical JSON file. Directories become nested objects with a
// "children" array; files become leaf objects with "content" and "size".
//
// Uses parent_id + name traversal (same as BoltDB materializer) so node
// names containing slashes or special characters are handled correctly.
//
// Output is written atomically: data goes to a temp file first, then
// os.Rename replaces the target path.
type JSONMaterializer struct{}

type jsonNode struct {
	id       string
	parentID string
	name     string
	kind     int // 1=dir, 0=file
	size     int64
	content  sql.NullString
}

// jsonEntry is the JSON-serializable representation of a node.
type jsonEntry struct {
	Name     string       `json:"name"`
	Type     string       `json:"type"`
	Children []*jsonEntry `json:"children,omitempty"`
	Size     *int64       `json:"size,omitempty"`
	Content  *string      `json:"content,omitempty"`
}

func (m *JSONMaterializer) Materialize(srcDB, outPath string) error {
	db, err := sql.Open("sqlite", srcDB)
	if err != nil {
		return fmt.Errorf("open source db: %w", err)
	}
	defer func() { _ = db.Close() }()

	// Load all nodes into memory and build parent→children index.
	rows, err := db.Query(`SELECT id, COALESCE(parent_id, ''), name, kind, size, record FROM nodes ORDER BY id`)
	if err != nil {
		return fmt.Errorf("query nodes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var nodes []jsonNode
	childrenOf := map[string][]int{} // parent_id → indices into nodes
	for rows.Next() {
		var n jsonNode
		if err := rows.Scan(&n.id, &n.parentID, &n.name, &n.kind, &n.size, &n.content); err != nil {
			return fmt.Errorf("scan node: %w", err)
		}
		idx := len(nodes)
		nodes = append(nodes, n)
		childrenOf[n.parentID] = append(childrenOf[n.parentID], idx)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate nodes: %w", err)
	}

	// Build the JSON tree recursively from root children.
	// Initialize to empty slice (not nil) so JSON encodes as [] not null.
	visited := map[string]bool{}
	root := buildJSONChildren("", nodes, childrenOf, visited)
	if root == nil {
		root = []*jsonEntry{}
	}

	// Write atomically: temp file in same directory, then rename.
	dir := filepath.Dir(outPath)
	tmp, err := os.CreateTemp(dir, ".mache-json-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	bw := bufio.NewWriter(tmp)
	enc := json.NewEncoder(bw)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("encode json: %w", err)
	}
	if err := bw.Flush(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("flush: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp: %w", err)
	}

	if err := os.Rename(tmpPath, outPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename to output: %w", err)
	}
	return nil
}

// buildJSONChildren recursively builds []*jsonEntry for all children of parentID.
// An unnamed root directory node (name="" at root level) is treated as
// transparent — its children are promoted to the current level.
func buildJSONChildren(parentID string, nodes []jsonNode, childrenOf map[string][]int, visited map[string]bool) []*jsonEntry {
	children, ok := childrenOf[parentID]
	if !ok {
		return nil
	}

	var entries []*jsonEntry
	for _, idx := range children {
		n := nodes[idx]

		// Cycle protection.
		if visited[n.id] {
			continue
		}
		visited[n.id] = true

		if n.kind == 1 {
			// Directory node.
			if n.name == "" && parentID == "" {
				// Transparent unnamed root — promote children.
				promoted := buildJSONChildren(n.id, nodes, childrenOf, visited)
				entries = append(entries, promoted...)
				continue
			}
			entry := &jsonEntry{
				Name:     n.name,
				Type:     "directory",
				Children: buildJSONChildren(n.id, nodes, childrenOf, visited),
			}
			entries = append(entries, entry)
		} else if n.kind == 0 {
			// File node.
			if n.name == "" {
				continue
			}
			entry := &jsonEntry{
				Name: n.name,
				Type: "file",
				Size: &n.size,
			}
			if n.content.Valid {
				s := n.content.String
				entry.Content = &s
			}
			entries = append(entries, entry)
		}
	}
	return entries
}
