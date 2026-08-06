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

// TestLadder_SourceLocationGap records, as an executable fact, that the public
// browse path cannot yet locate a symbol in source.
//
// This is the last rung: a reader who has found `Greet` still has to open it.
// Every tool downstream — editor, terminal, LLM context — addresses source as
// "path:line". mache knows the line (ley-line-open's _lsp tables carry it), but
// graph.Open returns nodes whose Origin is nil, so through the public API a
// consumer gets no location at all: not line, not column, not even the byte
// range SourceOrigin is shaped to hold.
//
// Asserted rather than described, so it cannot quietly stay true. When
// positions land (mache-e57065) this test FAILS, and the fix is to replace it
// with the positive assertion in the comment below — a deliberate step, which
// is the point.
func TestLadder_SourceLocationGap(t *testing.T) {
	g := openLadder(t)

	lookuper, ok := g.(graph.DefsLookuper)
	if !ok {
		t.Skip("backend does not implement graph.DefsLookuper")
	}
	defs := lookuper.LookupDef("Greet")
	if len(defs) == 0 {
		t.Skip("no definition to locate")
	}

	node, err := g.GetNode(defs[0])
	if err != nil {
		t.Fatalf("GetNode(%q): %v", defs[0], err)
	}

	if node.Origin != nil {
		t.Fatalf("Origin is now populated (%s bytes [%d,%d)) — the location gap closed. "+
			"Replace this test with the positive assertion: file plus a 1-based line "+
			"a reader can act on (mache-e57065).",
			node.Origin.FilePath, node.Origin.StartByte, node.Origin.EndByte)
	}
	t.Logf("symbol %q is findable but not locatable: Origin is nil through graph.Open (mache-e57065)", defs[0])
}
