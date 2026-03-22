package leyline

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSemanticClient_NilSafety(t *testing.T) {
	// Nil client should return empty results, not errors
	var sc *SemanticClient

	results, err := sc.Search("test", 10)
	assert.NoError(t, err)
	assert.Nil(t, results)

	status, err := sc.Status()
	assert.NoError(t, err)
	assert.Equal(t, EmbedStatus{}, status)

	n, err := sc.EmbedContent([]NodeContent{{ID: "a", Content: "hello"}})
	assert.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestSemanticClient_NilSocket(t *testing.T) {
	sc := NewSemanticClient(nil)

	results, err := sc.Search("test", 10)
	assert.NoError(t, err)
	assert.Nil(t, results)
}

func TestSemanticClient_Search(t *testing.T) {
	sockPath := mockServer(t, func(req map[string]any) map[string]any {
		op, _ := req["op"].(string)
		if op != "semantic_search" {
			return map[string]any{"error": "unexpected op"}
		}
		query, _ := req["query"].(string)
		if query == "" {
			return map[string]any{"error": "empty query"}
		}
		k, _ := req["k"].(float64)
		_ = k
		return map[string]any{
			"results": []any{
				map[string]any{"id": "go/auth/Validate/source", "distance": 0.234},
				map[string]any{"id": "go/middleware/Check/source", "distance": 0.456},
			},
			"count": 2,
		}
	})

	sock, err := DialSocket(sockPath)
	require.NoError(t, err)
	defer func() { _ = sock.Close() }()

	sc := NewSemanticClient(sock)
	results, err := sc.Search("authentication", 10)
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "go/auth/Validate/source", results[0].ID)
	assert.InDelta(t, 0.234, results[0].Distance, 0.001)
	assert.Equal(t, "go/middleware/Check/source", results[1].ID)
}

func TestSemanticClient_Search_Error(t *testing.T) {
	sockPath := mockServer(t, func(req map[string]any) map[string]any {
		return map[string]any{"error": "embeddings not enabled"}
	})

	sock, err := DialSocket(sockPath)
	require.NoError(t, err)
	defer func() { _ = sock.Close() }()

	sc := NewSemanticClient(sock)
	_, err = sc.Search("test", 5)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "embeddings not enabled")
}

func TestSemanticClient_Status(t *testing.T) {
	sockPath := mockServer(t, func(req map[string]any) map[string]any {
		return map[string]any{
			"ready":      true,
			"count":      float64(1523),
			"dimensions": float64(384),
			"model":      "minilm-q",
		}
	})

	sock, err := DialSocket(sockPath)
	require.NoError(t, err)
	defer func() { _ = sock.Close() }()

	sc := NewSemanticClient(sock)
	status, err := sc.Status()
	require.NoError(t, err)
	assert.True(t, status.Ready)
	assert.Equal(t, 1523, status.Count)
	assert.Equal(t, 384, status.Dimensions)
	assert.Equal(t, "minilm-q", status.Model)
}

func TestSemanticClient_Status_NotReady(t *testing.T) {
	sockPath := mockServer(t, func(req map[string]any) map[string]any {
		return map[string]any{"ready": false}
	})

	sock, err := DialSocket(sockPath)
	require.NoError(t, err)
	defer func() { _ = sock.Close() }()

	sc := NewSemanticClient(sock)
	status, err := sc.Status()
	require.NoError(t, err)
	assert.False(t, status.Ready)
	assert.Equal(t, 0, status.Count)
}

func TestSemanticClient_EmbedContent(t *testing.T) {
	sockPath := mockServer(t, func(req map[string]any) map[string]any {
		nodes, ok := req["nodes"].([]any)
		if !ok {
			return map[string]any{"error": "missing nodes"}
		}
		return map[string]any{"ok": true, "embedded": float64(len(nodes))}
	})

	sock, err := DialSocket(sockPath)
	require.NoError(t, err)
	defer func() { _ = sock.Close() }()

	sc := NewSemanticClient(sock)
	n, err := sc.EmbedContent([]NodeContent{
		{ID: "a", Content: "func Validate()"},
		{ID: "b", Content: "func Check()"},
	})
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}

func TestSemanticClient_DefaultK(t *testing.T) {
	var receivedK float64
	sockPath := mockServer(t, func(req map[string]any) map[string]any {
		receivedK, _ = req["k"].(float64)
		return map[string]any{"results": []any{}, "count": 0}
	})

	sock, err := DialSocket(sockPath)
	require.NoError(t, err)
	defer func() { _ = sock.Close() }()

	sc := NewSemanticClient(sock)
	_, _ = sc.Search("test", 0) // k=0 should default to 10
	assert.Equal(t, float64(10), receivedK)
}
