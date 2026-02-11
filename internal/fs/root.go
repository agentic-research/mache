package fs

import (
	"path/filepath"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/winfsp/cgofuse/fuse"
)

// MacheFS implements the FUSE interface from cgofuse.
// It delegates all file/directory decisions to the Graph — no heuristics.
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

// Open validates that the path is a file node.
func (fs *MacheFS) Open(path string, flags int) (int, uint64) {
	node, err := fs.Graph.GetNode(path)
	if err != nil {
		return fuse.ENOENT, 0
	}
	if node.Mode.IsDir() {
		return fuse.ENOENT, 0
	}
	return 0, 0
}

// Getattr trusts the node's declared Mode.
func (fs *MacheFS) Getattr(path string, stat *fuse.Stat_t, fh uint64) int {
	stat.Atim = fs.mountTime
	stat.Mtim = fs.mountTime
	stat.Ctim = fs.mountTime
	stat.Birthtim = fs.mountTime

	if path == "/" {
		stat.Mode = fuse.S_IFDIR | 0o555
		stat.Nlink = 2
		return 0
	}

	node, err := fs.Graph.GetNode(path)
	if err != nil {
		return fuse.ENOENT
	}

	if node.Mode.IsDir() {
		stat.Mode = fuse.S_IFDIR | 0o555
		stat.Nlink = 2
	} else {
		stat.Mode = fuse.S_IFREG | 0o444
		stat.Nlink = 1
		stat.Size = int64(len(node.Data))
	}
	return 0
}

// Readdir lists children of a directory node.
func (fs *MacheFS) Readdir(path string, fill func(name string, stat *fuse.Stat_t, ofst int64) bool, ofst int64, fh uint64) int {
	// For non-root paths, verify this is actually a directory
	if path != "/" {
		node, err := fs.Graph.GetNode(path)
		if err != nil {
			return fuse.ENOENT
		}
		if !node.Mode.IsDir() {
			return fuse.ENOENT
		}
	}

	fill(".", nil, 0)
	fill("..", nil, 0)

	children, err := fs.Graph.ListChildren(path)
	if err != nil {
		return 0
	}
	for _, childID := range children {
		name := filepath.Base(childID)
		fill(name, nil, 0)
	}

	return 0
}

// Read returns the Data of a file node.
func (fs *MacheFS) Read(path string, buff []byte, ofst int64, fh uint64) int {
	node, err := fs.Graph.GetNode(path)
	if err != nil {
		return fuse.ENOENT
	}
	if node.Mode.IsDir() {
		return fuse.ENOENT
	}

	if ofst >= int64(len(node.Data)) {
		return 0
	}
	end := ofst + int64(len(buff))
	if end > int64(len(node.Data)) {
		end = int64(len(node.Data))
	}
	n := copy(buff, node.Data[ofst:end])
	return n
}
