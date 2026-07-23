package graph

import "encoding/json"

// Node Properties are accessed exclusively through this file. Keeping the
// encoding behind four functions is what lets the underlying map's value type
// change (mache-90b89b) without touching every consumer.

// PropString returns a string-valued property, or "" when the node, the map,
// or the key is absent.
func PropString(n *Node, key string) string {
	return string(PropRaw(n, key))
}

// SetPropString sets a string-valued property, allocating Properties if nil.
func SetPropString(n *Node, key, value string) {
	SetPropRaw(n, key, []byte(value))
}

// PropRaw returns a property's raw bytes. For object-valued properties such as
// "imports" these bytes are JSON; for the rest they are a plain string.
func PropRaw(n *Node, key string) []byte {
	if n == nil || n.Properties == nil {
		return nil
	}
	return n.Properties[key]
}

// SetPropRaw sets a property's raw bytes, allocating Properties if nil.
func SetPropRaw(n *Node, key string, raw []byte) {
	if n.Properties == nil {
		n.Properties = make(map[string][]byte)
	}
	n.Properties[key] = raw
}

// DecodeProps unmarshals a serialized Properties blob as read from SQLite.
// Returns nil when raw is empty or is not a property map — a node carrying
// inline content rather than properties decodes to nil, which is expected.
//
// Every SQL read path goes through this rather than unmarshaling inline, so
// the on-disk encoding stays knowable from this file alone.
func DecodeProps(raw []byte) map[string][]byte {
	if len(raw) == 0 {
		return nil
	}
	var p map[string][]byte
	if err := json.Unmarshal(raw, &p); err != nil || len(p) == 0 {
		return nil
	}
	return p
}
