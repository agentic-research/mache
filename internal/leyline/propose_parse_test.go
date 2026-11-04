package leyline

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	daemonwire "github.com/agentic-research/ley-line-open/clients/go/leyline-schema/daemon/wire"
)

// parseProposeLine runs one daemon answer, as JSON, through the parse Propose
// applies to it.
func parseProposeLine(t *testing.T, line string) (*ProposeResult, error) {
	t.Helper()
	var resp daemonwire.ProposeResponse
	require.NoError(t, json.Unmarshal([]byte(line), &resp))
	return proposeResult(&resp)
}

// TestProposeResult parses each outcome once into values, and refuses an
// answer missing a field its outcome needs rather than handing callers a zero
// value: a refusal must have a reason, a conflict its lines, an applied blob
// its path and hash. A reason mache does not name is still a refusal.
func TestProposeResult(t *testing.T) {
	for _, tc := range []struct {
		name, line, wantErr string
		want                *ProposeResult
	}{
		{
			name: "applied",
			line: `{"ok":true,"tree_root":"r","blobs":[{"path":"a.go","blob_hash":"h"}],"merged":["a.go"]}`,
			want: &ProposeResult{TreeRoot: "r", Blobs: []PathHash{{Path: "a.go", BlobHash: "h"}}, Merged: []string{"a.go"}},
		},
		{
			name: "stale, with conflicts",
			line: `{"ok":false,"error":"moved","refusal":{"reason":"stale","moved":[{"path":"a.go","expected":"e","current":"c","conflicts":[{"start":4,"end":5}]}]}}`,
			want: &ProposeResult{Message: "moved", Refusal: &ProposalRefusal{
				Reason: daemonwire.RefusalStale,
				Moved:  []MovedPath{{Path: "a.go", Expected: "e", Current: "c", Conflicts: []LineRange{{Start: 4, End: 5}}}},
			}},
		},
		{
			name: "a reason mache does not name",
			line: `{"ok":false,"error":"no","refusal":{"reason":"from_the_future","path":"a.go","other":"b.go"}}`,
			want: &ProposeResult{Message: "no", Refusal: &ProposalRefusal{Reason: "from_the_future", Path: "a.go", Other: "b.go"}},
		},
		{name: "not ok, no refusal", line: `{"ok":false,"error":"boom"}`, wantErr: "boom"},
		{name: "refusal without a reason", line: `{"ok":false,"refusal":{"path":"a.go"}}`, wantErr: "without a reason"},
		{name: "conflict without its lines", line: `{"ok":false,"refusal":{"reason":"stale","moved":[{"path":"a.go","conflicts":[{"start":4}]}]}}`, wantErr: "without its lines"},
		{name: "blob without its hash", line: `{"ok":true,"blobs":[{"path":"a.go"}]}`, wantErr: "without its path or hash"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseProposeLine(t, tc.line)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
