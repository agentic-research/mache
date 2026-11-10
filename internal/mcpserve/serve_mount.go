package mcpserve

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/leylinegraph"
	"github.com/agentic-research/mache/internal/projcfg"
	"github.com/agentic-research/mache/internal/schemainfer"
)

// --mount: serving the composite of several source trees (mache-iegm).

// buildMaybeMultiGraph dispatches between single-source and composite
// (multi-mount) construction. With no --mount flags, it's a thin
// pass-through to buildServeGraph (propagating its *SheafInvalidator).
// With one or more --mount NAME=PATH flags, it builds a CompositeGraph
// by calling buildServeGraph for each mount path and Mount-ing the
// result under NAME. The composite case returns a nil *SheafInvalidator
// — per-mount watchers each have their own invalidator captured in
// their closures, and there's no semantically correct way to expose
// a single composite invalidator to the MCP handler layer (each mount
// has its own CommunityResult). Composite mounts forfeit the cascade
// in exchange for the multi-mount feature; document for the wiring PR
// that revisits this if a use case emerges.
//
// When --mount is set, the positional dataSource is rejected — the
// caller must choose between a single source and a composite.
//
// Cleanup runs in reverse-mount order so child graphs are torn down
// before any shared resources they depend on.
func buildMaybeMultiGraph(dataSource string, schema *api.Topology) (graph.Graph, *graph.SheafInvalidator, func(), error) {
	if len(serveMounts) == 0 {
		return buildServeGraph(dataSource, schema)
	}
	if dataSource != "" {
		return nil, nil, func() {}, fmt.Errorf("cannot use both a positional source (%q) and --mount; use one or the other", dataSource)
	}

	composite := graph.NewCompositeGraph()
	// Global cross-mount fallback extractor. Since ADR-0012 step 4 removed
	// in-process CGO tree-sitter, the fallback is a no-op — cross-mount
	// callees resolve through the per-mount picker below (which reads each
	// mount's `_ast`); a mount with no AST simply contributes no calls.
	composite.SetCallExtractor(leylinegraph.NoopCallExtractor())

	// Wire the per-mount picker (ADR-0012): for SQLiteGraph mounts that carry
	// `_ast`, use the pure-Go ASTWalker extractor; the picker returns nil for
	// other backends so cross-mount resolution falls through to the global
	// no-op above. Each mount picks independently — heterogeneous fleets
	// (e.g. one ll-open .db and one mache-built .db) get the right extractor.
	composite.SetCallExtractorPicker(func(local graph.Graph) graph.CallExtractor {
		sg, ok := local.(*graph.SQLiteGraph)
		if !ok {
			return nil
		}
		return leylinegraph.PickCallExtractor(sg.DB())
	})

	var cleanups []func()
	runAll := func() {
		// Reverse order so later mounts close before earlier ones.
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}

	for _, spec := range serveMounts {
		name, path, ok := strings.Cut(spec, "=")
		if !ok || name == "" || path == "" {
			runAll()
			return nil, nil, func() {}, fmt.Errorf("invalid --mount spec %q (expected NAME=PATH)", spec)
		}
		ms, err := mountSchema(path, schema)
		if err != nil {
			runAll()
			return nil, nil, func() {}, fmt.Errorf("mount %s=%s: %w", name, path, err)
		}
		g, _, cleanup, err := buildServeGraph(path, ms)
		if err != nil {
			runAll()
			return nil, nil, func() {}, fmt.Errorf("mount %s=%s: %w", name, path, err)
		}
		cleanups = append(cleanups, cleanup)
		if err := composite.Mount(name, g); err != nil {
			runAll()
			return nil, nil, func() {}, fmt.Errorf("mount %q: %w", name, err)
		}
		log.Printf("mounted %s -> %s", name, path)
	}
	return composite, nil, runAll, nil
}

// mountsRoot is the registry key every --mount session shares: the mount set
// itself, since there is no single source tree to name the graph after.
func mountsRoot() string { return "mounts:" + strings.Join(serveMounts, ",") }

// mountSchema is the schema one mount is served with: schema when the caller
// gives one, else --schema when set, else the schema inferred for that
// mount's own tree, so a Go repo mounted beside a Rust one is not forced
// through one language's schema.
func mountSchema(path string, schema *api.Topology) (*api.Topology, error) {
	if schema != nil {
		return schema, nil
	}
	if serveSchema != "" {
		return projcfg.ResolveSchema(serveSchema, path)
	}
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return schemainfer.InferDirSchema(path)
	}
	return &api.Topology{Version: api.SchemaVersion}, nil
}
