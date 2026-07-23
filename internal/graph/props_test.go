package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

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
