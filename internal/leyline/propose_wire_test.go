package leyline_test

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	daemonwire "github.com/agentic-research/ley-line-open/clients/go/leyline-schema/daemon/wire"

	"github.com/agentic-research/mache/internal/leyline"
	"github.com/agentic-research/mache/internal/lltest"
)

const (
	proposeBase  = "package pkg\n\nfunc A() int { return B() }\n\nfunc B() int { return 1 }\n"
	proposeTheir = "package pkg\n\nfunc A() int { return B() }\n\nfunc B() int { return 2 }\n"
	proposeOurs  = "package pkg\n\nfunc A() int { return B() + 1 }\n\nfunc B() int { return 1 }\n"
	proposeClash = "package pkg\n\nfunc A() int { return B() }\n\nfunc B() int { return 3 }\n"
	proposeAll   = "package pkg\n\nfunc A() int { return B() + 1 }\n\nfunc B() int { return 2 }\n"
)

// text is a proposed write's content, which the wire carries as a pointer.
func text(s string) *string { return &s }

// requireHash asserts s is a blob hash as the wire spells it: 32 bytes, hex.
func requireHash(t *testing.T, s string) {
	t.Helper()
	b, err := hex.DecodeString(s)
	require.NoError(t, err, "%q is not hex", s)
	require.Len(t, b, 32, "%q is not a 32-byte hash", s)
}

// TestProposeWire_AgainstThePinnedDaemon checks the read_source and propose
// mirrors (propose.go) against the pinned binary, end to end: an applied write
// lands in the source dir, a stale base whose change does not overlap is
// merged and named, an overlapping one is refused stale with the conflicting
// base lines, a malformed base is refused with that reason, a create under a
// file is refused path_conflict naming that file in Other, and reading a file
// the store does not hold is an error (mache-3a0fb9).
func TestProposeWire_AgainstThePinnedDaemon(t *testing.T) {
	src := t.TempDir()
	file := filepath.Join(src, "pkg", "a.go")
	require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o755))
	require.NoError(t, os.WriteFile(file, []byte(proposeBase), 0o644))
	c, err := leyline.DialSocket(lltest.StartPinnedSourceDaemon(t, src))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	read, err := c.ReadSource("pkg/a.go")
	require.NoError(t, err)
	assert.Equal(t, proposeBase, read.Content)
	assert.Equal(t, "100644", read.Mode)
	requireHash(t, read.BlobHash)
	requireHash(t, read.StoreRoot)
	base := read.BlobHash

	applied, err := c.Propose([]leyline.ProposedWrite{{Path: "pkg/a.go", Base: base, Content: text(proposeTheir)}})
	require.NoError(t, err)
	require.Nil(t, applied.Refusal, applied.Message)
	require.Len(t, applied.Blobs, 1)
	assert.Equal(t, "pkg/a.go", applied.Blobs[0].Path)
	requireHash(t, applied.Blobs[0].BlobHash)
	assert.Empty(t, applied.Merged, "the base had not moved")
	onDisk, err := os.ReadFile(file)
	require.NoError(t, err)
	assert.Equal(t, proposeTheir, string(onDisk), "the daemon writes an accepted proposal into its source dir")

	merged, err := c.Propose([]leyline.ProposedWrite{{Path: "pkg/a.go", Base: base, Content: text(proposeOurs)}})
	require.NoError(t, err)
	require.Nil(t, merged.Refusal, merged.Message)
	assert.Equal(t, []string{"pkg/a.go"}, merged.Merged)
	now, err := c.ReadSource("pkg/a.go")
	require.NoError(t, err)
	assert.Equal(t, proposeAll, now.Content, "a merge keeps both writers' changes")
	assert.Equal(t, merged.Blobs[0].BlobHash, now.BlobHash)

	stale, err := c.Propose([]leyline.ProposedWrite{{Path: "pkg/a.go", Base: base, Content: text(proposeClash)}})
	require.NoError(t, err, "a refusal is a result, not an error")
	require.NotNil(t, stale.Refusal)
	assert.Equal(t, daemonwire.RefusalStale, stale.Refusal.Reason)
	require.Len(t, stale.Refusal.Moved, 1)
	moved := stale.Refusal.Moved[0]
	assert.Equal(t, "pkg/a.go", moved.Path)
	assert.Equal(t, base, moved.Expected)
	assert.Equal(t, now.BlobHash, moved.Current)
	assert.Equal(t, []leyline.LineRange{{Start: 4, End: 5}}, moved.Conflicts, "0-based base lines: line 5, func B")
	assert.NotEmpty(t, stale.Message)

	bad, err := c.Propose([]leyline.ProposedWrite{{Path: "pkg/a.go", Base: "not-a-hash", Content: text(proposeClash)}})
	require.NoError(t, err)
	require.NotNil(t, bad.Refusal)
	assert.Equal(t, daemonwire.RefusalMalformedBase, bad.Refusal.Reason)

	under, err := c.Propose([]leyline.ProposedWrite{{Path: "pkg/a.go/b.go", Base: daemonwire.BaseAbsent, Kind: daemonwire.WriteKindCreate, Content: text(proposeBase)}})
	require.NoError(t, err)
	require.NotNil(t, under.Refusal, "a file cannot be created under a file")
	assert.Equal(t, daemonwire.RefusalPathConflict, under.Refusal.Reason)
	assert.Equal(t, "pkg/a.go/b.go", under.Refusal.Path)
	assert.Equal(t, "pkg/a.go", under.Refusal.Other, "the refusal names the file in the way")

	_, err = c.ReadSource("pkg/missing.go")
	require.Error(t, err)
}

// TestPropose_NotOkWithoutARefusalIsAnError: a not-ok answer that carries no
// typed refusal is not a verdict mache can act on, so it is an error, never a
// silently empty result.
func TestPropose_NotOkWithoutARefusalIsAnError(t *testing.T) {
	sock := lltest.FakeDaemon(t, func(map[string]any) any {
		return map[string]any{"ok": false, "error": "propose: the write to 'a.go' has no `content`"}
	})
	c, err := leyline.DialSocket(sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	_, err = c.Propose([]leyline.ProposedWrite{{Path: "a.go", Base: daemonwire.BaseAny}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no `content`")
}

// TestProposedWrite_SendsTheSchemaSpelling: the request is spelled as LLO's
// ProposedWrite, with unset kind and mode left off (an unset kind is replace).
func TestProposedWrite_SendsTheSchemaSpelling(t *testing.T) {
	var got map[string]any
	sock := lltest.FakeDaemon(t, func(req map[string]any) any {
		got = req
		return map[string]any{"ok": true, "store_root": "t", "blobs": []any{}}
	})
	c, err := leyline.DialSocket(sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	_, err = c.Propose([]leyline.ProposedWrite{
		{Path: "a.go", Base: "ab", Content: text("x")},
		{Path: "d", Base: daemonwire.BaseAbsent, Kind: daemonwire.WriteKindMkdir},
	})
	require.NoError(t, err)
	assert.Equal(t, "propose", got["op"])
	assert.Equal(t, []any{
		map[string]any{"path": "a.go", "base": "ab", "content": "x"},
		map[string]any{"path": "d", "base": "absent", "kind": "mkdir"},
	}, got["writes"])
}

// TestProposeWire_AliasesExistingOnAFoldingCheckout: where the checkout's file
// system folds case, creating "A.go" beside "a.go" would write one file under
// two names, and the pinned daemon refuses it aliases_existing, naming the
// entry it aliases in Other. Linux runners do not fold; macOS ones do, so the
// case runs there.
func TestProposeWire_AliasesExistingOnAFoldingCheckout(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.go"), []byte(proposeBase), 0o644))
	if _, err := os.Stat(filepath.Join(src, "A.go")); err != nil {
		t.Skipf("%s does not fold case, so nothing aliases", src)
	}
	c, err := leyline.DialSocket(lltest.StartPinnedSourceDaemon(t, src))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	res, err := c.Propose([]leyline.ProposedWrite{{Path: "A.go", Base: daemonwire.BaseAbsent, Kind: daemonwire.WriteKindCreate, Content: text(proposeTheir)}})
	require.NoError(t, err)
	require.NotNil(t, res.Refusal, "A.go and a.go are one file here")
	assert.Equal(t, daemonwire.RefusalAliasesExisting, res.Refusal.Reason)
	assert.Equal(t, "A.go", res.Refusal.Path)
	assert.Equal(t, "a.go", res.Refusal.Other)
	onDisk, err := os.ReadFile(filepath.Join(src, "a.go"))
	require.NoError(t, err)
	assert.Equal(t, proposeBase, string(onDisk), "a refused proposal writes nothing")
}
