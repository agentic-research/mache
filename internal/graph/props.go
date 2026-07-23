package graph

import "encoding/json"

// Node Properties are accessed exclusively through this file. Values are JSON:
// a string property is stored as a JSON string (`"go"`, not `go`), so the map
// marshals into a `props` column that json_extract can read (mache-90b89b).
// The previous map[string][]byte base64'd every value and double-encoded the
// one that was already JSON, which made json_extract(record,'$.lang') return
// `Z28=` and put lang/pkg/imports out of reach of every SQL consumer.

// PropString returns a string-valued property, or "" when the node, the map, or
// the key is absent, or the value is not a JSON string.
func PropString(n *Node, key string) string {
	raw := PropRaw(n, key)
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// SetPropString sets a string-valued property, allocating Properties if nil.
func SetPropString(n *Node, key, value string) {
	b, err := json.Marshal(value)
	if err != nil {
		return // coverage:ignore — marshaling a string cannot fail
	}
	SetPropRaw(n, key, b)
}

// PropRaw returns a property's raw JSON bytes.
func PropRaw(n *Node, key string) []byte {
	if n == nil || n.Properties == nil {
		return nil
	}
	return n.Properties[key]
}

// SetPropRaw sets a property's raw JSON bytes, allocating Properties if nil.
// The caller is responsible for raw being valid JSON.
func SetPropRaw(n *Node, key string, raw []byte) {
	if n.Properties == nil {
		n.Properties = make(map[string]json.RawMessage)
	}
	n.Properties[key] = raw
}

// DecodeProps unmarshals a serialized Properties blob as read from SQLite.
// Returns nil when raw is empty or is not a property map — a node carrying
// inline content rather than properties decodes to nil, which is expected.
//
// Every SQL read path goes through this rather than unmarshaling inline, so
// the on-disk encoding stays knowable from this file alone.
func DecodeProps(raw []byte) map[string]json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var p map[string]json.RawMessage
	if err := json.Unmarshal(raw, &p); err != nil || len(p) == 0 {
		return nil
	}
	return p
}
