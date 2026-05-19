package ingest

import "github.com/agentic-research/mache/internal/graph"

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
}

func (b *bufferingTarget) AddNode(n *graph.Node) {
	if n.Mode.IsDir() {
		b.IngestionTarget.AddNode(n)
	} else { // coverage:ignore
		b.bufferedNodes = append(b.bufferedNodes, n) // coverage:ignore
	} // coverage:ignore
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
	}
	b.IngestionTarget.AddNode(parent)
}
