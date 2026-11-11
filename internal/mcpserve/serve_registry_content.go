package mcpserve

import (
	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/leyline"
)

// ParserID forwards graph.ContentHasher to the base graph, the graph whose
// communities get_communities pushes as a sheaf topology. A base that is not
// one records no parserId, and says so in the words PushTopology refuses with.
func (lg *lazyGraph) ParserID() (string, error) {
	g, err := lg.get()
	if err != nil || g == nil {
		return "", err
	}
	ch, ok := g.(graph.ContentHasher)
	if !ok {
		return "", leyline.ErrNoContentHashes
	}
	return ch.ParserID()
}

// NodeHashes forwards graph.ContentHasher to the base graph. A base that is
// not one wrote no node_hash: no id has one.
func (lg *lazyGraph) NodeHashes(ids []string) (map[string][32]byte, error) {
	g, err := lg.get()
	if err != nil || g == nil {
		return nil, err
	}
	ch, ok := g.(graph.ContentHasher)
	if !ok {
		return map[string][32]byte{}, nil
	}
	return ch.NodeHashes(ids)
}
