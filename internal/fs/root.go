package fs

import (
	"hash/fnv"
	"path/filepath"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/winfsp/cgofuse/fuse"
)

// pathIno returns a stable inode number for a given path.
// Root gets inode 1 (FUSE convention). All others use FNV-1a hash.
func pathIno(path string) uint64 {
	if path == "/" {
		return 1
	}
	h := fnv.New64a()
	h.Write([]byte(path))
	ino := h.Sum64()
	if ino <= 1 {
		ino = 2 // 0 = unknown, 1 = root — both reserved
	}
	return ino
}

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
		return -fuse.ENOENT, 0
	}
	if node.Mode.IsDir() {
		return -fuse.EISDIR, 0
	}
	return 0, 0
}

// Getattr trusts the node's declared Mode.
func (fs *MacheFS) Getattr(path string, stat *fuse.Stat_t, fh uint64) int {
	stat.Atim = fs.mountTime
	stat.Mtim = fs.mountTime
	stat.Ctim = fs.mountTime
	stat.Birthtim = fs.mountTime

	stat.Ino = pathIno(path)

	if path == "/" {
		stat.Mode = fuse.S_IFDIR | 0o555
		stat.Nlink = 2
		return 0
	}

	node, err := fs.Graph.GetNode(path)
	if err != nil {
		return -fuse.ENOENT
	}

	if node.Mode.IsDir() {
		stat.Mode = fuse.S_IFDIR | 0o555
		stat.Nlink = 2
	} else {
		stat.Mode = fuse.S_IFREG | 0o444
		stat.Nlink = 1
		stat.Size = node.ContentSize()
	}
	return 0
}

// Readdir lists children of a directory node.
// Uses explicit offsets for pagination to handle large directories (e.g. 323K NVD entries).
// Offset layout: 0=start, 1=".", 2="..", 3+i=children[i].
func (fs *MacheFS) Readdir(path string, fill func(name string, stat *fuse.Stat_t, ofst int64) bool, ofst int64, fh uint64) int {
	// For non-root paths, verify this is actually a directory
	if path != "/" {
		node, err := fs.Graph.GetNode(path)
		if err != nil {
			return -fuse.ENOENT
		}
		if !node.Mode.IsDir() {
			return -fuse.ENOTDIR
		}
	}

	children, err := fs.Graph.ListChildren(path)
	if err != nil {
		return -fuse.ENOENT
	}

	// Use offset=0 (auto mode): FUSE manages buffering and pagination internally.
	// In auto mode, fill() should never return true (per FUSE spec), so we don't
	// check the return value. This is required for fuse-t compatibility.
	fill(".", nil, 0)
	fill("..", nil, 0)

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
		return -fuse.ENOENT
	}
	if node.Mode.IsDir() {
		return -fuse.EISDIR
	}

	n, err := fs.Graph.ReadContent(path, buff, ofst)
	if err != nil {
		return -fuse.EIO
	}
	return n
}
