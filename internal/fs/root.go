package fs

import (
	"hash/fnv"
	"log"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/writeback"
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

// writeHandle tracks an in-progress write to a file node.
type writeHandle struct {
	path   string // FUSE path (used to resolve node ID)
	nodeID string // graph node ID
	buf    []byte
	dirty  bool
}

// MacheFS implements the FUSE interface from cgofuse.
// It delegates all file/directory decisions to the Graph — no heuristics.
type MacheFS struct {
	fuse.FileSystemBase
	Schema    *api.Topology
	Graph     graph.Graph
	mountTime fuse.Timespec

	// Write-back support (nil Engine = read-only)
	Writable bool
	Engine   *ingest.Engine

	// Directory handle cache: Opendir builds the entry list once,
	// Readdir slices from it, Releasedir frees it.
	handleMu     sync.Mutex
	handles      map[uint64][]string     // fh → [".", "..", "child1", ...]
	writeHandles map[uint64]*writeHandle // fh → write buffer
	nextHandle   uint64
}

func NewMacheFS(schema *api.Topology, g graph.Graph) *MacheFS {
	return &MacheFS{
		Schema:       schema,
		Graph:        g,
		mountTime:    fuse.NewTimespec(time.Now()),
		handles:      make(map[uint64][]string),
		writeHandles: make(map[uint64]*writeHandle),
	}
}

// Open validates that the path is a file node. For writable mounts,
// write flags allocate a writeHandle backed by the node's current content.
func (fs *MacheFS) Open(path string, flags int) (int, uint64) {
	node, err := fs.Graph.GetNode(path)
	if err != nil {
		return -fuse.ENOENT, 0
	}
	if node.Mode.IsDir() {
		return -fuse.EISDIR, 0
	}

	writing := flags&(syscall.O_WRONLY|syscall.O_RDWR) != 0
	if writing {
		if !fs.Writable || node.Origin == nil {
			return -fuse.EACCES, 0
		}

		// Pre-fill buffer with existing content (for O_RDWR / partial writes)
		var buf []byte
		if flags&syscall.O_TRUNC == 0 {
			size := node.ContentSize()
			if size > 0 {
				buf = make([]byte, size)
				n, _ := fs.Graph.ReadContent(path, buf, 0)
				buf = buf[:n]
			}
		}

		fs.handleMu.Lock()
		fh := fs.nextHandle
		fs.nextHandle++
		fs.writeHandles[fh] = &writeHandle{
			path:   path,
			nodeID: node.ID,
			buf:    buf,
			dirty:  false,
		}
		fs.handleMu.Unlock()
		return 0, fh
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
		perm := uint32(0o444)
		if fs.Writable && node.Origin != nil {
			perm = 0o644
		}
		stat.Mode = fuse.S_IFREG | perm
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
// If a writeHandle exists for this fh, reads from the in-progress buffer instead.
func (fs *MacheFS) Read(path string, buff []byte, ofst int64, fh uint64) int {
	fs.handleMu.Lock()
	wh, isWrite := fs.writeHandles[fh]
	fs.handleMu.Unlock()

	if isWrite && wh.buf != nil {
		if ofst >= int64(len(wh.buf)) {
			return 0
		}
		n := copy(buff, wh.buf[ofst:])
		return n
	}

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

// Write appends/overwrites data in the writeHandle buffer.
func (fs *MacheFS) Write(path string, buff []byte, ofst int64, fh uint64) int {
	fs.handleMu.Lock()
	wh, ok := fs.writeHandles[fh]
	fs.handleMu.Unlock()

	if !ok {
		return -fuse.EBADF
	}

	end := ofst + int64(len(buff))
	if end > int64(len(wh.buf)) {
		grown := make([]byte, end)
		copy(grown, wh.buf)
		wh.buf = grown
	}
	copy(wh.buf[ofst:], buff)
	wh.dirty = true
	return len(buff)
}

// Truncate resizes the writeHandle buffer (called via ftruncate).
func (fs *MacheFS) Truncate(path string, size int64, fh uint64) int {
	fs.handleMu.Lock()
	wh, ok := fs.writeHandles[fh]
	fs.handleMu.Unlock()

	if !ok {
		// No write handle — check if this is a writable node
		if !fs.Writable {
			return -fuse.EACCES
		}
		return 0
	}

	if size < int64(len(wh.buf)) {
		wh.buf = wh.buf[:size]
	} else if size > int64(len(wh.buf)) {
		grown := make([]byte, size)
		copy(grown, wh.buf)
		wh.buf = grown
	}
	wh.dirty = true
	return 0
}

// Flush is a no-op — we commit on Release.
func (fs *MacheFS) Flush(path string, fh uint64) int {
	return 0
}

// Release is THE COMMIT POINT for write-back.
// On close: splice new content into source → goimports → re-ingest → graph updated.
func (fs *MacheFS) Release(path string, fh uint64) int {
	fs.handleMu.Lock()
	wh, ok := fs.writeHandles[fh]
	if ok {
		delete(fs.writeHandles, fh)
	}
	fs.handleMu.Unlock()

	if !ok || !wh.dirty {
		return 0
	}

	node, err := fs.Graph.GetNode(wh.path)
	if err != nil {
		log.Printf("writeback: node %s not found: %v", wh.nodeID, err)
		return -fuse.EIO
	}
	if node.Origin == nil {
		return -fuse.EACCES
	}

	// 1. Splice new content into source file
	if err := writeback.Splice(*node.Origin, wh.buf); err != nil {
		log.Printf("writeback: splice failed for %s: %v", node.Origin.FilePath, err)
		return -fuse.EIO
	}

	// 2. Run goimports (failure-tolerant — if agent wrote broken code,
	//    we still want it in the FS so the agent can see and fix the error)
	if err := exec.Command("goimports", "-w", node.Origin.FilePath).Run(); err != nil {
		log.Printf("writeback: goimports failed for %s (continuing): %v", node.Origin.FilePath, err)
	}

	// 3. Re-ingest the source file — updates ALL Origins from this file
	if fs.Engine != nil {
		if err := fs.Engine.Ingest(node.Origin.FilePath); err != nil {
			log.Printf("writeback: re-ingest failed for %s: %v", node.Origin.FilePath, err)
		}
	}

	return 0
}
