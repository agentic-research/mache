package examples_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/ingest"
	machetmpl "github.com/agentic-research/mache/internal/template"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLLMRosetta_CanonicalProjection verifies that the LLM rosetta schema
// projects 6 different provider formats into a canonical tree where the
// same path means the same thing across all providers.
//
// This is a proof-of-concept for schema-driven API format translation:
// no code, no regex, no parser — just a declarative schema.
func TestLLMRosetta_CanonicalProjection(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	dir := filepath.Dir(thisFile)

	schemaBytes, err := os.ReadFile(filepath.Join(dir, "llm-rosetta-schema.json"))
	require.NoError(t, err)
	var schema api.Topology
	require.NoError(t, json.Unmarshal(schemaBytes, &schema))

	store := graph.NewMemoryStore()
	resolver := graph.NewSQLiteResolver(machetmpl.Render)
	defer resolver.Close()
	store.SetResolver(resolver.Resolve)

	engine := ingest.NewEngine(&schema, store)
	require.NoError(t, engine.Ingest(filepath.Join(dir, "llm-providers.json")))

	// All 6 providers should exist as top-level directories
	rootChildren, err := store.ListChildren("")
	require.NoError(t, err)
	providers := map[string]bool{}
	for _, id := range rootChildren {
		providers[id] = true
	}
	for _, name := range []string{"openai", "anthropic", "google", "ollama", "cohere", "mistral"} {
		assert.True(t, providers[name], "should have provider %s", name)
	}

	// Canonical field: response-content should be "Hello! How can I help?" for ALL providers
	for _, provider := range []string{"openai", "anthropic", "google", "ollama", "cohere", "mistral"} {
		node, err := store.GetNode(provider + "/response-content")
		require.NoError(t, err, "provider %s should have response-content", provider)
		content, err := readNodeContent(store, node)
		require.NoError(t, err)
		assert.Equal(t, "Hello! How can I help?", content,
			"%s/response-content should normalize to canonical text", provider)
	}

	// Canonical field: model should be provider-specific
	expectedModels := map[string]string{
		"openai": "gpt-4o", "anthropic": "claude-sonnet-4-20250514",
		"google": "gemini-2.0-flash", "ollama": "llama3",
		"cohere": "command-r-plus", "mistral": "mistral-large-latest",
	}
	for provider, expected := range expectedModels {
		node, err := store.GetNode(provider + "/model")
		require.NoError(t, err)
		content, err := readNodeContent(store, node)
		require.NoError(t, err)
		assert.Equal(t, expected, content, "%s/model", provider)
	}

	// Canonical field: input-tokens should be "12" for all
	for _, provider := range []string{"openai", "anthropic", "google", "ollama", "cohere", "mistral"} {
		node, err := store.GetNode(provider + "/input-tokens")
		require.NoError(t, err, "%s should have input-tokens", provider)
		content, err := readNodeContent(store, node)
		require.NoError(t, err)
		assert.Equal(t, "12", content, "%s/input-tokens", provider)
	}

	// Canonical field: finish-reason (provider-specific values, all non-empty)
	expectedReasons := map[string]string{
		"openai": "stop", "anthropic": "end_turn", "google": "STOP",
		"ollama": "stop", "cohere": "COMPLETE", "mistral": "stop",
	}
	for provider, expected := range expectedReasons {
		node, err := store.GetNode(provider + "/finish-reason")
		require.NoError(t, err)
		content, err := readNodeContent(store, node)
		require.NoError(t, err)
		assert.Equal(t, expected, content, "%s/finish-reason", provider)
	}
}

func readNodeContent(store *graph.MemoryStore, node *graph.Node) (string, error) {
	size := node.ContentSize()
	if size == 0 {
		return "", nil
	}
	buf := make([]byte, size)
	n, err := store.ReadContent(node.ID, buf, 0)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}
