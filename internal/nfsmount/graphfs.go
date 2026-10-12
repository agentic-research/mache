// Package nfsmount provides an NFS-based mount backend for mache.
// It adapts mache's graph.Graph interface to billy.Filesystem for use
// with willscott/go-nfs, replacing the FUSE mount layer.
package nfsmount

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	billy "github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/helper/chroot"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/vfs"
)

var errReadOnly = fmt.Errorf("read-only filesystem")

// GraphFS adapts mache's graph.Graph to billy.Filesystem.
// This is the bridge between mache's projection logic and go-nfs.
type GraphFS struct {
	graph      graph.Graph
	schema     *api.Topology
	schemaJSON []byte
	mountTime  time.Time
	writable   bool
	writeBack  WriteBackFunc

	// Virtual path resolver — shared with FUSE backend.
	resolver *vfs.Resolver

	// Optional prompt content served as /PROMPT.txt virtual file (agent mode).
	promptContent []byte

	// sessions holds, per writable path, the bytes the NFS client has written
	// so far. go-nfs serves every WRITE request as its own open/write/close,
	// so without this each request after the first started over (mache-bc65f8).
	sessionsMu sync.Mutex
	sessions   map[string]*writeSession
}

// NewGraphFS creates a billy.Filesystem backed by a mache Graph.
func NewGraphFS(g graph.Graph, schema *api.Topology) *GraphFS {
	sj, _ := json.MarshalIndent(schema, "", "  ")
	sj = append(sj, '\n')

	resolver := vfs.NewDefaultResolver(g, sj)

	// Share diagnostics map with MemoryStore if available
	if ms, ok := g.(*graph.MemoryStore); ok {
		resolver.SetWritable(false, &ms.WriteStatus)
	}

	return &GraphFS{
		graph:      g,
		schema:     schema,
		schemaJSON: sj,
		mountTime:  time.Now(),
		resolver:   resolver,
	}
}

// SetPromptContent sets the content for the /PROMPT.txt virtual file.
func (fs *GraphFS) SetPromptContent(content []byte) {
	fs.promptContent = content
	fs.resolver.SetPromptContent(content)
}

// SetWriteBack enables write support. The callback is invoked when a
// written file is closed, triggering the splice pipeline.
func (fs *GraphFS) SetWriteBack(fn WriteBackFunc) {
	fs.writable = true
	fs.writeBack = fn
	fs.resolver.SetWritable(true, nil)
}

// --- billy.Basic ---

// Create signals success for existing writable files (NFS CREATE on existing file).
// go-nfs closes this file immediately — the actual writes come via separate
// OpenFile calls from WRITE RPCs. We return a no-op file to avoid premature splice.
func (fs *GraphFS) Create(filename string) (billy.File, error) {
	if !fs.writable {
		return nil, errReadOnly
	}
	filename = cleanPath(filename)

	// Block AppleDouble / metadata files (silently succeed to avoid log spam)
	if strings.HasPrefix(filepath.Base(filename), "._") {
		return &bytesFile{name: filename, data: nil}, nil
	}

	node, err := fs.graph.GetNode(filename)
	if err != nil {
		return nil, &os.PathError{Op: "create", Path: filename, Err: os.ErrNotExist}
	}
	if node.Origin == nil {
		return nil, &os.PathError{Op: "create", Path: filename, Err: fmt.Errorf("no source origin")}
	}

	// Return a no-op file — go-nfs will close this immediately.
	// The real content writes come through OpenFile via WRITE RPCs.
	return &bytesFile{name: filename, data: nil}, nil
}

func (fs *GraphFS) Open(filename string) (billy.File, error) {
	return fs.OpenFile(filename, os.O_RDONLY, 0)
}

func (fs *GraphFS) OpenFile(filename string, flag int, perm os.FileMode) (billy.File, error) {
	filename = cleanPath(filename)

	writing := flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC) != 0

	if writing {
		if !fs.writable {
			return nil, errReadOnly
		}
		return fs.openWritable(filename, flag)
	}

	// Virtual paths: delegate to resolver
	if entry := fs.resolver.Resolve(filename); entry != nil {
		switch entry.Kind {
		case vfs.KindDir:
			return nil, &os.PathError{Op: "open", Path: filename, Err: fmt.Errorf("is a directory")}
		case vfs.KindSymlink:
			// NFS serves callers/callees entries as graphFiles (not symlinks).
			// Use NodeID to open the referenced node's content.
			nodeID := "/" + entry.NodeID
			refNode, err := fs.graph.GetNode(entry.NodeID)
			if err != nil {
				return nil, &os.PathError{Op: "open", Path: filename, Err: os.ErrNotExist}
			}
			return &graphFile{id: nodeID, size: refNode.ContentSize(), graph: fs.graph}, nil
		default:
			// KindFile: return content as bytesFile
			return &bytesFile{name: filepath.Base(filename), data: entry.Content}, nil
		}
	}

	node, err := fs.graph.GetNode(filename)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: filename, Err: os.ErrNotExist}
	}
	if node.Mode.IsDir() {
		return nil, &os.PathError{Op: "open", Path: filename, Err: fmt.Errorf("is a directory")}
	}

	return &graphFile{
		id:    filename,
		size:  node.ContentSize(),
		graph: fs.graph,
	}, nil
}

// openWritable returns a writeFile for nodes that have a SourceOrigin.
func (fs *GraphFS) openWritable(filename string, flag int) (billy.File, error) {
	if filename == "/"+graph.SchemaDotJSON {
		return nil, &os.PathError{Op: "open", Path: filename, Err: fmt.Errorf("read-only virtual file")}
	}

	node, err := fs.graph.GetNode(filename)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: filename, Err: os.ErrNotExist}
	}
	if node.Mode.IsDir() {
		return nil, &os.PathError{Op: "open", Path: filename, Err: fmt.Errorf("is a directory")}
	}
	if node.Origin == nil {
		return nil, &os.PathError{Op: "open", Path: filename, Err: fmt.Errorf("no source origin for write-back")}
	}

	current := fs.readAll(filename, node)

	// A write continues the client's own bytes for this path when a write is
	// in progress, and the node's content otherwise. O_TRUNC starts empty.
	var buf []byte
	if flag&os.O_TRUNC == 0 {
		if buf = fs.sessionBytes(filename, current); buf == nil {
			buf = bytes.Clone(current)
		}
	}

	return &writeFile{
		id:      filename,
		origin:  *node.Origin,
		buf:     buf,
		onClose: fs.writeBack,
		// Agents and editors mean "replace" when they write a source file from
		// the top, often without sending a truncate. So a write that STARTS at
		// offset 0 replaces the whole buffer. Only then: a write at an offset
		// is the next chunk of one edit, or a patch, and continues the bytes
		// before it rather than zero-filling them (mache-bc65f8).
		replaceAtZero: filepath.Base(filename) == "source" && flag&os.O_APPEND == 0,
		session:       fs,
	}, nil
}

func (fs *GraphFS) Stat(filename string) (os.FileInfo, error) {
	return fs.Lstat(filename)
}

func (fs *GraphFS) Rename(oldpath, newpath string) error {
	return errReadOnly
}

func (fs *GraphFS) Remove(filename string) error {
	if !fs.writable {
		return errReadOnly
	}
	filename = cleanPath(filename)

	node, err := fs.graph.GetNode(filename)
	if err != nil {
		return &os.PathError{Op: "remove", Path: filename, Err: os.ErrNotExist}
	}
	if node.Origin == nil {
		return &os.PathError{Op: "remove", Path: filename, Err: fmt.Errorf("no source origin for delete")}
	}

	// Splice empty content to "delete" the node
	if fs.writeBack != nil {
		return fs.writeBack(filename, *node.Origin, []byte{})
	}
	return nil
}

func (fs *GraphFS) Join(elem ...string) string {
	return filepath.Join(elem...)
}

// --- billy.TempFile ---

func (fs *GraphFS) TempFile(dir, prefix string) (billy.File, error) {
	return nil, billy.ErrNotSupported
}

// --- billy.Dir ---

func (fs *GraphFS) ReadDir(path string) ([]os.FileInfo, error) {
	path = cleanPath(path)

	// Virtual directory listings (diagnostics, callers, callees)
	if dirEntries, ok := fs.resolver.ListDir(path); ok {
		infos := make([]os.FileInfo, 0, len(dirEntries))
		for _, de := range dirEntries {
			var fullPath string
			if path == "/" {
				fullPath = "/" + de.Name
			} else {
				fullPath = path + "/" + de.Name
			}
			// For symlink entries (callers/callees), resolve the full entry
			// to get the referenced node's content size via vEntryToFileInfo.
			if de.Kind == vfs.KindSymlink {
				if entry := fs.resolver.Resolve(fullPath); entry != nil {
					infos = append(infos, fs.vEntryToFileInfo(fullPath, entry, fs.mountTime))
					continue
				}
			}
			mode := os.FileMode(de.Perm)
			if de.Kind == vfs.KindDir {
				mode |= os.ModeDir
			}
			infos = append(infos, newFileInfo(fullPath, de.Size, mode, fs.mountTime))
		}
		return infos, nil
	}

	node, err := fs.graph.GetNode(path)
	if err != nil && path != "/" {
		return nil, &os.PathError{Op: "readdir", Path: path, Err: os.ErrNotExist}
	}
	if node != nil && !node.Mode.IsDir() {
		return nil, &os.PathError{Op: "readdir", Path: path, Err: fmt.Errorf("not a directory")}
	}

	childStats, err := fs.graph.ListChildStats(path)
	if err != nil {
		return nil, &os.PathError{Op: "readdir", Path: path, Err: os.ErrNotExist}
	}

	infos := make([]os.FileInfo, 0, len(childStats)+3)

	// Inject virtual entries from all handlers
	for _, extra := range fs.resolver.DirExtras(path, node) {
		var fullPath string
		if path == "/" {
			fullPath = "/" + extra.Name
		} else {
			fullPath = path + "/" + extra.Name
		}
		mode := os.FileMode(extra.Perm)
		if extra.Kind == vfs.KindDir {
			mode |= os.ModeDir
		}
		infos = append(infos, newFileInfo(fullPath, extra.Size, mode, fs.mountTime))
	}

	for _, stat := range childStats {
		infos = append(infos, fs.statToFileInfo(stat))
	}

	return infos, nil
}

func (fs *GraphFS) MkdirAll(filename string, perm os.FileMode) error {
	return errReadOnly
}

// --- billy.Symlink ---

func (fs *GraphFS) Lstat(filename string) (os.FileInfo, error) {
	filename = cleanPath(filename)

	// Root — use dynamic ModTime from graph so NFS clients invalidate
	// cached directory listings when the underlying graph changes (e.g., HotSwapGraph.Swap).
	if filename == "/" {
		modTime := fs.mountTime
		if n, err := fs.graph.GetNode("/"); err == nil && !n.ModTime.IsZero() {
			modTime = n.ModTime
		}
		return newFileInfo("/", 0, os.ModeDir|0o555, modTime), nil
	}

	// Virtual paths: delegate to resolver
	if entry := fs.resolver.Resolve(filename); entry != nil {
		return fs.vEntryToFileInfo(filename, entry, fs.mountTime), nil
	}

	node, err := fs.graph.GetNode(filename)
	if err != nil {
		return nil, &os.PathError{Op: "lstat", Path: filename, Err: os.ErrNotExist}
	}

	return fs.nodeToFileInfo(node), nil
}

func (fs *GraphFS) Symlink(target, link string) error {
	return billy.ErrNotSupported
}

func (fs *GraphFS) Readlink(link string) (string, error) {
	return "", billy.ErrNotSupported
}

// --- billy.Chroot ---

func (fs *GraphFS) Chroot(path string) (billy.Filesystem, error) {
	return chroot.New(fs, path), nil
}

func (fs *GraphFS) Root() string {
	return "/"
}

// --- billy.Capable ---

func (fs *GraphFS) Capabilities() billy.Capability {
	caps := billy.ReadCapability | billy.SeekCapability
	if fs.writable {
		caps |= billy.WriteCapability
	}
	return caps
}

// vEntryToFileInfo converts a VEntry to os.FileInfo for NFS.
// For KindSymlink entries (callers/callees), NFS presents them as regular
// files since NFS callers/callees use graphFile (content access), not symlinks.
func (fs *GraphFS) vEntryToFileInfo(fullPath string, e *vfs.VEntry, modTime time.Time) os.FileInfo {
	mode := os.FileMode(e.Perm)
	size := e.Size
	switch e.Kind {
	case vfs.KindDir:
		mode |= os.ModeDir
	case vfs.KindSymlink:
		// NFS: callers/callees are regular files (graphFile), not symlinks.
		// Use the referenced node's content size, not the symlink target length.
		mode = 0o444
		if refNode, err := fs.graph.GetNode(e.NodeID); err == nil {
			size = refNode.ContentSize()
		}
	}
	return newFileInfo(fullPath, size, mode, modTime)
}

// --- internals ---

// cleanPath normalizes a billy path to a clean absolute path.
func cleanPath(path string) string {
	path = filepath.Clean("/" + path)
	if path == "." {
		return "/"
	}
	return path
}

// statToFileInfo converts a NodeStat (from ListChildStats) to os.FileInfo.
// Uses mountTime as fallback for zero ModTime. No GetNode call needed.
func (fs *GraphFS) statToFileInfo(s graph.NodeStat) os.FileInfo {
	mode := os.FileMode(0o444)
	if s.IsDir {
		mode = os.ModeDir | 0o555
	} else if s.HasOrigin {
		mode = 0o644
	}
	var size int64
	if s.IsDir {
		size = 4096
	} else {
		size = s.ContentSize
	}
	modTime := s.ModTime
	if modTime.IsZero() {
		modTime = fs.mountTime
	}
	return newFileInfo(s.ID, size, mode, modTime)
}

// nodeToFileInfo converts a graph.Node to os.FileInfo.
// Zero ModTime falls back to the stable mount time (not time.Now()) so that
// build tools see deterministic timestamps across remounts.
func (fs *GraphFS) nodeToFileInfo(n *graph.Node) os.FileInfo {
	mode := os.FileMode(0o444)
	if n.Mode.IsDir() {
		mode = os.ModeDir | 0o555
	} else if n.Origin != nil {
		mode = 0o644
	}
	var size int64
	if n.Mode.IsDir() {
		size = 4096
	} else {
		size = n.ContentSize()
	}

	modTime := n.ModTime
	if modTime.IsZero() {
		modTime = fs.mountTime
	}

	return newFileInfo(n.ID, size, mode, modTime)
}

// staticFileInfo implements os.FileInfo with static values.
// Sys() returns *syscall.Stat_t with user uid/gid and a unique Ino derived
// from FNV hash of the full path. Setting Ino=0 caused all NFS entries to
// share Fileid=0, breaking macOS NFS client directory listings.
type staticFileInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	modTime time.Time
	ino     uint64 // FNV hash of full path — unique Fileid for NFS
}

// newFileInfo creates a staticFileInfo with ino automatically derived from fullPath.
// Use this everywhere instead of &staticFileInfo{} to guarantee unique NFS Fileids.
func newFileInfo(fullPath string, size int64, mode os.FileMode, modTime time.Time) *staticFileInfo {
	return &staticFileInfo{
		name:    filepath.Base(fullPath),
		size:    size,
		mode:    mode,
		modTime: modTime,
		ino:     pathIno(fullPath),
	}
}

func (fi *staticFileInfo) Name() string       { return fi.name }
func (fi *staticFileInfo) Size() int64        { return fi.size }
func (fi *staticFileInfo) Mode() os.FileMode  { return fi.mode }
func (fi *staticFileInfo) ModTime() time.Time { return fi.modTime }
func (fi *staticFileInfo) IsDir() bool        { return fi.mode.IsDir() }
func (fi *staticFileInfo) Sys() any {
	return &syscall.Stat_t{
		Ino: fi.ino,
		Uid: uint32(os.Getuid()),
		Gid: uint32(os.Getgid()),
	}
}

// pathIno computes a stable unique inode number from a full path.
func pathIno(fullPath string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(fullPath))
	v := h.Sum64()
	if v == 0 {
		v = 1 // avoid zero — NFS clients treat Fileid=0 as invalid
	}
	return v
}

// Compile-time interface checks.
var (
	_ billy.Filesystem = (*GraphFS)(nil)
	_ billy.Capable    = (*GraphFS)(nil)
)

// Verify errReadOnly is a proper error.
var _ error = errReadOnly

// Verify file types satisfy billy.File.
var (
	_ billy.File = (*graphFile)(nil)
	_ billy.File = (*bytesFile)(nil)
)

// writeSession is the NFS client's bytes for one path, and the node's content
// as it stood right after those bytes were last committed or truncated. A
// node whose content has moved on since was changed by someone else, so the
// session is stale and is dropped.
type writeSession struct {
	buf      []byte
	nodeView []byte
}

// sessionBytes returns a copy of the client's bytes for filename when a write
// session exists and the node still holds what that session last left, and
// nil otherwise. A stale session is dropped.
func (fs *GraphFS) sessionBytes(filename string, current []byte) []byte {
	fs.sessionsMu.Lock()
	defer fs.sessionsMu.Unlock()
	s, ok := fs.sessions[filename]
	if !ok {
		return nil
	}
	if !bytes.Equal(s.nodeView, current) {
		delete(fs.sessions, filename)
		return nil
	}
	if s.buf == nil {
		return []byte{}
	}
	return bytes.Clone(s.buf)
}

// saveSession records the client's bytes for filename, against the node's
// content as it stands now.
func (fs *GraphFS) saveSession(filename string, buf []byte) {
	node, err := fs.graph.GetNode(filename)
	if err != nil {
		return
	}
	nodeView := fs.readAll(filename, node)
	fs.sessionsMu.Lock()
	defer fs.sessionsMu.Unlock()
	if fs.sessions == nil {
		fs.sessions = map[string]*writeSession{}
	}
	fs.sessions[filename] = &writeSession{buf: bytes.Clone(buf), nodeView: nodeView}
}

// readAll returns a node's whole content.
func (fs *GraphFS) readAll(filename string, node *graph.Node) []byte {
	size := node.ContentSize()
	if size <= 0 {
		return nil
	}
	buf := make([]byte, size)
	n, _ := fs.graph.ReadContent(filename, buf, 0)
	return buf[:n]
}
