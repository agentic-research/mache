package graph

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPropStringIsStoredAsQuotedJSON(t *testing.T) {
	n := &Node{}
	SetPropString(n, "lang", "go")

	// The stored form must be JSON, not the bare bytes — this is what makes
	// json_extract(props,'$.lang') return `go` instead of `Z28=`.
	assert.Equal(t, `"go"`, string(n.Properties["lang"]),
		"string properties must be stored as JSON strings")
	assert.Equal(t, "go", PropString(n, "lang"), "and must read back unquoted")
}

func TestPropertiesMarshalWithoutBase64(t *testing.T) {
	n := &Node{}
	SetPropString(n, "lang", "go")
	SetPropRaw(n, "imports", []byte(`{"fmt":"fmt"}`))

	out, err := json.Marshal(n.Properties)
	require.NoError(t, err)

	assert.Contains(t, string(out), `"lang":"go"`)
	assert.NotContains(t, string(out), "Z28=", `base64 of "go" must not appear`)
	assert.Contains(t, string(out), `"imports":{"fmt":"fmt"}`,
		"imports must nest as a real object, not a base64 string")
}

func TestPropStringOnNonStringValueReturnsEmpty(t *testing.T) {
	n := &Node{}
	SetPropRaw(n, "imports", []byte(`{"fmt":"fmt"}`))
	assert.Equal(t, "", PropString(n, "imports"),
		"an object-valued property is not a string property")
}

func TestDecodeRoundTripsThroughMarshal(t *testing.T) {
	n := &Node{}
	SetPropString(n, "lang", "go")
	SetPropRaw(n, "imports", []byte(`{"fmt":"fmt"}`))

	blob, err := json.Marshal(n.Properties)
	require.NoError(t, err)

	back := &Node{Properties: DecodeProps(blob)}
	assert.Equal(t, "go", PropString(back, "lang"))
	assert.JSONEq(t, `{"fmt":"fmt"}`, string(PropRaw(back, "imports")))
}

func TestDecodePropsRejectsNonPropertyBlobs(t *testing.T) {
	assert.Nil(t, DecodeProps(nil))
	assert.Nil(t, DecodeProps([]byte("")))
	assert.Nil(t, DecodeProps([]byte("{}")), "empty map decodes to nil, not an empty map")
	assert.Nil(t, DecodeProps([]byte("not json")))
	assert.Nil(t, DecodeProps([]byte(`"a rendered file body"`)),
		"a node carrying inline content must not decode as properties")
}

func TestPropStringRoundTrip(t *testing.T) {
	n := &Node{ID: "pkg/functions/Foo"}
	SetPropString(n, "lang", "go")
	assert.Equal(t, "go", PropString(n, "lang"))
}

func TestPropStringAbsentAndNilSafe(t *testing.T) {
	assert.Equal(t, "", PropString(nil, "lang"), "nil node must not panic")
	assert.Equal(t, "", PropString(&Node{}, "lang"), "nil Properties map must not panic")

	n := &Node{}
	SetPropString(n, "pkg", "main")
	assert.Equal(t, "", PropString(n, "lang"), "absent key returns empty")
}

func TestPropRawPreservesObjectJSON(t *testing.T) {
	n := &Node{}
	SetPropRaw(n, "imports", []byte(`{"fmt":"fmt"}`))
	assert.JSONEq(t, `{"fmt":"fmt"}`, string(PropRaw(n, "imports")),
		"object-valued properties must survive byte-for-byte as JSON")
}
