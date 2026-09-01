package mcpserve

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/leylinegraph"
	machetmpl "github.com/agentic-research/mache/internal/template"
	"github.com/agentic-research/mache/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLeylineProjection_TerraformAddressRefsReachFindCallers protects the
// producer/consumer seam that v0.18.2 repairs. The pre-v0.18.2 Leyline HCL
// dispatcher emitted a complete _ast but never ran its registered address-ref
// extractor, leaving node_refs empty and making Mache's caller view lie by
// omission.
func TestLeylineProjection_TerraformAddressRefsReachFindCallers(t *testing.T) {
	testutil.RequirePinnedLeyline(t)

	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "main.tf"), []byte(`
variable "bucket_name" {
  type = string
}

variable "region" {
  default = "us-east-1"
}

module "logging" {
  source = "./modules/logging"
}

resource "aws_s3_bucket" "main" {
  bucket = var.bucket_name
}
`), 0o644))

	dbPath, cleanup, err := leylinegraph.AutoInvokeLeylineParse(src)
	require.NoError(t, err)
	defer cleanup()

	sg, err := graph.OpenSQLiteGraph(dbPath, &api.Topology{Version: api.SchemaVersion}, machetmpl.Render)
	require.NoError(t, err)
	defer func() { require.NoError(t, sg.Close()) }()

	rows, err := sg.QueryRefs("SELECT token FROM node_refs ORDER BY token")
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()

	var tokens []string
	for rows.Next() {
		var token string
		require.NoError(t, rows.Scan(&token))
		tokens = append(tokens, token)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"env:bucket_name", "env:region", "mod:./modules/logging"}, tokens,
		"the released Leyline artifact must serialize the registered Terraform address refs")

	handler := makeFindCallersHandler(sg)
	for _, token := range tokens {
		result, err := handler(context.Background(), testutil.MakeRequest(map[string]any{"token": token}))
		require.NoError(t, err)
		require.False(t, result.IsError, testutil.ResultText(t, result))

		var callers []string
		require.NoError(t, json.Unmarshal([]byte(testutil.ResultText(t, result)), &callers))
		sort.Strings(callers)
		assert.NotEmptyf(t, callers, "find_callers must expose the serialized %s ref", token)
	}
}
