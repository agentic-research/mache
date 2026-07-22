package graph

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestLoadImports_UsesStructuredProperties covers the only supported import
// source: structured data set at ingest and round-tripped through the
// nodes-table `record` column.
func TestLoadImports_UsesStructuredProperties(t *testing.T) {
	structured, err := json.Marshal(map[string]string{"alias": "real/path", "http": "net/http"})
	if err != nil {
		t.Fatal(err)
	}
	node := &Node{Properties: map[string][]byte{"imports": structured}}

	got := loadImports(node)
	want := map[string]string{"alias": "real/path", "http": "net/http"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("loadImports = %v, want %v", got, want)
	}
}

// TestLoadImports_ContextIsNotParsed pins the deliberate behavior change from
// mache-f930b6: there is no text-scraping fallback any more.
//
// A regex import parser used to run over node.Context here "for backward
// compatibility". It was heuristical matching of Go source text and it
// mis-classified dot imports — `. "os"` degraded into a normal `os` import,
// because the alias group `(\w+)?` cannot match ".". Every path that reached it
// is now covered structurally:
//
//   - MemoryStore (fresh ingest): the engine sets Properties directly.
//   - .db built by SQLiteWriter: Properties round-trip via `record`
//     (NodesTableReader restores them — nodes_table_properties_test.go).
//   - .db built by leyline: has no `context` column at all, so the fallback
//     could never have fired there.
//
// Context alone must therefore yield no imports rather than a guess.
func TestLoadImports_ContextIsNotParsed(t *testing.T) {
	node := &Node{Context: []byte("import (\n\t. \"os\"\n\t\"net/http\"\n)")}
	if got := loadImports(node); got != nil {
		t.Errorf("loadImports = %v, want nil — context text must not be scraped for imports", got)
	}
}

// TestLoadImports_MalformedPropertiesYieldNil ensures a corrupt or
// wrong-shaped imports blob degrades to "no imports" instead of panicking or
// returning half-parsed data.
func TestLoadImports_MalformedPropertiesYieldNil(t *testing.T) {
	for name, raw := range map[string][]byte{
		"not json":     []byte("{not json"),
		"wrong shape":  []byte(`["a","b"]`),
		"empty object": []byte(`{}`),
		"empty bytes":  {},
	} {
		t.Run(name, func(t *testing.T) {
			node := &Node{Properties: map[string][]byte{"imports": raw}}
			if got := loadImports(node); got != nil {
				t.Errorf("loadImports = %v, want nil", got)
			}
		})
	}
}

// TestLoadImports_NoPropertiesYieldsNil covers the bare node case.
func TestLoadImports_NoPropertiesYieldsNil(t *testing.T) {
	if got := loadImports(&Node{}); got != nil {
		t.Errorf("loadImports = %v, want nil", got)
	}
}
