package cmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRenderFindings_JSONEmptyIsArrayNotNull asserts the json format serializes
// an empty finding set as `[]`, not `null`. A nil []smellFinding otherwise
// marshals to `null`, which forces consumers to special-case it (and diverges
// from --format sarif, which always emits arrays). Zero findings is the common
// case on a clean gate, so the shape must be a stable array.
func TestRenderFindings_JSONEmptyIsArrayNotNull(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, renderFindings(&buf, "dead_code", nil, "json", 200))

	assert.Contains(t, buf.String(), `"findings": []`, "empty findings must render as [] not null")

	var resp struct {
		Rule     string         `json:"rule"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &resp))
	assert.NotNil(t, resp.Findings, "findings must unmarshal to a non-nil (empty) slice")
	assert.Empty(t, resp.Findings)
}
