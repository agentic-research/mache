package fs

import (
	"context"
	"syscall"

	"github.com/agentic-research/mache/api"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// MacheRoot is the root node of the Mache filesystem.
// It embeds fs.Inode to inherit standard FUSE node behavior (Lookup, Readdir, etc.).
type MacheRoot struct {
	fs.Inode

	// Source is the raw input data (e.g., JSON) that populates the FS.
	Source []byte
	// Schema defines the topology of the FS.
	Schema *api.Topology
}

var _ = (fs.NodeOnAdder)(nil)

// NewMacheRoot creates a new root node.
func NewMacheRoot(source []byte, schema *api.Topology) *MacheRoot {
	return &MacheRoot{
		Source: source,
		Schema: schema,
	}
}

// OnAdd is called when the node is added to the tree.
// We use this to populate the static topology or setup dynamic lookup.
// For now, we hardcode a "hello" file as requested.
func (r *MacheRoot) OnAdd(ctx context.Context) {
	// Constraint: "Just hardcode a 'Hello World' node to prove the mount works."
	
	// Define the child node (HelloFile)
	child := &HelloFile{
		Content: []byte("Hello, World!\n"),
	}

	// Add the child to the root Inode.
	// This automatically enables Lookup("hello") and Readdir() showing "hello".
	// The fs.Inode logic handles the syscalls.
	// mode is 0444 (read-only)
	r.AddChild("hello", r.NewInode(ctx, child, fs.StableAttr{Mode: fuse.S_IFREG | 0444}), true)
}

// HelloFile is a simple static file node.
type HelloFile struct {
	fs.Inode
	Content []byte
}

var _ = (fs.NodeReader)(nil)

// Read implements the fs.NodeReader interface.
// This allows cat/grep to read the file content.
func (f *HelloFile) Read(ctx context.Context, fh fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	end := int(off) + len(dest)
	if end > len(f.Content) {
		end = len(f.Content)
	}
	if int(off) < len(f.Content) {
		n := copy(dest, f.Content[off:end])
		return fuse.ReadResultData(dest[:n]), 0
	}
	return fuse.ReadResultData(nil), 0
}
