package ingest_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/lltest"
)

// TestFlattenASTDB_FieldChildKindsOnARealParse pins a fielded child's
// GRAMMAR kind on a real pinned-leyline parse. From projection-v12 the kind
// is read from the child's occurrence (_ast.kind_id -> kinds), because
// node_content.kind is the canonical κ kind for a named node: "variable"
// where the grammar says "short_var_declaration", "type" for "struct_type".
// Fixtures write one value for both, so only a real parse can tell the two
// apart (mache-108e26). The two fields asserted are ones where they differ.
func TestFlattenASTDB_FieldChildKindsOnARealParse(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.go"),
		[]byte("package a\n\ntype T struct{ n int }\n\nfunc F(x int) error {\n\tif y := x; y > 0 {\n\t\treturn nil\n\t}\n\treturn nil\n}\n"), 0o644))
	db, _ := lltest.ParseSourceViaLeyline(t, src)

	recs, err := ingest.FlattenASTDB(db, "", 0)
	require.NoError(t, err)
	byType := map[string]map[string]any{}
	for _, r := range recs {
		m := r.(map[string]any)
		byType[m["type"].(string)] = m
	}
	require.Contains(t, byType, "if_statement")
	require.Contains(t, byType, "type_spec")
	assert.Equal(t, "short_var_declaration", byType["if_statement"]["field_initializer_type"],
		"the grammar kind, not node_content's κ kind (variable)")
	assert.Equal(t, "struct_type", byType["type_spec"]["field_type_type"],
		"the grammar kind, not node_content's κ kind (type)")
	assert.Equal(t, "identifier", byType["function_declaration"]["field_name_type"])
}
