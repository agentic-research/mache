package ingest

import (
	"strings"

	"github.com/agentic-research/mache/internal/graph"
)

// --- Parallel ingestion types ---

// recordJob is sent from the SQLite reader to worker goroutines.
type recordJob struct {
	recordID string
	raw      string
}

// recordResult is the output from a worker: all nodes for one record.
type recordResult struct {
	nodes       []*graph.Node
	parentLinks []parentLink
	refLinks    []refLink
	err         error
}

type parentLink struct {
	childID  string
	parentID string
}

type refLink struct {
	token  string
	nodeID string
}

// bufferingTarget buffers file nodes for atomic replacement while passing
// directory updates through immediately.
type bufferingTarget struct {
	IngestionTarget
	bufferedNodes []*graph.Node
	// bufferedChildren maps a parent ID to the buffered file children not yet
	// written to the table, so ListChildren can answer completely. Without it
	// a construct dir reports zero children until ReplaceFileNodes runs, which
	// is the whole window ingest operates in.
	bufferedChildren map[string][]string
}

// ListChildren unions the underlying store's answer with the file nodes this
// target is still holding.
//
// Necessary because AddNode defers file nodes for a later ReplaceFileNodes
// swap while passing directory nodes straight through. Asking the underlying
// SQLiteWriter alone would report a construct directory as childless for the
// entire duration of ingest — technically true of the table, and useless as an
// answer. Children have one accessor; it has to be right (mache-e3d9bb).
func (b *bufferingTarget) ListChildren(id string) ([]string, error) {
	base, err := b.IngestionTarget.ListChildren(id)
	if err != nil {
		return nil, err
	}
	pending := b.bufferedChildren[id]
	if len(pending) == 0 {
		return base, nil
	}
	seen := make(map[string]bool, len(base))
	for _, c := range base {
		seen[c] = true
	}
	out := append([]string(nil), base...)
	for _, c := range pending {
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out, nil
}

// noteBuffered records a buffered node's parent so ListChildren can find it.
func (b *bufferingTarget) noteBuffered(parentID, childID string) {
	if parentID == "" {
		return
	}
	if b.bufferedChildren == nil {
		b.bufferedChildren = make(map[string][]string)
	}
	b.bufferedChildren[parentID] = append(b.bufferedChildren[parentID], childID)
}

func (b *bufferingTarget) AddNode(n *graph.Node) {
	if n.Mode.IsDir() {
		b.IngestionTarget.AddNode(n)
		return
	}
	b.bufferedNodes = append(b.bufferedNodes, n)
	// Derive the parent from the ID — a buffered file's parent is its path
	// prefix — so ListChildren sees it before the swap.
	if i := strings.LastIndex(n.ID, "/"); i > 0 {
		b.noteBuffered(n.ID[:i], n.ID)
	}
}

func (b *bufferingTarget) AddDef(token, dirID string) error {
	return b.IngestionTarget.AddDef(token, dirID)
}

// AddFileChildren buffers file nodes for the later ReplaceFileNodes atomic swap
// and passes the parent dir update through immediately (same as AddNode for dirs).
// Children are appended in-memory here; the real store sees the complete parent.
// Safe without locking because bufferingTarget is single-goroutine.
//
// Multi-call invariant (bead mache-ad3f75): when processNode is invoked
// repeatedly for the same parent ID across multiple matches, each call
// constructs a fresh `parent` node by re-fetching the existing Children
// from the store before the file loop. The append below therefore extends
// the most recently published Children list rather than the stale slice
// held by the previous call. Re-publishing the parent via AddNode then
// installs the merged list. If processNode were ever changed to reuse a
// `parent` pointer across calls, this loop would silently lose children.
func (b *bufferingTarget) AddFileChildren(parent *graph.Node, files []*graph.Node) {
	b.bufferedNodes = append(b.bufferedNodes, files...)
	for _, f := range files {
		parent.Children = append(parent.Children, f.ID)
		b.noteBuffered(parent.ID, f.ID)
	}
	b.IngestionTarget.AddNode(parent)
}
