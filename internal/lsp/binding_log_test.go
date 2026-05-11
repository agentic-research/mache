package lsp_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	capnp "capnproto.org/go/capnp/v3"
	"github.com/agentic-research/ley-line-open/clients/go/leyline-schema/binding"
	"github.com/agentic-research/mache/internal/lsp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeBindingLog writes records to path using the same wire format LLO
// produces — back-to-back capnp segment messages. Pinning the producer
// shape here ensures the reader test is decoupled from any specific
// LLO version: if the wire format ever drifts, this helper drifts with
// the producer (or the test breaks immediately, which is also fine).
func writeBindingLog(t *testing.T, path string, recs []lsp.Binding) {
	t.Helper()
	f, err := os.Create(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	enc := capnp.NewEncoder(f)
	for _, r := range recs {
		msg, seg, err := capnp.NewMessage(capnp.SingleSegment(nil))
		require.NoError(t, err)
		rec, err := binding.NewRootBindingRecord(seg)
		require.NoError(t, err)
		require.NoError(t, rec.SetTargetNodeId(r.TargetNodeID))
		require.NoError(t, rec.SetRefToken(r.RefToken))
		require.NoError(t, rec.SetConstructNodeId(r.ConstructNodeID))
		require.NoError(t, rec.SetRefSiteNodeId(r.RefSiteNodeID))
		require.NoError(t, rec.SetRefUri(r.RefURI))
		rec.SetParseGen(r.ParseGen)
		rng, err := rec.NewRefRange()
		require.NoError(t, err)
		start, err := rng.NewStart()
		require.NoError(t, err)
		start.SetLine(r.RefRange.StartLine)
		start.SetColumn(r.RefRange.StartColumn)
		start.SetByte(r.RefRange.StartByte)
		end, err := rng.NewEnd()
		require.NoError(t, err)
		end.SetLine(r.RefRange.EndLine)
		end.SetColumn(r.RefRange.EndColumn)
		end.SetByte(r.RefRange.EndByte)
		require.NoError(t, enc.Encode(msg))
	}
}

func TestSiblingBindingLogPath(t *testing.T) {
	// Pinning LLO's Path::with_extension semantics
	// (cli-lib/src/cmd_lsp.rs, daemon/lsp_pass.rs): the trailing
	// `.db` (or whatever extension) is REPLACED, not appended. A
	// producer-side change must propagate here in lockstep.
	cases := []struct{ in, want string }{
		{"/tmp/foo.db", "/tmp/foo.bindings.capnp"},
		{"/tmp/no-ext", "/tmp/no-ext.bindings.capnp"},
		{"/tmp/multi.dot.db", "/tmp/multi.dot.bindings.capnp"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, lsp.SiblingBindingLogPath(c.in), "input: %s", c.in)
	}
}

func TestReadBindingLog_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db.bindings.capnp")

	want := []lsp.Binding{
		{
			TargetNodeID:    "auth/methods/Authenticator.Validate",
			RefToken:        "Validate",
			ConstructNodeID: "billing/functions/Charge",
			RefSiteNodeID:   "billing/functions/Charge/.../field_identifier",
			RefURI:          "file:///billing.go",
			ParseGen:        42,
			RefRange: lsp.Range{
				StartLine: 11, StartColumn: 4, StartByte: 123,
				EndLine: 11, EndColumn: 12, EndByte: 131,
			},
		},
		{
			// Second record exercises the documented "no enclosing
			// construct" state — empty ConstructNodeID is the canonical
			// signal for a top-level reference (e.g. cross-repo).
			TargetNodeID:    "stdlib/io/Reader",
			RefToken:        "Reader",
			ConstructNodeID: "",
			RefSiteNodeID:   "pkg/types.go/.../type_identifier",
			RefURI:          "file:///types.go",
			ParseGen:        42,
			RefRange: lsp.Range{
				StartLine: 3, StartColumn: 8, StartByte: 50,
				EndLine: 3, EndColumn: 14, EndByte: 56,
			},
		},
	}
	writeBindingLog(t, path, want)

	got, err := lsp.ReadBindingLog(path)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestReadBindingLog_EmptyFileReturnsEmptySlice(t *testing.T) {
	// LLO writes a zero-byte .bindings.capnp when the LSP pass found
	// no references. The reader must accept that as a valid (empty)
	// log, not an error — a missing log and an empty log are different
	// states with different debug semantics.
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.db.bindings.capnp")
	require.NoError(t, os.WriteFile(path, nil, 0o644))

	got, err := lsp.ReadBindingLog(path)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestReadBindingLog_MissingFileWrapsErrNotExist(t *testing.T) {
	// Callers (canonical-view migration) need to distinguish
	// "log not produced yet — fall back to SQL" from "log corrupt".
	// errors.Is(err, fs.ErrNotExist) is the load-bearing predicate;
	// pin it.
	dir := t.TempDir()
	_, err := lsp.ReadBindingLog(filepath.Join(dir, "does-not-exist.bindings.capnp"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, os.ErrNotExist),
		"missing file must wrap os.ErrNotExist; got %v", err)
}

func TestReadBindingLog_TruncatedPayloadReportsRecordIndex(t *testing.T) {
	// If a producer crashed mid-write (or the file is corrupt),
	// the error must surface WHERE the read failed so the operator
	// can decide whether to keep the partial set or rebuild. The
	// per-record index is the only signal that lets you triage
	// "first record bad" vs "tail of a 30k-record log".
	dir := t.TempDir()
	full := filepath.Join(dir, "full.db.bindings.capnp")
	writeBindingLog(t, full, []lsp.Binding{
		{TargetNodeID: "a", RefToken: "x", RefURI: "file:///a"},
		{TargetNodeID: "b", RefToken: "y", RefURI: "file:///b"},
	})

	bytesAll, err := os.ReadFile(full)
	require.NoError(t, err)
	require.Greater(t, len(bytesAll), 16, "test fixture too small to truncate meaningfully")

	truncated := filepath.Join(dir, "truncated.db.bindings.capnp")
	require.NoError(t, os.WriteFile(truncated, bytesAll[:len(bytesAll)-8], 0o644))

	_, err = lsp.ReadBindingLog(truncated)
	require.Error(t, err)
	// We don't pin the exact wrapped error (that's capnp's affair —
	// could be ErrUnexpectedEOF or a segment-size-mismatch). What we
	// DO pin is that the error surfaces the path and the records-read
	// count so an operator running with --verbose can see where it
	// died.
	assert.Contains(t, err.Error(), truncated)
	assert.Contains(t, err.Error(), "after ", "record-index context missing from truncation error")
}

func TestIterateBindingLog_StreamsAndShortCircuits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "iter.db.bindings.capnp")
	want := []lsp.Binding{
		{TargetNodeID: "a", RefToken: "x", RefURI: "file:///a"},
		{TargetNodeID: "b", RefToken: "y", RefURI: "file:///b"},
		{TargetNodeID: "c", RefToken: "z", RefURI: "file:///c"},
	}
	writeBindingLog(t, path, want)

	// Streaming all the way through.
	var seen []lsp.Binding
	require.NoError(t, lsp.IterateBindingLog(path, func(b lsp.Binding) error {
		seen = append(seen, b)
		return nil
	}))
	assert.Equal(t, want, seen)

	// Short-circuit: stop after the second record. Iteration must
	// surface the user error verbatim (errors.Is roundtrip) so
	// callers can detect "I asked you to stop" vs decode failures.
	stop := errors.New("found what I needed")
	var partial []lsp.Binding
	gotErr := lsp.IterateBindingLog(path, func(b lsp.Binding) error {
		partial = append(partial, b)
		if len(partial) == 2 {
			return stop
		}
		return nil
	})
	require.True(t, errors.Is(gotErr, stop), "user error must round-trip via errors.Is; got %v", gotErr)
	assert.Equal(t, want[:2], partial, "iteration continued past short-circuit signal")
}

// TestReadBindingLog_RealLLOOutput exercises the reader against an
// actual file produced by `leyline lsp` rather than the synthetic
// in-test encoder. Catches the class of bug where mache's encoder
// happens to use the same wire format as the test reader but a real
// LLO build emits something subtly different.
//
// Skipped when MACHE_FALSIFIABILITY_INTEGRATION isn't set or leyline
// or gopls isn't on PATH — same gate as the falsifiability tests so
// CI-default `go test ./...` doesn't pay the gopls warmup tax.
func TestReadBindingLog_RealLLOOutput(t *testing.T) {
	if os.Getenv("MACHE_FALSIFIABILITY_INTEGRATION") != "1" {
		t.Skip("set MACHE_FALSIFIABILITY_INTEGRATION=1 to run; needs leyline + gopls")
	}
	if _, err := exec.LookPath("leyline"); err != nil {
		t.Skip("leyline not on PATH")
	}
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH")
	}

	srcDir, err := os.Getwd()
	require.NoError(t, err)
	// Walk up from internal/lsp/ to repo root.
	repoRoot := filepath.Dir(filepath.Dir(srcDir))
	enrichTarget := filepath.Join(repoRoot, "validate", "validate.go")
	if _, statErr := os.Stat(enrichTarget); statErr != nil {
		t.Skipf("expected fixture %s missing", enrichTarget)
	}

	dir := t.TempDir()
	parsedDB := filepath.Join(dir, "self.db")
	enrichedDB := filepath.Join(dir, "self-enriched.db")

	cmd := exec.Command("leyline", "parse", repoRoot, "-o", parsedDB, "--lang", "go")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "leyline parse failed: %s", out)

	cmd = exec.Command("leyline", "lsp",
		"--server", "gopls",
		"--input", enrichTarget,
		"--output", enrichedDB,
		"--merge-db", parsedDB)
	out, err = cmd.CombinedOutput()
	require.NoError(t, err, "leyline lsp failed: %s", out)

	logPath := lsp.SiblingBindingLogPath(enrichedDB)
	_, statErr := os.Stat(logPath)
	require.NoError(t, statErr,
		"LLO did not produce expected sibling log at %s — convention drift?", logPath)

	got, err := lsp.ReadBindingLog(logPath)
	require.NoError(t, err)
	require.NotEmpty(t, got, "LLO produced empty .bindings.capnp despite gopls reporting refs")

	// Pin the documented schema invariants on real producer output.
	// If LLO ever emits a record violating these, the consumer side's
	// downstream JOIN logic breaks, so catch it at the boundary.
	for i, b := range got {
		assert.NotEmpty(t, b.RefToken, "record %d: empty refToken (LLO should skip these per producer logic)", i)
		assert.NotEmpty(t, b.RefURI, "record %d: empty refUri", i)
		assert.True(t, b.RefRange.EndByte >= b.RefRange.StartByte,
			"record %d: range end-byte before start-byte: %+v", i, b.RefRange)
	}
	t.Logf("ReadBindingLog: %d records from %s", len(got), logPath)
}
