package ingest

import sitter "github.com/smacker/go-tree-sitter"

// parentAwareMatch wraps a Match and injects a _parent key into Values()
// containing the parent match's values. This allows child templates to
// reference parent fields via {{._parent.fieldName}}.
//
// All other interfaces (OriginProvider, GetCaptureNode) are forwarded to the
// inner match so that tree-sitter features (doc comments, location, write-back)
// continue to work.
type parentAwareMatch struct {
	inner        Match
	parentValues map[string]any
	cached       map[string]any // built once on first Values() call
}

func (m *parentAwareMatch) Values() map[string]any {
	if m.cached != nil {
		return m.cached
	}
	inner := m.inner.Values()
	v := make(map[string]any, len(inner)+1)
	for k, val := range inner {
		v[k] = val
	}
	v["_parent"] = m.parentValues
	m.cached = v
	return v
}

func (m *parentAwareMatch) Context() any {
	return m.inner.Context()
}

// CaptureOrigin forwards to the inner match if it implements OriginProvider.
// Required for write-back byte-range tracking.
func (m *parentAwareMatch) CaptureOrigin(name string) (startByte, endByte uint32, ok bool) {
	if op, is := m.inner.(OriginProvider); is {
		return op.CaptureOrigin(name)
	}
	return 0, 0, false
}

// GetCaptureNode forwards to the inner match if it supports tree-sitter capture lookup.
// Required for doc comment extraction and location metadata.
func (m *parentAwareMatch) GetCaptureNode(name string) *sitter.Node {
	type captureNodeProvider interface {
		GetCaptureNode(string) *sitter.Node
	}
	if cn, ok := m.inner.(captureNodeProvider); ok {
		return cn.GetCaptureNode(name)
	}
	return nil
}
