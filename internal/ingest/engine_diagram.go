package ingest

import (
	"fmt"
	"text/template"

	"github.com/agentic-research/mache/internal/graph"
)

// refsProvider is the subset of stores that expose their refs map.
type refsProvider interface {
	RefsMap() map[string][]string
}

// ensureDiagramData lazily computes and caches the CommunityResult and refs.
// Safe for concurrent use; the computation runs at most once.
func (e *Engine) ensureDiagramData() {
	e.diagramOnce.Do(func() {
		rp, ok := e.Store.(refsProvider)
		if !ok {
			return
		}
		e.cachedRefs = rp.RefsMap()
		if len(e.cachedRefs) > 0 {
			e.cachedCommunities = graph.DetectCommunities(e.cachedRefs, 2)
		}
	})
}

// DiagramFuncMap returns a template.FuncMap containing the {{diagram "name"}}
// function. The returned FuncMap is built once via sync.Once and reused;
// the closure inside captures the Engine and lazily initializes community
// data on first call. Safe for concurrent use.
func (e *Engine) DiagramFuncMap() template.FuncMap {
	e.diagramFuncMapOnce.Do(func() {
		e.diagramFuncMap = template.FuncMap{
			"diagram": func(name string) string {
				e.ensureDiagramData()

				if e.cachedCommunities == nil || len(e.cachedCommunities.Communities) == 0 {
					return "%% diagram: no communities detected"
				}

				// Determine layout from schema diagrams map or default.
				layout := "TD"
				if e.Schema != nil && e.Schema.Diagrams != nil {
					if def, ok := e.Schema.Diagrams[name]; ok {
						layout = def.Layout
					} else if name != "system" {
						return fmt.Sprintf("%% diagram %q not defined", name)
					}
				}

				q := graph.ComputeQuotient(e.cachedCommunities, e.cachedRefs)
				return q.Mermaid(layout)
			},
		}
	})
	return e.diagramFuncMap
}

// RenderContentTemplate renders a content template with the standard mache
// functions plus the Engine's diagram function. This is the method that
// processNode and collectNodes should use for file content rendering.
func (e *Engine) RenderContentTemplate(tmpl string, values map[string]any) (string, error) {
	return RenderTemplateWithFuncs(tmpl, values, e.DiagramFuncMap(), &e.diagramTmplCache)
}
