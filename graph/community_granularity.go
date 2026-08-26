package graph

import "strings"

// Granularity selects the cell size community detection runs over.
//
// The filesystem hierarchy is already encoded in every node id —
// `cmd/serve.go/function_declaration_1/block/...` IS fs→AST — so coarser cells
// are a pure prefix operation on the ids mache already has. No re-ingestion,
// no second index.
//
// Why this exists: at construct granularity the projection runs over every AST
// node, so on mache itself the largest communities were things like
// `CHANGELOG.md/section/...` and individual test-function bodies — bounded and
// well-separated (modularity 0.985) but useless for "how should cmd/ be
// split". That question is ABOUT files and directories, so the cells must be
// files or directories.
type Granularity string

const (
	// GranularityConstruct is the raw projection over AST-level node ids.
	GranularityConstruct Granularity = "construct"
	// GranularityFile aggregates every node to its source file.
	GranularityFile Granularity = "file"
	// GranularityDir aggregates every node to its file's directory.
	GranularityDir Granularity = "dir"
)

// AggregateRefs rewrites refs (token → node ids) to the requested granularity
// and applies an optional path-prefix scope.
//
// Scope BEFORE aggregation, on the raw id: scoping to "cmd/" must keep
// cmd/serve.go's nodes whether the caller asked for construct, file, or dir
// cells.
//
// Aggregation DEDUPES within a token: a token referenced 40 times inside one
// file is ONE occurrence of that file, not 40 — otherwise buildProjection
// would inflate pair weights quadratically for chatty files, and the hub
// problem this pipeline just removed at token level would reappear at cell
// level. Order is preserved first-seen so identical input keeps producing an
// identical projection (mache-ff7e31).
//
// A token whose occurrences all collapse into a single cell contributes no
// pairs and is dropped by buildProjection's len<2 skip — correctly: a symbol
// used in only one file says nothing about which files belong together.
func AggregateRefs(refs map[string][]string, g Granularity, scope string) map[string][]string {
	if g == GranularityConstruct || g == "" {
		if scope == "" {
			return refs
		}
	}
	out := make(map[string][]string, len(refs))
	for tok, nodes := range refs {
		var cells []string
		var seen map[string]bool
		for _, id := range nodes {
			if scope != "" && !strings.HasPrefix(id, scope) {
				continue
			}
			cell := id
			switch g {
			case GranularityFile:
				cell = fileOf(id)
			case GranularityDir:
				cell = dirOfFile(fileOf(id))
			}
			if seen == nil {
				seen = make(map[string]bool, len(nodes))
			}
			if !seen[cell] {
				seen[cell] = true
				cells = append(cells, cell)
			}
		}
		if len(cells) > 0 {
			out[tok] = cells
		}
	}
	return out
}

// fileOf trims a node id to its source-file prefix: the id up to and including
// the first path component containing a dot.
//
// Node ids are fs-then-AST by construction (`cmd/serve.go/function_declaration_1`),
// and grammar-node components (`function_declaration_1`, `section`, `block`)
// never contain dots, while source files essentially always do. A dotted
// DIRECTORY above the file (`v1.2/pkg/a.go`) trims early and merely makes the
// cell coarser — a grouping error, not a crash — and an id with no dotted
// component anywhere is returned whole.
func fileOf(id string) string {
	for i, r := range id {
		if r == '.' {
			if j := strings.IndexByte(id[i:], '/'); j >= 0 {
				return id[:i+j]
			}
			return id
		}
	}
	return id
}

// dirOfFile returns the directory of a file cell, or "." for a top-level file
// so root files share one cell instead of each becoming their own.
func dirOfFile(file string) string {
	if i := strings.LastIndexByte(file, '/'); i > 0 {
		return file[:i]
	}
	return "."
}
