package lang

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsTestPath(t *testing.T) {
	for p, want := range map[string]bool{
		"graph/sqlite_graph_test.go":     true,
		"main_test.go":                   true,
		"graph/sqlite_graph.go":          false,
		"cmd/contest.go":                 false,
		"pkg/test_helpers.py":            true,
		"pkg/helpers_test.py":            true,
		"pkg/helpers.py":                 false,
		"web/app.spec.ts":                true,
		"web/app.test.tsx":               true,
		"web/app.ts":                     false,
		"crate/tests/integration.rs":     true,
		"tests/e2e.go":                   true,
		"src/__tests__/a.js":             true,
		"spec/models/user_spec.rb":       true,
		"src/main/java/FooTest.java":     true,
		"src/main/java/Foo.java":         false,
		"internal/testdata/fixture.go":   false,
		"internal/testutil/helpers.go":   false,
		"docs/test.md":                   false,
		"README.md":                      false,
		"weird/x_test.go.orig":           false,
		"internal/lang/lang_test.go.txt": false,
	} {
		assert.Equal(t, want, IsTestPath(p), p)
	}
}
