package graph

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestParseGoImports pins the behavior of the deprecated regex import parser.
//
// It is NOT dead code: Properties["imports"] is only set when the ingest engine
// has structured imports (`if fileImports != nil`), and nodes hydrated from a
// .db via nodes_table_reader carry Context but no imports property — so this is
// the live path for .db-backed graphs. It parses a possibly-PARTIAL context
// blob, which is why it stays lenient text matching rather than go/parser
// (a strict parse of a fragment without a package clause fails outright).
//
// These cases exist so the eventual removal — persisting structured imports on
// the nodes-table path — can prove equivalence before deleting.
func TestParseGoImports(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  string
		want map[string]string
	}{
		{
			"single unaliased import — alias is last path segment",
			`import "fmt"`,
			map[string]string{"fmt": "fmt"},
		},
		{
			"single aliased import",
			`import f "fmt"`,
			map[string]string{"f": "fmt"},
		},
		{
			"unaliased nested path uses last segment",
			`import "net/http"`,
			map[string]string{"http": "net/http"},
		},
		{
			"grouped imports",
			"import (\n\t\"fmt\"\n\t\"os\"\n)",
			map[string]string{"fmt": "fmt", "os": "os"},
		},
		{
			"grouped with alias",
			"import (\n\tf \"fmt\"\n\t\"net/http\"\n)",
			map[string]string{"f": "fmt", "http": "net/http"},
		},
		{
			// KNOWN QUIRK (characterized, not endorsed): blank imports are
			// skipped correctly, but a DOT import is not. addGoImport skips
			// alias "." — however memberImportRe's `(\w+)?` cannot capture
			// ".", so the alias arrives empty and the import degrades into a
			// regular one keyed by its last path segment ("os"). A real parser
			// would classify it. Fixing this is part of removing the regex
			// entirely (persist structured imports on the .db path).
			"blank imports skipped; dot import degrades to a normal import",
			"import (\n\t_ \"embed\"\n\t. \"os\"\n\t\"fmt\"\n)",
			map[string]string{"fmt": "fmt", "os": "os"},
		},
		{"no imports", "package foo\n\nvar x = 1", map[string]string{}},
		{"empty context", "", map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseGoImports([]byte(tc.ctx))
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseGoImports(%q) = %v, want %v", tc.ctx, got, tc.want)
			}
		})
	}
}

// TestLoadImports_PrefersStructuredOverRegex verifies the structured path wins:
// when Properties["imports"] is present it is used verbatim and the Context
// text is never consulted. This is the invariant that makes the regex fallback
// removable once the .db path persists imports.
func TestLoadImports_PrefersStructuredOverRegex(t *testing.T) {
	structured, err := json.Marshal(map[string]string{"alias": "real/path"})
	if err != nil {
		t.Fatal(err)
	}
	node := &Node{
		Properties: map[string][]byte{"imports": structured},
		// Deliberately conflicting context — must be ignored.
		Context: []byte(`import "should/not/be/used"`),
	}
	got := loadImports(node)
	want := map[string]string{"alias": "real/path"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("loadImports = %v, want %v (structured Properties must win over Context)", got, want)
	}
}

// TestLoadImports_FallsBackToContext covers the live .db-hydration path:
// no Properties["imports"], so Context is parsed.
func TestLoadImports_FallsBackToContext(t *testing.T) {
	node := &Node{Context: []byte(`import "net/http"`)}
	got := loadImports(node)
	want := map[string]string{"http": "net/http"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("loadImports = %v, want %v", got, want)
	}
}
