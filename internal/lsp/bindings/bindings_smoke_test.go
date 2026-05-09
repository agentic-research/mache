package bindings_test

import (
	"testing"

	capnp "capnproto.org/go/capnp/v3"
	"github.com/agentic-research/mache/internal/lsp/bindings"
	"github.com/stretchr/testify/require"
)

// TestBindingRecord_RoundTrip pins the generated bindings work end-to-
// end: build a record in memory, read every field back. Catches schema
// drift between LLO's source-of-truth .capnp and mache's vendored copy
// at compile + test time rather than at deserialization time on a real
// .bindings.capnp file. If LLO renames a field or adds a non-default-
// safe ordinal, this test breaks before any consumer code does.
func TestBindingRecord_RoundTrip(t *testing.T) {
	msg, seg, err := capnp.NewMessage(capnp.SingleSegment(nil))
	require.NoError(t, err)

	rec, err := bindings.NewRootBindingRecord(seg)
	require.NoError(t, err)

	require.NoError(t, rec.SetTargetNodeId("auth/methods/Authenticator.Validate"))
	require.NoError(t, rec.SetRefToken("Validate"))
	require.NoError(t, rec.SetConstructNodeId("billing/functions/Charge"))
	require.NoError(t, rec.SetRefSiteNodeId("billing/functions/Charge/.../field_identifier"))
	require.NoError(t, rec.SetRefUri("file:///billing.go"))
	rec.SetParseGen(42)

	rng, err := rec.NewRefRange()
	require.NoError(t, err)
	start, err := rng.NewStart()
	require.NoError(t, err)
	start.SetLine(11)
	start.SetColumn(4)
	start.SetByte(123)
	end, err := rng.NewEnd()
	require.NoError(t, err)
	end.SetLine(11)
	end.SetColumn(12)
	end.SetByte(131)

	// Read back via a fresh decode of the marshaled bytes — proves
	// the wire format works, not just in-memory accessor symmetry.
	wire, err := msg.Marshal()
	require.NoError(t, err)
	dec, err := capnp.Unmarshal(wire)
	require.NoError(t, err)
	got, err := bindings.ReadRootBindingRecord(dec)
	require.NoError(t, err)

	target, err := got.TargetNodeId()
	require.NoError(t, err)
	require.Equal(t, "auth/methods/Authenticator.Validate", target)

	tok, err := got.RefToken()
	require.NoError(t, err)
	require.Equal(t, "Validate", tok)

	construct, err := got.ConstructNodeId()
	require.NoError(t, err)
	require.Equal(t, "billing/functions/Charge", construct)

	refSite, err := got.RefSiteNodeId()
	require.NoError(t, err)
	require.Equal(t, "billing/functions/Charge/.../field_identifier", refSite)

	uri, err := got.RefUri()
	require.NoError(t, err)
	require.Equal(t, "file:///billing.go", uri)

	require.Equal(t, uint64(42), got.ParseGen())

	gotRng, err := got.RefRange()
	require.NoError(t, err)
	gotStart, err := gotRng.Start()
	require.NoError(t, err)
	require.Equal(t, uint32(11), gotStart.Line())
	require.Equal(t, uint32(4), gotStart.Column())
	require.Equal(t, uint64(123), gotStart.Byte())
	gotEnd, err := gotRng.End()
	require.NoError(t, err)
	require.Equal(t, uint32(11), gotEnd.Line())
	require.Equal(t, uint32(12), gotEnd.Column())
	require.Equal(t, uint64(131), gotEnd.Byte())
}
