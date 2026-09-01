package graph

import (
	"encoding/json"
	"errors"
	"io/fs"
	"time"
)

var ErrNotFound = errors.New("node not found")

// ErrActNotSupported is returned by Graph implementations that do not support actions.
var ErrActNotSupported = errors.New("act not supported by this graph")

// ActionResult is returned when an action is performed on a graph node.
// Used by interactive graphs (browser DOM, iTerm2 sessions, macOS AX elements).
type ActionResult struct {
	NodeID  string `json:"node_id"`           // mache ID of the acted-upon node
	Action  string `json:"action"`            // "click", "type", "enter", "focus"
	Path    string `json:"path"`              // filesystem path
	Payload string `json:"payload,omitempty"` // optional (e.g., typed text)
}

// ContentRef is a recipe for lazily resolving file content from a backing store.
// Instead of storing the full byte content in RAM, we store enough info to re-fetch it on demand.
type ContentRef struct {
	DBPath     string // Path to the SQLite database
	RecordID   string // Row ID in the results table
	Template   string // Content template to re-render
	ContentLen int64  // Pre-computed rendered byte length
}

// SourceOrigin tracks the byte range of a construct in its source file.
// Used by write-back to splice edits into the original source.
type SourceOrigin struct {
	FilePath  string `json:"file"`
	StartByte uint32 `json:"start_byte"`
	EndByte   uint32 `json:"end_byte"`

	// Line/column of the same range, 1-based, or 0 when unknown.
	//
	// Byte offsets are the right primitive for write-back — a splice needs
	// bytes — but they are the wrong unit for a READER. No editor, terminal,
	// or LLM context references source by byte; everything downstream speaks
	// "path:line". Carrying both means a consumer never has to re-read the
	// file and count newlines to act on a result (mache-e57065).
	//
	// 1-based because that is what every consumer of a line number expects.
	// ley-line-open's `_ast` stores tree-sitter's 0-based rows, so the
	// producer side adds one; 0 here therefore means "not known", never
	// "first line". Columns follow the same convention.
	StartLine uint32 `json:"start_line,omitempty"`
	StartCol  uint32 `json:"start_col,omitempty"`
	EndLine   uint32 `json:"end_line,omitempty"`
	EndCol    uint32 `json:"end_col,omitempty"`
}

// NodeStat holds the immutable stat fields needed for directory listing.
// Value type — no pointers to mutable store data. Copying a NodeStat gives
// the caller a frozen snapshot that is safe to read without any lock.
type NodeStat struct {
	ID          string
	IsDir       bool
	ContentSize int64
	ModTime     time.Time
	HasOrigin   bool // true if write-back is possible (Origin != nil)
}

// Node is the universal primitive.
// The Mode field explicitly declares whether this is a file or directory.
type Node struct {
	ID        string
	Mode      fs.FileMode // fs.ModeDir for directories, 0 for regular files
	ModTime   time.Time   // Modification time
	Data      []byte      // Inline content (small files, nil for lazy nodes)
	Context   []byte      // Context content (imports/globals, for virtual 'context' file)
	DraftData []byte      // Draft content (uncommitted/invalid edits)
	Ref       *ContentRef // Lazy content reference (large files, nil for inline nodes)
	// Properties holds node metadata as JSON values (lang/pkg/imports/location/
	// ast_*). Access it only via PropString/PropRaw and their setters — the
	// encoding is their business (mache-90b89b).
	Properties map[string]json.RawMessage

	Children []string      // Child node IDs (directories only)
	Origin   *SourceOrigin // Source byte range (nil for dirs, JSON, SQLite nodes)
}

// ContentSize returns the byte length of this node's content,
// regardless of whether it is inline or lazy.
func (n *Node) ContentSize() int64 {
	if n.DraftData != nil {
		return int64(len(n.DraftData))
	}
	if n.Data != nil {
		return int64(len(n.Data))
	}
	if n.Ref != nil {
		return n.Ref.ContentLen
	}
	return 0
}

// ContentResolverFunc resolves a ContentRef into byte content.
type ContentResolverFunc func(ref *ContentRef) ([]byte, error)

// QualifiedCall represents a function call with optional package qualifier.
type QualifiedCall struct {
	Token     string // Function/method name (e.g., "Validate")
	Qualifier string // Package qualifier (e.g., "auth"); empty for unqualified calls
}

// CallExtractor parses source code and returns qualified function call tokens.
// Used for on-demand "callees/" resolution.
// langName is the tree-sitter language identifier (e.g. "go", "python").
type CallExtractor func(content []byte, path, langName string) ([]QualifiedCall, error)

// ScopedCallExtractor returns qualified call tokens made BY a single
// construct, scoped to it. Unlike CallExtractor, it is keyed on the
// construct's REAL `_ast`/`_source` id and `_ast` scope node id — not a
// graph node id and not source content — so it works directly against a
// pre-parsed `_ast` table without re-deriving anything from the graph.
//
// GetCallees prefers this over the legacy content-based CallExtractor when
// the construct's node carries "ast_source_id"/"ast_scope_id" Properties
// (persisted at projection time; see ingest.ASTScope). Feeding the graph
// node id as a source_id into the old content-based path matched zero
// `_ast` rows every time — the root cause of find_callees silently
// returning nothing on the serve/mount path (bead mache-fd9982).
type ScopedCallExtractor func(sourceID, scopeID, langName string) ([]QualifiedCall, error)

// Graph is the interface for the FUSE layer.
// This allows us to swap the backend later (Memory -> SQLite -> Mmap).
type Graph interface {
	GetNode(id string) (*Node, error)
	ListChildren(id string) ([]string, error)
	// ListChildStats returns stat snapshots for all children of a directory.
	// Eliminates N individual GetNode calls during FUSE/NFS readdir.
	// Returns []NodeStat (value types, safe to read without a lock).
	ListChildStats(id string) ([]NodeStat, error)
	ReadContent(id string, buf []byte, offset int64) (int, error)
	GetCallers(token string) ([]*Node, error)
	GetCallees(id string) ([]*Node, error)
	// Invalidate evicts cached data for a node (size, content).
	// Called after write-back to force re-render on next access.
	Invalidate(id string)
	// Act performs an action on the node at the given path.
	// Interactive graphs (browser DOM, terminal sessions, macOS AX elements)
	// implement real actions. Passive graphs (code, data) return ErrActNotSupported.
	Act(id, action, payload string) (*ActionResult, error)
}

// NodesForPathProvider is implemented by graph backends that can answer
// "which node IDs originated from this file?". MemoryStore implements it;
// other backends (SQLiteGraph, CompositeGraph) may either implement it
// directly or compose it via type assertion on their inner store.
//
// Kept as a separate interface (rather than added to Graph) so consumers
// that don't need this query — most graph readers — aren't forced to
// implement it. Watcher-driven invalidation paths type-assert and degrade
// gracefully when the backing store doesn't satisfy the interface.
type NodesForPathProvider interface {
	NodesForPath(filePath string) []string
}

// NormalizeID strips a leading slash from node IDs.
// FUSE/NFS paths use "/foo/source" but graph keys are "foo/source".
// Only strips one leading slash; "//double" becomes "/double".
func NormalizeID(id string) string {
	if len(id) > 0 && id[0] == '/' {
		return id[1:]
	}
	return id
}

// parentOfNodeID returns the parent ID by stripping the last `/segment`.
// Top-level IDs (no `/`) return "". Used by deleteFileNodes to find the
// minimal set of parents whose Children slices need pruning.
func parentOfNodeID(id string) string {
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == '/' {
			return id[:i]
		}
	}
	return ""
}

// SliceContent copies content bytes into buf at the given offset.
// Returns the number of bytes copied. Shared by all ReadContent implementations.
func SliceContent(data, buf []byte, offset int64) int {
	if offset < 0 || offset >= int64(len(data)) {
		return 0
	}
	end := min(offset+int64(len(buf)), int64(len(data)))
	return copy(buf, data[offset:end])
}
