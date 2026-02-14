// Package nfsmount provides an NFS-based mount backend for mache.
// It adapts mache's graph.Graph interface to billy.Filesystem for use
// with willscott/go-nfs, replacing the FUSE mount layer.
package nfsmount

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	billy "github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/helper/chroot"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
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
}

// NewGraphFS creates a billy.Filesystem backed by a mache Graph.
func NewGraphFS(g graph.Graph, schema *api.Topology) *GraphFS {
	sj, _ := json.MarshalIndent(schema, "", "  ")
	sj = append(sj, '\n')
	return &GraphFS{
		graph:      g,
		schema:     schema,
		schemaJSON: sj,
		mountTime:  time.Now(),
	}
}

// SetWriteBack enables write support. The callback is invoked when a
// written file is closed, triggering the splice pipeline.
func (fs *GraphFS) SetWriteBack(fn WriteBackFunc) {
	fs.writable = true
	fs.writeBack = fn
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

	// Virtual: _schema.json
	if filename == "/_schema.json" {
		return &bytesFile{name: "_schema.json", data: fs.schemaJSON}, nil
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
	if filename == "/_schema.json" {
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

	// Pre-fill buffer with existing content (for O_RDWR / partial writes)
	var buf []byte
	if flag&os.O_TRUNC == 0 {
		size := node.ContentSize()
		if size > 0 {
			buf = make([]byte, size)
			n, _ := fs.graph.ReadContent(filename, buf, 0)
			buf = buf[:n]
		}
	}

	return &writeFile{
		id:      filename,
		origin:  *node.Origin,
		buf:     buf,
		onClose: fs.writeBack,
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

	node, err := fs.resolveNode(path)
	if err != nil && path != "/" {
		return nil, &os.PathError{Op: "readdir", Path: path, Err: os.ErrNotExist}
	}
	if node != nil && !node.Mode.IsDir() {
		return nil, &os.PathError{Op: "readdir", Path: path, Err: fmt.Errorf("not a directory")}
	}

	children, err := fs.graph.ListChildren(path)
	if err != nil {
		return nil, &os.PathError{Op: "readdir", Path: path, Err: os.ErrNotExist}
	}

	infos := make([]os.FileInfo, 0, len(children)+1)

	// Virtual files at root
	if path == "/" {
		infos = append(infos, &staticFileInfo{
			name:    "_schema.json",
			size:    int64(len(fs.schemaJSON)),
			mode:    0o444,
			modTime: fs.mountTime,
		})
	}

	for _, childID := range children {
		childNode, err := fs.graph.GetNode(childID)
		if err != nil {
			continue
		}
		infos = append(infos, nodeToFileInfo(childNode))
	}

	return infos, nil
}

func (fs *GraphFS) MkdirAll(filename string, perm os.FileMode) error {
	return errReadOnly
}

// --- billy.Symlink ---

func (fs *GraphFS) Lstat(filename string) (os.FileInfo, error) {
	filename = cleanPath(filename)

	// Root
	if filename == "/" {
		return &staticFileInfo{
			name:    "/",
			mode:    os.ModeDir | 0o555,
			modTime: fs.mountTime,
		}, nil
	}

	// Virtual: _schema.json
	if filename == "/_schema.json" {
		return &staticFileInfo{
			name:    "_schema.json",
			size:    int64(len(fs.schemaJSON)),
			mode:    0o444,
			modTime: fs.mountTime,
		}, nil
	}

	node, err := fs.resolveNode(filename)
	if err != nil {
		return nil, &os.PathError{Op: "lstat", Path: filename, Err: os.ErrNotExist}
	}

	return nodeToFileInfo(node), nil
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

// --- internals ---

// resolveNode looks up a graph node, handling path normalization.
func (fs *GraphFS) resolveNode(path string) (*graph.Node, error) {
	node, err := fs.graph.GetNode(path)
	if err != nil {
		return nil, err
	}
	return node, nil
}

// cleanPath normalizes a billy path to a clean absolute path.
func cleanPath(path string) string {
	path = filepath.Clean("/" + path)
	if path == "." {
		return "/"
	}
	return path
}

// nodeToFileInfo converts a graph.Node to os.FileInfo.
func nodeToFileInfo(n *graph.Node) os.FileInfo {
	mode := os.FileMode(0o444)
	if n.Mode.IsDir() {
		mode = os.ModeDir | 0o555
	} else if n.Origin != nil {
		mode = 0o644
	}
	size := n.ContentSize()

	modTime := n.ModTime
	if modTime.IsZero() {
		modTime = time.Now()
	}

	return &staticFileInfo{
		name:    filepath.Base(n.ID),
		size:    size,
		mode:    mode,
		modTime: modTime,
	}
}

// staticFileInfo implements os.FileInfo with static values.
type staticFileInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	modTime time.Time
}

func (fi *staticFileInfo) Name() string       { return fi.name }
func (fi *staticFileInfo) Size() int64        { return fi.size }
func (fi *staticFileInfo) Mode() os.FileMode  { return fi.mode }
func (fi *staticFileInfo) ModTime() time.Time { return fi.modTime }
func (fi *staticFileInfo) IsDir() bool        { return fi.mode.IsDir() }
func (fi *staticFileInfo) Sys() interface{}   { return nil }

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
