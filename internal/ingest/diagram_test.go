package ingest

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"text/template"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- RenderTemplateWithFuncs ---

func TestRenderTemplateWithFuncs_MergesExtraFuncs(t *testing.T) {
	extra := template.FuncMap{
		"shout": func(s string) string { return strings.ToUpper(s) },
	}
	var cache sync.Map

	result, err := RenderTemplateWithFuncs(`{{shout "hello"}}`, nil, extra, &cache)
	require.NoError(t, err)
	assert.Equal(t, "HELLO", result)
}

func TestRenderTemplateWithFuncs_BaseFuncsStillWork(t *testing.T) {
	extra := template.FuncMap{
		"noop": func() string { return "" },
	}
	var cache sync.Map

	// json is from the base tmplFuncs
	result, err := RenderTemplateWithFuncs(`{{json .items}}`, map[string]any{
		"items": []string{"a", "b"},
	}, extra, &cache)
	require.NoError(t, err)
	assert.Equal(t, `["a","b"]`, result)
}

func TestRenderTemplateWithFuncs_CachesTemplates(t *testing.T) {
	extra := template.FuncMap{
		"echo": func(s string) string { return s },
	}
	var cache sync.Map

	tmpl := `{{echo .name}}`
	r1, err := RenderTemplateWithFuncs(tmpl, map[string]any{"name": "first"}, extra, &cache)
	require.NoError(t, err)
	assert.Equal(t, "first", r1)

	// Second call should use cached template
	r2, err := RenderTemplateWithFuncs(tmpl, map[string]any{"name": "second"}, extra, &cache)
	require.NoError(t, err)
	assert.Equal(t, "second", r2)

	// Verify it's cached
	_, ok := cache.Load(tmpl)
	assert.True(t, ok, "template should be in cache")
}

func TestRenderTemplateWithFuncs_EmptyExtraFuncs(t *testing.T) {
	var cache sync.Map

	result, err := RenderTemplateWithFuncs(`{{.name}}`, map[string]any{"name": "test"}, nil, &cache)
	require.NoError(t, err)
	assert.Equal(t, "test", result)
}

func TestRenderTemplateWithFuncs_InvalidTemplate(t *testing.T) {
	var cache sync.Map

	_, err := RenderTemplateWithFuncs(`{{.missing}`, nil, nil, &cache)
	assert.Error(t, err)
}

// --- DiagramFuncMap ---

// buildTestEngine creates an Engine with a MemoryStore populated with refs that
// form two distinct communities connected by a shared token.
func buildTestEngine(t *testing.T, diagrams map[string]api.DiagramDef) *Engine {
	t.Helper()

	store := graph.NewMemoryStore()

	// Community 1: nodes a1, a2, a3 share token "alpha"
	for _, id := range []string{"a1", "a2", "a3"} {
		store.AddNode(&graph.Node{ID: id})
		require.NoError(t, store.AddRef("alpha", id))
	}

	// Community 2: nodes b1, b2, b3 share token "beta"
	for _, id := range []string{"b1", "b2", "b3"} {
		store.AddNode(&graph.Node{ID: id})
		require.NoError(t, store.AddRef("beta", id))
	}

	// Bridge: a1 and b1 both reference "bridge" (creates cross-community edge)
	require.NoError(t, store.AddRef("bridge", "a1"))
	require.NoError(t, store.AddRef("bridge", "b1"))

	schema := &api.Topology{
		Version:  "v1",
		Diagrams: diagrams,
	}

	return NewEngine(schema, store)
}

func TestDiagramFuncMap_SystemDefault(t *testing.T) {
	// No schema diagrams defined; {{diagram "system"}} should still work
	// and use "TD" layout.
	engine := buildTestEngine(t, nil)

	fm := engine.DiagramFuncMap()
	require.Contains(t, fm, "diagram")

	diagramFn := fm["diagram"].(func(string) string)
	result := diagramFn("system")

	assert.True(t, strings.HasPrefix(result, "graph TD\n"),
		"expected 'graph TD' prefix, got: %s", result)
	assert.NotContains(t, result, "not defined")
}

func TestDiagramFuncMap_SchemaDefinedLayout(t *testing.T) {
	engine := buildTestEngine(t, map[string]api.DiagramDef{
		"architecture": {Layout: "LR"},
	})

	fm := engine.DiagramFuncMap()
	diagramFn := fm["diagram"].(func(string) string)
	result := diagramFn("architecture")

	assert.True(t, strings.HasPrefix(result, "graph LR\n"),
		"expected 'graph LR' prefix, got: %s", result)
}

func TestDiagramFuncMap_UnknownName(t *testing.T) {
	engine := buildTestEngine(t, map[string]api.DiagramDef{
		"deps": {Layout: "TD"},
	})

	fm := engine.DiagramFuncMap()
	diagramFn := fm["diagram"].(func(string) string)
	result := diagramFn("nonexistent")

	assert.Contains(t, result, `diagram "nonexistent" not defined`)
}

func TestDiagramFuncMap_SystemWithSchemaDiagrams(t *testing.T) {
	// When schema has diagrams defined, "system" should still work as default
	engine := buildTestEngine(t, map[string]api.DiagramDef{
		"custom": {Layout: "BT"},
	})

	fm := engine.DiagramFuncMap()
	diagramFn := fm["diagram"].(func(string) string)
	result := diagramFn("system")

	// "system" is not in schema but should use default TD layout
	assert.True(t, strings.HasPrefix(result, "graph TD\n"),
		"expected 'graph TD' prefix for implicit system, got: %s", result)
}

func TestDiagramFuncMap_SchemaOverridesSystem(t *testing.T) {
	// When schema explicitly defines "system", it should use that layout
	engine := buildTestEngine(t, map[string]api.DiagramDef{
		"system": {Layout: "RL"},
	})

	fm := engine.DiagramFuncMap()
	diagramFn := fm["diagram"].(func(string) string)
	result := diagramFn("system")

	assert.True(t, strings.HasPrefix(result, "graph RL\n"),
		"expected 'graph RL' prefix, got: %s", result)
}

func TestDiagramFuncMap_NoCommunities(t *testing.T) {
	// Store with no refs -> no communities
	store := graph.NewMemoryStore()
	engine := NewEngine(&api.Topology{Version: "v1"}, store)

	fm := engine.DiagramFuncMap()
	diagramFn := fm["diagram"].(func(string) string)
	result := diagramFn("system")

	assert.Contains(t, result, "no communities detected")
}

func TestDiagramFuncMap_Idempotent(t *testing.T) {
	engine := buildTestEngine(t, nil)

	fm1 := engine.DiagramFuncMap()
	fm2 := engine.DiagramFuncMap()

	// Should return the same FuncMap (pointer equality for the map)
	assert.Equal(t, fmt.Sprintf("%p", fm1), fmt.Sprintf("%p", fm2))
}

func TestDiagramFuncMap_CachesComputations(t *testing.T) {
	engine := buildTestEngine(t, nil)

	fm := engine.DiagramFuncMap()
	diagramFn := fm["diagram"].(func(string) string)

	// Call twice -- community detection should run only once (via sync.Once)
	r1 := diagramFn("system")
	r2 := diagramFn("system")

	assert.Equal(t, r1, r2, "repeated calls should produce identical output")

	// Verify cached data is populated
	assert.NotNil(t, engine.cachedCommunities)
	assert.NotNil(t, engine.cachedRefs)
}

// --- RenderContentTemplate ---

func TestRenderContentTemplate_DiagramInTemplate(t *testing.T) {
	engine := buildTestEngine(t, map[string]api.DiagramDef{
		"overview": {Layout: "LR"},
	})

	result, err := engine.RenderContentTemplate(
		`# Architecture\n{{diagram "overview"}}`,
		nil,
	)
	require.NoError(t, err)
	assert.Contains(t, result, "graph LR")
}

func TestRenderContentTemplate_StandardFuncsAvailable(t *testing.T) {
	engine := buildTestEngine(t, nil)

	result, err := engine.RenderContentTemplate(
		`{{json .items}}`,
		map[string]any{"items": []int{1, 2}},
	)
	require.NoError(t, err)
	assert.Equal(t, `[1,2]`, result)
}

func TestRenderContentTemplate_MixedFuncs(t *testing.T) {
	engine := buildTestEngine(t, map[string]api.DiagramDef{
		"deps": {Layout: "TD"},
	})

	result, err := engine.RenderContentTemplate(
		`name={{.name}} diagram={{diagram "deps"}}`,
		map[string]any{"name": "test"},
	)
	require.NoError(t, err)
	assert.Contains(t, result, "name=test")
	assert.Contains(t, result, "graph TD")
}

func TestRenderContentTemplate_NoDiagramUsed(t *testing.T) {
	// Template that doesn't use {{diagram}} should still work fine
	engine := buildTestEngine(t, nil)

	result, err := engine.RenderContentTemplate(
		`Hello {{.who}}!`,
		map[string]any{"who": "world"},
	)
	require.NoError(t, err)
	assert.Equal(t, "Hello world!", result)

	// Community detection should NOT have been triggered
	assert.Nil(t, engine.cachedCommunities, "communities should not be computed if diagram func not called")
}

func TestRenderContentTemplate_AllLayouts(t *testing.T) {
	for _, layout := range []string{"TD", "LR", "BT", "RL"} {
		t.Run(layout, func(t *testing.T) {
			engine := buildTestEngine(t, map[string]api.DiagramDef{
				"test": {Layout: layout},
			})

			result, err := engine.RenderContentTemplate(
				`{{diagram "test"}}`,
				nil,
			)
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(result, "graph "+layout+"\n"),
				"expected 'graph %s' prefix, got: %s", layout, result)
		})
	}
}

// --- Edge case: store without RefsMap ---

// minimalStore implements IngestionTarget without providing RefsMap,
// testing the fallback path when the store cannot provide refs.
type minimalStore struct {
	graph.Graph
	nodes map[string]*graph.Node
}

func (m *minimalStore) AddNode(n *graph.Node)    { m.nodes[n.ID] = n }
func (m *minimalStore) AddRoot(_ *graph.Node)    {}
func (m *minimalStore) AddRef(_, _ string) error { return nil }
func (m *minimalStore) AddDef(_, _ string) error { return nil }
func (m *minimalStore) DeleteFileNodes(_ string) {}
func (m *minimalStore) AddFileChildren(parent *graph.Node, files []*graph.Node) {
	for _, f := range files {
		m.nodes[f.ID] = f
		parent.Children = append(parent.Children, f.ID)
	}
	m.nodes[parent.ID] = parent
}

func (m *minimalStore) GetNode(id string) (*graph.Node, error) {
	if n, ok := m.nodes[id]; ok {
		return n, nil
	}
	return nil, graph.ErrNotFound
}
func (m *minimalStore) ListChildren(_ string) ([]string, error)              { return nil, nil }
func (m *minimalStore) ReadContent(_ string, _ []byte, _ int64) (int, error) { return 0, nil }
func (m *minimalStore) GetCallers(_ string) ([]*graph.Node, error)           { return nil, nil }
func (m *minimalStore) GetCallees(_ string) ([]*graph.Node, error)           { return nil, nil }
func (m *minimalStore) Invalidate(_ string)                                  {}
func (m *minimalStore) Act(_, _, _ string) (*graph.ActionResult, error) {
	return nil, graph.ErrActNotSupported
}

func TestDiagramFuncMap_StoreWithoutRefsMap(t *testing.T) {
	store := &minimalStore{nodes: make(map[string]*graph.Node)}
	engine := NewEngine(&api.Topology{Version: "v1"}, store)

	fm := engine.DiagramFuncMap()
	diagramFn := fm["diagram"].(func(string) string)
	result := diagramFn("system")

	assert.Contains(t, result, "no communities detected")
}
