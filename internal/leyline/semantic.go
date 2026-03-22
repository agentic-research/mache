// Package leyline — semantic.go provides typed methods for the ley-line daemon's
// semantic search operations (embedding-based KNN search).
//
// SemanticClient wraps a SocketClient and translates between mache's search
// requests and the daemon's semantic_search / embed_status / embed_content ops.
// All methods are no-ops when the underlying SocketClient is nil, making the
// integration fully optional.
package leyline

import "fmt"

// SemanticClient provides typed access to ley-line's embedding operations.
// A nil SemanticClient is safe — all methods return zero values without error.
type SemanticClient struct {
	sock *SocketClient
}

// NewSemanticClient wraps an existing SocketClient. sock may be nil.
func NewSemanticClient(sock *SocketClient) *SemanticClient {
	return &SemanticClient{sock: sock}
}

// SemanticResult is a single KNN search result.
type SemanticResult struct {
	ID       string  `json:"id"`
	Distance float64 `json:"distance"`
}

// EmbedStatus reports the state of the embedding index.
type EmbedStatus struct {
	Ready      bool   `json:"ready"`
	Count      int    `json:"count"`
	Dimensions int    `json:"dimensions"`
	Model      string `json:"model"`
}

// NodeContent is a node to embed: id + text content.
type NodeContent struct {
	ID      string `json:"id"`
	Content string `json:"content"`
}

// Search performs a semantic search over embedded nodes.
// Returns results sorted by distance (closest first).
func (sc *SemanticClient) Search(query string, k int) ([]SemanticResult, error) {
	if sc == nil || sc.sock == nil {
		return nil, nil
	}
	if k <= 0 {
		k = 10
	}

	resp, err := sc.sock.SendOp(map[string]any{
		"op":    "semantic_search",
		"query": query,
		"k":     k,
	})
	if err != nil {
		return nil, fmt.Errorf("semantic_search: %w", err)
	}
	if errMsg, ok := resp["error"].(string); ok {
		return nil, fmt.Errorf("semantic_search: %s", errMsg)
	}

	rawResults, ok := resp["results"].([]any)
	if !ok {
		return nil, nil
	}

	results := make([]SemanticResult, 0, len(rawResults))
	for _, raw := range rawResults {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["id"].(string)
		dist, _ := m["distance"].(float64)
		if id != "" {
			results = append(results, SemanticResult{ID: id, Distance: dist})
		}
	}
	return results, nil
}

// Status returns the current embedding index status.
func (sc *SemanticClient) Status() (EmbedStatus, error) {
	if sc == nil || sc.sock == nil {
		return EmbedStatus{}, nil
	}

	resp, err := sc.sock.SendOp(map[string]any{"op": "embed_status"})
	if err != nil {
		return EmbedStatus{}, fmt.Errorf("embed_status: %w", err)
	}

	status := EmbedStatus{}
	if v, ok := resp["ready"].(bool); ok {
		status.Ready = v
	}
	if v, ok := resp["count"].(float64); ok {
		status.Count = int(v)
	}
	if v, ok := resp["dimensions"].(float64); ok {
		status.Dimensions = int(v)
	}
	if v, ok := resp["model"].(string); ok {
		status.Model = v
	}
	return status, nil
}

// EmbedContent pushes node content to the daemon for embedding.
// Returns the number of nodes successfully embedded.
func (sc *SemanticClient) EmbedContent(nodes []NodeContent) (int, error) {
	if sc == nil || sc.sock == nil {
		return 0, nil
	}

	resp, err := sc.sock.SendOp(map[string]any{
		"op":    "embed_content",
		"nodes": nodes,
	})
	if err != nil {
		return 0, fmt.Errorf("embed_content: %w", err)
	}
	if errMsg, ok := resp["error"].(string); ok {
		return 0, fmt.Errorf("embed_content: %s", errMsg)
	}

	embedded := 0
	if v, ok := resp["embedded"].(float64); ok {
		embedded = int(v)
	}
	return embedded, nil
}
