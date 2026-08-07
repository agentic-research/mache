// Package publicapi_test is the executable proof that mache's public API is
// sufficient on its own.
//
// Every other test in this repo can reach into internal/ — which means none of
// them can tell you whether an EXTERNAL consumer could do the same work. This
// file imports only `mache/build` and `mache/graph`, and internal/lint's
// TestPublicAPIExample_ImportsNothingInternal fails the build if that ever
// stops being true. So if it compiles and passes, the public surface really is
// enough.
//
// What it walks is the whole point of the projection — the browsing ladder:
//
//	segments (directories and files)
//	  -> pieces (constructs: functions, types, methods)
//	    -> symbols (which name is defined where, and who references it)
//
// A rung that needs a hand-written type assertion, an internal import, or a
// re-derived contract is a rung that costs every consumer — human or agent —
// the same rediscovery. That cost is what this asserts against.
package publicapi_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/build"
	"github.com/agentic-research/mache/graph"
)

// corpus writes a small, deterministic Go package and returns its directory.
// Deliberately not a fixture from elsewhere in the repo: an external consumer
// starts from source on disk and nothing else.
func corpus(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("mkdir corpus: %v", err)
	}
	files := map[string]string{
		"greeter.go": "package demo\n\n" +
			"// Greeter renders greetings.\n" +
			"type Greeter struct{ Prefix string }\n\n" +
			"// Greet returns a greeting for name.\n" +
			"func (g *Greeter) Greet(name string) string { return g.Prefix + name }\n",
		"main.go": "package demo\n\n" +
			"func Run() string {\n" +
			"\tg := &Greeter{Prefix: \"hello \"}\n" +
			"\treturn g.Greet(\"world\")\n" +
			"}\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return src
}

// openLadder runs the two public entry points a consumer starts from:
// build.Parse (source tree -> .db) and graph.Open (.db -> queryable graph).
//
// Skips when leyline is unavailable, which is the same degradation a real
// consumer sees — build.Parse resolves the pinned binary and reports why if it
// cannot.
func openLadder(t *testing.T) graph.Graph {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "demo.db")
	if err := build.Parse(corpus(t), dbPath); err != nil {
		t.Skipf("leyline unavailable, skipping public-API ladder: %v", err)
	}
	g, err := graph.Open(dbPath)
	if err != nil {
		t.Fatalf("graph.Open: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })
	return g
}

// TestLadder_SegmentsToPiecesToSymbols is the end-to-end walk. Each rung is
// reached through a NAMED public contract — no anonymous interface literals,
// no internal imports — which is precisely what was impossible before
// mache-9a89cd.
func TestLadder_SegmentsToPiecesToSymbols(t *testing.T) {
	g := openLadder(t)

	// Rung 1 — SEGMENTS. Walk the tree without knowing what is in it.
	roots, err := g.ListChildren("")
	if err != nil {
		t.Fatalf("ListChildren(root): %v", err)
	}
	if len(roots) == 0 {
		t.Fatal("projection has no top-level segments; nothing to browse")
	}
	// The root must not appear among its own children. leyline writes a root
	// node whose id and parent_id are both empty, and the root listing used to
	// match it — so ListChildren("") returned "", and listing THAT returned ""
	// again. A consumer walking the tree recursively never terminated. This is
	// the cheapest possible assertion for "browsing is finite".
	for _, r := range roots {
		if r == "" {
			t.Fatal(`ListChildren("") returned the root as its own child — a recursive walk cannot terminate`)
		}
	}

	// Rung 2 — PIECES. Any listed segment resolves to a node with a kind.
	node, err := g.GetNode(roots[0])
	if err != nil {
		t.Fatalf("GetNode(%q): %v", roots[0], err)
	}
	if node.ID == "" {
		t.Fatalf("node %q came back without an identity", roots[0])
	}

	// Rung 3 — SYMBOLS. The question a reader actually arrives with:
	// "where is Greet defined?" — answered through graph.DefsLookuper, the
	// contract that used to be unexported in package cmd.
	lookuper, ok := g.(graph.DefsLookuper)
	if !ok {
		t.Fatal("graph.Open's result must satisfy graph.DefsLookuper — " +
			"without it a consumer cannot ask where a symbol is defined")
	}
	defs := lookuper.LookupDef("Greet")
	if len(defs) == 0 {
		t.Fatal("LookupDef(Greet) found nothing; the symbol rung is not answering")
	}

	// Rung 3b — the shape-only variant, for when the exact name is unknown.
	searcher, ok := g.(graph.DefsSearcher)
	if !ok {
		t.Fatal("graph.Open's result must satisfy graph.DefsSearcher")
	}
	if matches := searcher.SearchDefs("Gree%", 10); len(matches) == 0 {
		t.Fatal("SearchDefs(Gree%) found nothing; pattern search is not answering")
	}

	// Rung 4 — BACK DOWN. A symbol is only useful if it leads somewhere: the
	// definition must name a real, readable node.
	def := defs[0]
	if _, err := g.GetNode(def); err != nil {
		t.Fatalf("LookupDef returned %q but GetNode cannot resolve it: %v", def, err)
	}
}

// TestLadder_ReachesSourceLocation is the top rung: a reader who has found
// `Greet` must be able to OPEN it.
//
// This replaced TestLadder_SourceLocationGap, which asserted the opposite —
// that Origin was always nil — and failed the moment positions landed. That
// was the point: the gap was recorded as an executable fact so closing it
// forced a deliberate update here rather than leaving a stale comment.
//
// Byte offsets are kept because write-back splices by byte. Lines are added
// because nothing downstream — editor, terminal, LLM context — addresses
// source any other way.
func TestLadder_ReachesSourceLocation(t *testing.T) {
	g := openLadder(t)

	lookuper, ok := g.(graph.DefsLookuper)
	if !ok {
		t.Fatal("graph.Open's result must satisfy graph.DefsLookuper")
	}
	defs := lookuper.LookupDef("Greet")
	if len(defs) == 0 {
		t.Fatal("LookupDef(Greet) found nothing")
	}

	node, err := g.GetNode(defs[0])
	if err != nil {
		t.Fatalf("GetNode(%q): %v", defs[0], err)
	}
	if node.Origin == nil {
		t.Fatalf("node %q has no Origin — a consumer cannot locate it in source", defs[0])
	}

	if node.Origin.FilePath == "" {
		t.Error("Origin carries no file path")
	}
	if node.Origin.EndByte <= node.Origin.StartByte {
		t.Errorf("byte range is empty: [%d,%d)", node.Origin.StartByte, node.Origin.EndByte)
	}
	// 1-based: 0 means "unknown", never "first line". Greet is declared well
	// past the top of the file, so a correct 1-based line cannot be 1 either —
	// which is what catches an off-by-one that a >0 check would wave through.
	if node.Origin.StartLine < 2 {
		t.Errorf("StartLine=%d — expected a 1-based line pointing at Greet's declaration, "+
			"not 0 (unknown) or 1 (an off-by-one from tree-sitter's 0-based rows)",
			node.Origin.StartLine)
	}
	if node.Origin.EndLine < node.Origin.StartLine {
		t.Errorf("EndLine %d precedes StartLine %d", node.Origin.EndLine, node.Origin.StartLine)
	}
	if node.Origin.StartCol == 0 {
		t.Error("StartCol=0 — columns are 1-based, so 0 means unknown")
	}

	// The whole ladder, in the form a reader actually consumes.
	t.Logf("Greet -> %s:%d:%d", node.Origin.FilePath, node.Origin.StartLine, node.Origin.StartCol)
}
