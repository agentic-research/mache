package fs

import (
	"path/filepath"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/winfsp/cgofuse/fuse"
)

// MacheFS implements the FUSE interface from cgofuse
type MacheFS struct {
	fuse.FileSystemBase
	Schema    *api.Topology
	Graph     graph.Graph
	mountTime fuse.Timespec
}

func NewMacheFS(schema *api.Topology, g graph.Graph) *MacheFS {
	return &MacheFS{
		Schema:    schema,
		Graph:     g,
		mountTime: fuse.NewTimespec(time.Now()),
	}
}

// Open checks if the path exists as a property (file).
func (fs *MacheFS) Open(path string, flags int) (int, uint64) {
	dir := filepath.Dir(path)
	base := filepath.Base(path)

	node, err := fs.Graph.GetNode(dir)
	if err == nil {
		if _, ok := node.Properties[base]; ok {
			return 0, 0
		}
	}
	return fuse.ENOENT, 0
}

// Getattr (Stat)
func (fs *MacheFS) Getattr(path string, stat *fuse.Stat_t, fh uint64) int {
	stat.Atim = fs.mountTime
	stat.Mtim = fs.mountTime
	stat.Ctim = fs.mountTime
	stat.Birthtim = fs.mountTime

	// Root is always there
	if path == "/" {
		stat.Mode = fuse.S_IFDIR | 0o555
		stat.Nlink = 2
		return 0
	}

	// 1. Is this a Node in the graph? (Directory)
	node, err := fs.Graph.GetNode(path)
	if err == nil {
		stat.Mode = fuse.S_IFDIR | 0o555
		stat.Nlink = 2
		_ = node
		return 0
	}

	// 2. Is it a Property of a parent node? (File)
	dir := filepath.Dir(path)
	base := filepath.Base(path)

	parentNode, err := fs.Graph.GetNode(dir)
	if err == nil {
		if content, ok := parentNode.Properties[base]; ok {
			stat.Mode = fuse.S_IFREG | 0o444
			stat.Nlink = 1
			stat.Size = int64(len(content))
			return 0
		}
	}

	return fuse.ENOENT
}

// Readdir (List directory)
func (fs *MacheFS) Readdir(path string, fill func(name string, stat *fuse.Stat_t, ofst int64) bool, ofst int64, fh uint64) int {
	fill(".", nil, 0)
	fill("..", nil, 0)

	// 1. List Children Nodes (subdirectories)
	children, err := fs.Graph.ListChildren(path)
	if err != nil && path != "/" {
		return fuse.ENOENT
	}
	if err == nil {
		for _, childID := range children {
			name := filepath.Base(childID)
			fill(name, nil, 0)
		}
	}

	// 2. List Properties as virtual files
	node, err := fs.Graph.GetNode(path)
	if err == nil {
		for propName := range node.Properties {
			fill(propName, nil, 0)
		}
	}

	return 0
}

// Read (Cat file)
func (fs *MacheFS) Read(path string, buff []byte, ofst int64, fh uint64) int {
	dir := filepath.Dir(path)
	base := filepath.Base(path)

	node, err := fs.Graph.GetNode(dir)
	if err != nil {
		return fuse.ENOENT
	}

	content, ok := node.Properties[base]
	if !ok {
		return fuse.ENOENT
	}

	if ofst >= int64(len(content)) {
		return 0
	}
	end := ofst + int64(len(buff))
	if end > int64(len(content)) {
		end = int64(len(content))
	}
	n := copy(buff, content[ofst:end])
	return n
}
