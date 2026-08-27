package testutil

import (
	"os"
	"testing"

	capnp "capnproto.org/go/capnp/v3"
	"github.com/agentic-research/ley-line-open/clients/go/leyline-schema/binding"
	"github.com/agentic-research/mache/internal/lsp"
	"github.com/stretchr/testify/require"
)

// WriteBindingLogForTest writes a single-record .bindings.capnp log
// next to dbPath for the test. Mirrors LLO's wire format (back-to-back
// capnp segment messages). Used by tests that need binding-fidelity
// rows in v_refs after mache-6bd4d8 retired the SQL _lsp_refs UNION
// arm.
func WriteBindingLogForTest(t testing.TB, dbPath, target, token, construct, refURI string) {
	t.Helper()
	logPath := lsp.SiblingBindingLogPath(dbPath)
	f, err := os.Create(logPath)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	enc := capnp.NewEncoder(f)
	msg, seg, err := capnp.NewMessage(capnp.SingleSegment(nil))
	require.NoError(t, err)
	rec, err := binding.NewRootBindingRecord(seg)
	require.NoError(t, err)
	require.NoError(t, rec.SetTargetNodeId(target))
	require.NoError(t, rec.SetRefToken(token))
	require.NoError(t, rec.SetConstructNodeId(construct))
	require.NoError(t, rec.SetRefSiteNodeId(""))
	require.NoError(t, rec.SetRefUri(refURI))
	rng, err := rec.NewRefRange()
	require.NoError(t, err)
	_, err = rng.NewStart()
	require.NoError(t, err)
	_, err = rng.NewEnd()
	require.NoError(t, err)
	require.NoError(t, enc.Encode(msg))
}

// BindingRec describes one record for WriteMultiBindingLogForTest.
// Fields use the same vocabulary as the BindingRecord schema; empty
// fields default per the schema-evolution invariant (Qualifier="" is
// the pre-T8.7 default and exercises the COALESCE fallback to token
// in the qualifier-aware fan_out_skew metric).
type BindingRec struct {
	Target, Token, Construct, Qualifier string
}

// WriteMultiBindingLogForTest writes N records to one .bindings.capnp
// log next to dbPath. Used by tests that need the qualifier signal
// across multiple referrers (mache-6c0d07 fan_out_skew), where the
// single-record helper would overwrite each record.
func WriteMultiBindingLogForTest(t testing.TB, dbPath string, recs []BindingRec) {
	t.Helper()
	logPath := lsp.SiblingBindingLogPath(dbPath)
	f, err := os.Create(logPath)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	enc := capnp.NewEncoder(f)
	for _, r := range recs {
		msg, seg, err := capnp.NewMessage(capnp.SingleSegment(nil))
		require.NoError(t, err)
		rec, err := binding.NewRootBindingRecord(seg)
		require.NoError(t, err)
		require.NoError(t, rec.SetTargetNodeId(r.Target))
		require.NoError(t, rec.SetRefToken(r.Token))
		require.NoError(t, rec.SetConstructNodeId(r.Construct))
		require.NoError(t, rec.SetRefSiteNodeId(""))
		require.NoError(t, rec.SetRefUri(""))
		require.NoError(t, rec.SetQualifier(r.Qualifier))
		rng, err := rec.NewRefRange()
		require.NoError(t, err)
		_, err = rng.NewStart()
		require.NoError(t, err)
		_, err = rng.NewEnd()
		require.NoError(t, err)
		require.NoError(t, enc.Encode(msg))
	}
}
