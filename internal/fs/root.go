package fs

import (
	"hash/fnv"
	"path/filepath"
	"sync"
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

	// Directory handle cache: Opendir builds the entry list once,
	// Readdir slices from it, Releasedir frees it.
	handleMu   sync.Mutex
	handles    map[uint64][]string // fh → [".", "..", "child1", ...]
	nextHandle uint64
}

func NewMacheFS(schema *api.Topology, g graph.Graph) *MacheFS {
	return &MacheFS{
		Schema:    schema,
		Graph:     g,
		mountTime: fuse.NewTimespec(time.Now()),
		handles:   make(map[uint64][]string),
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

// Opendir fetches the directory listing once and caches it by handle.
func (fs *MacheFS) Opendir(path string) (int, uint64) {
	if path != "/" {
		node, err := fs.Graph.GetNode(path)
		if err != nil {
			return -fuse.ENOENT, 0
		}
		if !node.Mode.IsDir() {
			return -fuse.ENOTDIR, 0
		}
	}

	children, err := fs.Graph.ListChildren(path)
	if err != nil {
		return -fuse.ENOENT, 0
	}

	entries := make([]string, 0, len(children)+2)
	entries = append(entries, ".", "..")
	for _, c := range children {
		entries = append(entries, filepath.Base(c))
	}

	fs.handleMu.Lock()
	fh := fs.nextHandle
	fs.nextHandle++
	fs.handles[fh] = entries
	fs.handleMu.Unlock()

	return 0, fh
}

// Releasedir frees the cached directory listing.
func (fs *MacheFS) Releasedir(path string, fh uint64) int {
	fs.handleMu.Lock()
	delete(fs.handles, fh)
	fs.handleMu.Unlock()
	return 0
}

// Readdir serves entries from the cached handle.
// Auto-mode (offset=0 to fill): fuse-t requires all results in the first pass.
// The NFS translation layer handles pagination to the macOS NFS client.
// cgofuse fill() convention: true = accepted, false = buffer full.
func (fs *MacheFS) Readdir(path string, fill func(name string, stat *fuse.Stat_t, ofst int64) bool, ofst int64, fh uint64) int {
	fs.handleMu.Lock()
	entries, ok := fs.handles[fh]
	fs.handleMu.Unlock()

	if !ok {
		// Fallback: no handle (shouldn't happen, but be safe)
		children, err := fs.Graph.ListChildren(path)
		if err != nil {
			return -fuse.ENOENT
		}
		entries = make([]string, 0, len(children)+2)
		entries = append(entries, ".", "..")
		for _, c := range children {
			entries = append(entries, filepath.Base(c))
		}
	}

	// Auto-mode: pass offset=0 to fill(). FUSE handles pagination internally.
	// fuse-t translates to NFS READDIR and manages cookie-based continuation.
	for _, name := range entries {
		if !fill(name, nil, 0) {
			break // buffer full
		}
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
