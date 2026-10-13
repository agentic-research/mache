package mcpserve

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/vfs"
)

// The virtual paths a mount shows (callers/, callees/, _schema.json) used to
// reach MCP only as rows mache wrote into the served .db before opening it
// (leylinegraph.MaterializeVirtuals). mache no longer writes into a
// ley-line-open projection-v6 arena, so on one of those the graph holds no such
// rows, and list_directory offered a callers/ entry that then listed as empty
// (mache-abaf17). Paths the graph does not hold are answered here instead, by
// the same resolver the NFS mount uses, so the two surfaces cannot drift.

// virtualResolver answers g's virtual paths. Paths are the mount's: rooted at
// "/".
func virtualResolver(g graph.Graph) *vfs.Resolver {
	handlers := []vfs.VHandler{&vfs.CallersHandler{Graph: g}, &vfs.CalleesHandler{Graph: g}}
	if sp, ok := g.(schemaProvider); ok && sp.Schema() != nil {
		if sj, err := json.MarshalIndent(sp.Schema(), "", "  "); err == nil {
			handlers = append(handlers, &vfs.SchemaHandler{Content: sj})
		}
	}
	return vfs.NewResolver(handlers...)
}

// listVirtualDir lists a virtual directory the graph does not hold; ok is
// false when path is not one. A callers/ or callees/ entry stands for the node
// it names, so it lists as a file of that node's size, which read_file reads.
func listVirtualDir(g graph.Graph, path string) ([]nodeEntry, bool) {
	r := virtualResolver(g)
	extras, ok := r.ListDir("/" + path)
	if !ok {
		return nil, false
	}
	entries := make([]nodeEntry, 0, len(extras))
	for _, x := range extras {
		e := nodeEntry{Name: x.Name, Path: path + "/" + x.Name, Type: "file", Size: x.Size}
		switch x.Kind {
		case vfs.KindDir:
			e.Type = "dir"
		case vfs.KindSymlink:
			v := r.Resolve("/" + e.Path)
			if v == nil {
				continue
			}
			if n, err := g.GetNode(v.NodeID); err == nil {
				e.Size = n.ContentSize()
			}
		}
		entries = append(entries, e)
	}
	return entries, true
}

// withVirtualRoot is a root listing with the virtual files the graph did not
// list (_schema.json) placed where the listing's byte order puts them, as they
// sorted when they were rows.
func withVirtualRoot(g graph.Graph, listed []nodeEntry) []nodeEntry {
	for _, e := range listed {
		if e.Name == graph.SchemaDotJSON {
			return listed
		}
	}
	v := virtualResolver(g).Resolve("/" + graph.SchemaDotJSON)
	if v == nil {
		return listed
	}
	at, _ := slices.BinarySearchFunc(listed, graph.SchemaDotJSON, func(e nodeEntry, name string) int {
		return strings.Compare(e.Name, name)
	})
	return slices.Insert(listed, at, nodeEntry{Name: graph.SchemaDotJSON, Path: graph.SchemaDotJSON, Type: "file", Size: v.Size})
}

// readVirtual reads a virtual path the graph does not hold: a virtual file's
// content, or, for a callers/ or callees/ entry, the content of the node it
// names.
func readVirtual(g graph.Graph, path string) (*fileReadResult, error) {
	v := virtualResolver(g).Resolve("/" + path)
	switch {
	case v == nil:
		return nil, fmt.Errorf("not found: %s", path)
	case v.Kind == vfs.KindDir:
		return nil, fmt.Errorf("%s is a directory — use list_directory", path)
	case v.Kind == vfs.KindSymlink:
		// The node an entry names may be a construct directory: read its
		// content, as the mount does when the entry is opened.
		n, err := g.GetNode(v.NodeID)
		if err != nil {
			return nil, fmt.Errorf("not found: %s", path)
		}
		content, err := readNodeContent(g, v.NodeID, n)
		if err != nil {
			return nil, err
		}
		return &fileReadResult{Content: content, Origin: n.Origin}, nil
	default:
		return &fileReadResult{Content: string(v.Content)}, nil
	}
}

// The resolver reads the schema and each construct's token off the graph a
// handler is given, which in a server is the registry's lazyGraph. A lazyGraph
// that stopped forwarding either would leave _schema.json missing and callers/
// empty, with nothing failing.
var (
	_ schemaProvider   = (*lazyGraph)(nil)
	_ graph.DefTokener = (*lazyGraph)(nil)
)
