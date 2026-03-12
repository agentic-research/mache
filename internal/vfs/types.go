// Package vfs provides a pluggable virtual handler chain for mache's
// virtual path types (_schema.json, PROMPT.txt, _diagnostics/, context,
// callers/, callees/, .query/). Both the FUSE and NFS backends delegate
// to a shared Resolver instead of duplicating if-chains.
package vfs

import "github.com/agentic-research/mache/internal/graph"

// EntryKind classifies a virtual entry.
type EntryKind int

const (
	KindNone    EntryKind = iota
	KindDir               // Virtual directory (e.g., _diagnostics/, callers/)
	KindFile              // Virtual regular file (e.g., _schema.json, context)
	KindSymlink           // Virtual symlink (e.g., callers/<entry>, callees/<entry>)
)

// VEntry is the stat result for a virtual path.
type VEntry struct {
	Kind    EntryKind
	Size    int64
	Perm    uint32 // Unix permission bits (e.g. 0o444, 0o555)
	Content []byte // File content (KindFile) or symlink target (KindSymlink)
	NodeID  string // Graph node ID — NFS uses this to create graphFile
}

// DirExtra is a child entry injected into a real directory listing.
type DirExtra struct {
	Name string
	Kind EntryKind
	Size int64
	Perm uint32
}

// VHandler resolves one family of virtual paths (e.g., callers/, _diagnostics/).
type VHandler interface {
	// Match returns true if this handler owns the given path.
	Match(path string) bool

	// Stat returns a VEntry for the path, or nil if the path does not exist.
	Stat(path string) *VEntry

	// ReadContent returns the byte content for a virtual file.
	// Returns (nil, false) if not applicable.
	ReadContent(path string) ([]byte, bool)

	// ListDir returns directory entries for a virtual directory.
	// Returns (nil, false) if not applicable (i.e. path is not a virtual dir).
	ListDir(path string) ([]DirExtra, bool)

	// DirExtras returns extra entries to inject into a real directory listing.
	// Called for every non-virtual directory to let handlers add their entries
	// (e.g., _diagnostics/ added to writable dirs, callers/ added when callers exist).
	DirExtras(parentPath string, node *graph.Node) []DirExtra
}
